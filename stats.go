package main

import (
	"context"
	"encoding/binary"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OwlShack/meshcore-go/hardware"
)

type RadioInfo struct {
	FreqHz  uint32
	BwHz    uint32
	SF      uint8
	CR      uint8
	TxPower uint8
}

// airtimeMs estimates time-on-air for a packet. The firmware's "time=" field is
// getEstAirtimeFor(len) -- also an estimate, not a measurement -- so this is the
// same number rather than a weaker substitute for one.
//
// ponytail: one estimator for every transport, because all three compute the
// same number here. sx12xx.Modem.AirtimeEstimator IS this estimator, and
// openhop's only departs from it when its preamble differs from MeshCore's,
// which setupOpenhop pins to hardware.PreambleForSF. Make the openHop preamble
// configurable and this needs routing through the driver's estimator instead.
func (r RadioInfo) airtimeMs(packetLen int) uint32 {
	if r.FreqHz == 0 {
		return 0
	}
	return hardware.LoRaAirtimeEstimator(&hardware.RadioConfig{
		FreqHz: r.FreqHz, BwHz: r.BwHz, SF: r.SF, CR: r.CR,
	})(packetLen)
}

// DeviceStats is the board's own readings. Each Have* flag is false when the
// board cannot measure that reading, so "no sensor" never reads as "measured 0":
// a Pi on the SPI path has neither a battery nor an MCU sensor we can reach.
type DeviceStats struct {
	NoiseFloor int16
	UptimeSecs uint32

	// HaveBattery is set by any reply, so a KISS board answering 0 still
	// publishes 0; only a board that never answers omits it.
	BatteryMV   uint16
	HaveBattery bool

	MCUTempC    float64
	HaveMCUTemp bool
}

// LinkStats is the transport's health counters. Throughout, nil means this
// transport cannot measure the counter at all and 0 means it measured none. The
// distinction matters on the wire: SPI-only keys are omitted when nil, while the
// KISS-only keys still publish 0 because they predate the SPI path and consumers
// may key off them.
type LinkStats struct {
	// InboundDroppedNew is a received frame thrown away because we could not
	// keep up. The SPI driver's PacketsDropped means the same thing and lands
	// here rather than under a key of its own.
	InboundDroppedNew uint64
	// HandlerSlow counts dispatches over the watchdog. RX dispatch is serial,
	// so one slow handler stalls reception for every bot.
	HandlerSlow uint64

	// KISS framing concepts. The SPI chip and the openHop protocol hand us a
	// decoded packet with its signal metadata attached, so none can occur there.
	InboundDroppedOldest *uint64
	HwErrors             *uint64 // HW_RESP_ERROR frames received
	TxOutcomeLost        *uint64 // TX_DONE waits abandoned by a reconnect
	// Both count a packet whose separate signal-metadata frame never paired
	// with it, so it is delivered with no SNR/RSSI. RxMetaTimeouts is the 1 s
	// pairing wait running out. RxMetaMisattributed is the same loss caught
	// sooner: the next packet arrived first, or a metadata frame arrived with
	// no packet to go with. Despite its name, it counts a pairing the library
	// REFUSED to make, so no published SNR/RSSI is ever wrong because of it.
	// Floods arrive in bursts, so on a real mesh most losses land here, not in
	// RxMetaTimeouts. Checked on hardware by dropping every second metadata
	// frame: every affected packet published without SNR/RSSI, and every other
	// one with exactly the firmware's values.
	RxMetaTimeouts      *uint64
	RxMetaMisattributed *uint64
	// HwDecodeErrors is a malformed SETHARDWARE frame: the battery, temperature
	// and noise-floor channel, not a mesh packet. Deliberately nil on SPI even
	// though sx12xx.PacketsRecvErrors sounds similar: that is a DATA packet
	// the driver failed to read, a different failure domain, and it is
	// published as RecvErrors below.
	HwDecodeErrors *uint64

	// RecvErrors is the radio driver failing to read a packet it knew had
	// arrived: the firmware's recv_errors. Polled from KISS firmware, read
	// directly from the SPI driver, not reported by openHop firmware.
	RecvErrors *uint64

	// Counters that only exist below the firmware, on SPI and openHop.
	CRCErrors      *uint64 // chip-level CRC and header errors: a noisy channel, not a fault
	DriverErrors   *uint64 // SPI transaction failures, busy timeouts, failed IRQ reads
	RecvRecoveries *uint64 // the watchdog re-arming a stuck receiver
}

