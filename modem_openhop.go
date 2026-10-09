package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/OwlShack/meshcore-go/hardware"
	"github.com/OwlShack/meshcore-go/hardware/openhop"
)

// openhopSyncWord is MeshCore's private sync word: the firmware's own default,
// and the only value that hears the mesh.
const openhopSyncWord = 0x12

// setupOpenhop drives openHop Modem firmware, which speaks its own protocol
// rather than KISS. It owns the radio and does channel-activity detection
// itself, so this process only frames packets. The driver reconnects on its
// own, re-pushing the radio config, so a dropped link heals without a restart.
func setupOpenhop(ctx context.Context, ms *modemState, cfg *Config, connAddr string, radio RadioInfo) error {
	var token string
	var dial openhop.Dialer
	if strings.HasPrefix(connAddr, "/") {
		// Auth is a TCP-client concept; the firmware never asks a serial
		// client for a token.
		// baudRate is ignored: every openHop board's UART is fixed at 921600
		// in firmware, so there is nothing to override, and a stored value is
		// usually KISS's 115200. The legacy-observer migration wrote resolved
		// defaults back to disk, so older config files carry baudRate = 115200
		// whether or not anyone chose it. Honouring it connects to a board
		// behind a USB-UART bridge (Heltec V3's CP2102) at the wrong speed.
		if cfg.BaudRate != nil && *cfg.BaudRate != openhop.DefaultBaudRate {
			slog.Warn("baudRate ignored for openhop: the firmware's UART is fixed",
				"component", "modem", "configured", *cfg.BaudRate, "using", openhop.DefaultBaudRate)
		}
		dial = openhop.SerialDialer(connAddr, openhop.DefaultBaudRate)
	} else {
		if cfg.ModemToken != nil {
			token = *cfg.ModemToken
		}
		dial = openhop.TCPDialer(connAddr, 0)
	}

	// PreambleLen is derived rather than left to the modem: openHop's own
	// default is tuned for its firmware, and a preamble MeshCore does not use
	// takes us off the air with every other node. It is also what keeps the
	// airtime estimate in stats.go exact; see RadioInfo.airtimeMs.
	rc := openhop.RadioConfig{
		FreqHz:      radio.FreqHz,
		BandwidthHz: radio.BwHz,
		SF:          radio.SF,
		CR:          radio.CR,
		TxPower:     int8(radio.TxPower),
		SyncWord:    openhopSyncWord,
		PreambleLen: uint8(hardware.PreambleForSF(radio.SF)),
	}

	m := openhop.New(dial,
		openhop.Config{Token: token, Radio: rc},
		openhop.WithLogger(slog.Default()),
		openhop.WithErrorHandler(func(err error) {
			slog.Warn("modem error", "component", "modem", "error", err)
		}),
	)
	m.SetLogHandler(func(l openhop.LogLevel, text string) {
		slog.Debug("modem log", "component", "modem", "level", l.String(), "text", text)
	})

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := m.Connect(connectCtx); err != nil {
		m.Close()
		return fmt.Errorf("openhop connect: %w", err)
	}
	ms.closers = append(ms.closers, m)

	slog.Info("radio up", "component", "modem", "transport", "openhop", "target", connAddr,
		"freq", *cfg.Freq, "bw", *cfg.Bw, "sf", *cfg.SF, "cr", *cfg.CR, "tx", radio.TxPower,
		"preamble_symbols", rc.PreambleLen)

	ms.stats = &openhopStatsProvider{modem: m, radio: radio, startTime: time.Now(),
		log: slog.Default().With("component", "stats", "type", "openhop")}
	ms.modem = m
	return nil
}

// openhopStatsProvider reads the firmware's own counters. One STATUS command
// answers with every reading at once, so unlike KISS there is nothing to fan
// out.
type openhopStatsProvider struct {
	modem     *openhop.Modem
	radio     RadioInfo
	startTime time.Time
	log       *slog.Logger

	mu     sync.Mutex
	last   openhop.Status
	lastAt time.Time
}

func (p *openhopStatsProvider) RadioConfig() RadioInfo { return p.radio }

// LastReply is the modem's last STATUS answer. It starts the liveness probe,
// which a serial link needs: the driver reconnects a dropped link itself, and
// TCP drops a silent one after 60 s, but serial has no idle deadline, so a hung
// board otherwise stays connected and healthy-looking.
func (p *openhopStatsProvider) LastReply() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastAt
}

// Stats has no timeout of its own to add: openhop.Modem.Status bounds itself.
func (p *openhopStatsProvider) Stats(ctx context.Context) DeviceStats {
	if st, err := p.modem.Status(ctx); err != nil {
		p.log.Debug("status unavailable", "error", err)
	} else {
		p.mu.Lock()
		p.last, p.lastAt = st, time.Now()
		p.mu.Unlock()
	}
	return p.snapshot()
}

// snapshot drops the board readings once the modem stops answering, so a stale
// voltage cannot look like a healthy board. UptimeSecs is time since this link
// came up, as on the other transports: the modem's own uptime would make one
// field mean two different things depending on the modem.
func (p *openhopStatsProvider) snapshot() DeviceStats {
	p.mu.Lock()
	st, at := p.last, p.lastAt
	p.mu.Unlock()

	ds := DeviceStats{UptimeSecs: uint32(time.Since(p.startTime).Seconds())}
	if at.IsZero() || time.Since(at) > staleReadingAfter {
		return ds
	}
	ds.NoiseFloor = int16(st.NoiseFloor)
	ds.BatteryMV, ds.HaveBattery = st.BatteryMV, st.BatteryValid
	ds.MCUTempC, ds.HaveMCUTemp = float64(st.TempC), st.TempValid
	return ds
}

// LinkStats leaves the KISS framing fields nil: openHop frames carry their own
// CRC and length, so none of those faults exist here. RecvErrors stays nil too,
// because the firmware does not report one.
//
// ModemStats.DecodeErrors is deliberately NOT mapped to hw_decode_errors. It
// counts malformed openHop protocol frames; the KISS key counts malformed
// SETHARDWARE frames. Same shape of name, different failure domain, and a sum
// across transports would mean nothing. OwlShack leaves it unmapped too.
func (p *openhopStatsProvider) LinkStats() LinkStats {
	ls := LinkStats{InboundDroppedNew: p.modem.Stats().InboundDropped}

	p.mu.Lock()
	defer p.mu.Unlock()
	// CRC errors is a cumulative chip counter, so the last answer stays
	// meaningful after the board goes quiet; only "never answered" is absent.
	if !p.lastAt.IsZero() {
		crc := uint64(p.last.CRCErrors)
		ls.CRCErrors = &crc
	}
	return ls
}