type StatsProvider interface {
	RadioConfig() RadioInfo
	// Stats polls the board and may block briefly for its replies.
	Stats(ctx context.Context) DeviceStats
	// LinkStats takes no ctx: atomic loads and the last poll's cached values,
	// never a round trip to the board.
	LinkStats() LinkStats
}

// staleReadingAfter is how long a board reading survives without the modem
// answering anything. A modem whose port had gone away otherwise kept
// publishing its last battery voltage and temperature, so a consumer saw a
// healthy board at the moment it disappeared.
const staleReadingAfter = 45 * time.Second

type kissStatsProvider struct {
	modem     *hardware.KissModem
	radio     RadioInfo
	startTime time.Time
	log       *slog.Logger

	// lastReply is UnixNano of the modem's last answer to anything, an error
	// reply included; 0 means it has never answered.
	lastReply atomic.Int64

	// pollMu serialises Stats. The liveness probe and the MQTT status both
	// poll; one poll at a time keeps them from doubling the queries.
	pollMu sync.Mutex

	mu          sync.Mutex
	recvErrors  *uint64 // nil until the firmware answers HW_CMD_GET_STATS
	noiseFloor  int16
	batteryMV   uint16
	haveBattery bool
	mcuTempC    float64
	haveMCUTemp bool
}

// hwRequestTimeout bounds a synchronous modem request. KissModem.Request has no
// timeout of its own: it waits on the caller's context, the transport dying, or
// the modem closing.
const hwRequestTimeout = 500 * time.Millisecond

func NewKissStatsProvider(modem *hardware.KissModem, radio RadioInfo) *kissStatsProvider {
	p := &kissStatsProvider{
		modem:     modem,
		radio:     radio,
		startTime: time.Now(),
		log:       slog.Default().With("component", "stats", "type", "kiss"),
	}

	// An error reply is still an answer: the firmware answers a command it
	// lacks with HW_ERR_UNKNOWN_CMD, so the board is alive.
	modem.OnHwResponse(hardware.HW_RESP_ERROR, func(byte, []byte) { p.answered() })
	modem.OnHwResponse(hardware.HwResp(hardware.HW_CMD_GET_NOISE_FLOOR), p.onNoiseFloor)
	modem.OnHwResponse(hardware.HwResp(hardware.HW_CMD_GET_BATTERY), p.onBattery)
	modem.OnHwResponse(hardware.HwResp(hardware.HW_CMD_GET_MCU_TEMP), p.onMCUTemp)

	return p
}

func (p *kissStatsProvider) answered() { p.lastReply.Store(time.Now().UnixNano()) }

// LastReply is when the modem last answered anything; zero means never. It is
// what starts the liveness probe in modem_watch.go.
func (p *kissStatsProvider) LastReply() time.Time {
	if ns := p.lastReply.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

func (p *kissStatsProvider) RadioConfig() RadioInfo {
	return p.radio
}

func (p *kissStatsProvider) LinkStats() LinkStats {
	s := p.modem.Stats()
	p.mu.Lock()
	recvErrors := p.recvErrors
	p.mu.Unlock()
	return kissLinkStats(s, recvErrors)
}

// kissLinkStats is split out so the mapping can be checked without a modem.
func kissLinkStats(s hardware.ModemStats, recvErrors *uint64) LinkStats {
	return LinkStats{
		InboundDroppedNew:    s.InboundDroppedNew,
		HandlerSlow:          s.HandlerSlow,
		InboundDroppedOldest: &s.InboundDroppedOldest,
		HwErrors:             &s.HwErrors,
		TxOutcomeLost:        &s.TxOutcomeLost,
		RxMetaTimeouts:       &s.RxMetaTimeouts,
		RxMetaMisattributed:  &s.RxMetaMisattributed,
		HwDecodeErrors:       &s.HwDecodeErrors,
		RecvErrors:           recvErrors,
	}
}

func (p *kissStatsProvider) Stats(ctx context.Context) DeviceStats {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	// The order no longer matters. Before meshcore-go v1.7.0 an error reply to
	// GetBattery (HW_ERR_NO_CALLBACK on a board with no cell) could fail this
	// request; v1.7.0 matches each reply to its command by position, the
	// fire-and-forget queries included.
	//
	// Only PacketsErrors is taken. FirmwareStats also carries PacketsRecv and
	// PacketsSent, but packets_recv on the wire is the observer's own tally and
	// is what flood_rx + direct_rx + dups reconciles against; substituting a
	// differently derived total there is a separate change.
	reqCtx, cancel := context.WithTimeout(ctx, hwRequestTimeout)
	fw, err := p.modem.FirmwareCounters(reqCtx)
	cancel()
	if err != nil {
		p.log.Debug("firmware counters unavailable", "error", err)
	} else {
		p.answered()
		n := uint64(fw.PacketsErrors)
		p.mu.Lock()
		p.recvErrors = &n
		p.mu.Unlock()
	}

	if err := p.modem.GetNoiseFloor(); err != nil {
		p.log.Error("get noise floor", "error", err)
	}
	if err := p.modem.GetBattery(); err != nil {
		p.log.Error("get battery", "error", err)
	}
	if err := p.modem.GetMCUTemp(); err != nil {
		p.log.Error("get mcu temp", "error", err)
	}

	// Give the modem a moment to respond.
	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
	}

	ds := p.snapshot()
	p.log.Log(ctx, LevelTrace, "stats polled",
		"noise_floor", ds.NoiseFloor, "battery_mv", ds.BatteryMV,
		"mcu_temp_c", ds.MCUTempC, "uptime_secs", ds.UptimeSecs,
		"readings_current", ds.HaveBattery || ds.HaveMCUTemp)
	return ds
}

// snapshot reports a board reading only while the modem is still answering. The
// have* flags are sticky, so without this a reading outlives the board.
func (p *kissStatsProvider) snapshot() DeviceStats {
	last := p.lastReply.Load()
	fresh := last != 0 && time.Since(time.Unix(0, last)) <= staleReadingAfter

	p.mu.Lock()
	defer p.mu.Unlock()
	return DeviceStats{
		NoiseFloor:  p.noiseFloor,
		UptimeSecs:  uint32(time.Since(p.startTime).Seconds()),
		BatteryMV:   p.batteryMV,
		HaveBattery: p.haveBattery && fresh,
		MCUTempC:    p.mcuTempC,
		HaveMCUTemp: p.haveMCUTemp && fresh,
	}
}

func (p *kissStatsProvider) onNoiseFloor(_ byte, data []byte) {
	if len(data) < 2 {
		return
	}
	p.answered()
	p.mu.Lock()
	p.noiseFloor = int16(binary.LittleEndian.Uint16(data[:2]))
	p.mu.Unlock()
}

func (p *kissStatsProvider) onBattery(_ byte, data []byte) {
	if len(data) < 2 {
		return
	}
	p.answered()
	p.mu.Lock()
	p.batteryMV = binary.LittleEndian.Uint16(data[:2])
	p.haveBattery = true
	p.mu.Unlock()
}

// onMCUTemp decodes the modem's int16 tenths-of-a-degree reply
// (KissModem::handleGetMCUTemp).
func (p *kissStatsProvider) onMCUTemp(_ byte, data []byte) {
	if len(data) < 2 {
		return
	}
	p.answered()
	p.mu.Lock()
	p.mcuTempC = float64(int16(binary.LittleEndian.Uint16(data[:2]))) / 10
	p.haveMCUTemp = true
	p.mu.Unlock()
}
