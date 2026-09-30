package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshcore-go/meshcore-go/hardware/sx12xx"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)

// periphOnce guards host.Init: a SIGHUP reconnect re-opens the SPI port, not
// the host.
var periphOnce struct {
	sync.Once
	err error
}

func initPeriph() error {
	periphOnce.Do(func() {
		if _, err := host.Init(); err != nil {
			periphOnce.err = fmt.Errorf("periph host init: %w", err)
		}
	})
	return periphOnce.err
}

// spiPortKey reduces "SPI0.1" and "/dev/spidev0.1" to "0.1", so the same device
// spelled two ways does not read as a mismatch.
func spiPortKey(s string) string {
	s = strings.TrimPrefix(s, "/dev/spidev")
	return strings.TrimPrefix(s, "SPI")
}

// setupSPI drives an SX1262 wired straight to the host's SPI bus. There is no
// MeshCore firmware in front of the chip, so this process is the radio stack.
func setupSPI(ms *modemState, cfg *Config, connAddr string, radio RadioInfo) error {
	board, err := lookupBoard(*cfg.SPIBoard)
	if err != nil {
		return err
	}
	if board.Verified != "hardware" {
		slog.Warn("board wiring has not been verified on hardware",
			"component", "modem", "board", board.Name, "provenance", board.Verified,
			"notes", board.Notes)
	}

	// Past the module's rating the PA cooks, so this is a hard error rather
	// than a silent clamp.
	if radio.TxPower > board.MaxTxPower {
		return fmt.Errorf("tx power %d dBm exceeds %s maximum of %d dBm",
			radio.TxPower, board.Label, board.MaxTxPower)
	}

	if err := initPeriph(); err != nil {
		return err
	}

	portName := connAddr
	if portName == "" {
		portName = board.SPIPort
	}
	// The wrong chip-select reads back as a chip that never answers, which the
	// driver reports as bad wiring, so say plainly that the two disagree before
	// that more confusing error appears.
	if connAddr != "" && spiPortKey(connAddr) != spiPortKey(board.SPIPort) {
		slog.Warn("connection names a different SPI device than this board's chip-select",
			"component", "modem", "board", board.Name,
			"connection", connAddr, "boardExpects", board.SPIPort)
	}
	port, err := spireg.Open(portName)
	if err != nil {
		return fmt.Errorf("open spi %s: %w", portName, err)
	}
	ms.closers = append(ms.closers, port)

	opts := board.opts
	dropMissingLEDs(&opts, board.Name)
	chip, err := sx12xx.NewSX126x(port, &opts)
	if err != nil {
		ms.Close()
		return fmt.Errorf("sx126x on %s: %w", portName, err)
	}

	if de, err := chip.DeviceErrors(); err != nil {
		slog.Warn("radio device errors unreadable", "component", "modem", "error", err)
	} else if de != 0 {
		slog.Error("radio reports device errors", "component", "modem",
			"errors", fmt.Sprintf("%#04x", de), "hint", "check TCXO voltage and frequency band")
	}

	stats := newSx12xxStatsProvider(radio)
	m, err := sx12xx.NewModem(chip, ms.radioConfig,
		sx12xx.WithTxPower(int(radio.TxPower)),
		sx12xx.WithModemLogger(slog.Default()),
		// The KISS threshold, so handler_slow means the same on every transport.
		sx12xx.WithHandlerWatchdog(rxHandlerWatchdog),
		// noteError owns driver_errors. A driver fault is not a packet that
		// failed to parse, so it must not also land in packet_parse_errors.
		sx12xx.WithModemErrorHandler(stats.noteError),
	)
	if err != nil {
		ms.Close()
		return fmt.Errorf("sx12xx modem: %w", err)
	}
	ms.closers = append(ms.closers, m)
	stats.modem.Store(m)

	slog.Info("radio up", "component", "modem", "transport", "spi",
		"board", board.Name, "chip", board.Chip, "spi", portName,
		"freq", *cfg.Freq, "bw", *cfg.Bw, "sf", *cfg.SF, "cr", *cfg.CR, "tx", radio.TxPower,
		"preamble_symbols", sx12xx.PreambleForSF(radio.SF))

	ms.stats = stats
	ms.modem = m
	return nil
}

// dropMissingLEDs clears LED pins this host does not have. The driver rejects
// an unknown pin name, and a cosmetic pin from an unverified board entry must
// not stop the radio.
func dropMissingLEDs(o *sx12xx.Opts, board string) {
	for _, led := range []struct {
		role string
		pin  *string
	}{{"tx", &o.TxLedPin}, {"rx", &o.RxLedPin}} {
		if *led.pin == "" || gpioreg.ByName(*led.pin) != nil {
			continue
		}
		slog.Warn("activity LED pin not present on this host, LED disabled",
			"component", "modem", "board", board, "role", led.role, "pin", *led.pin)
		*led.pin = ""
	}
}

// sx12xxStatsProvider reports what a directly attached SPI radio can measure.
type sx12xxStatsProvider struct {
	// modem is stored after NewModem, which needs noteError first; until then
	// the provider still reports the driver errors that stopped it coming up.
	modem     atomic.Pointer[sx12xx.Modem]
	radio     RadioInfo
	startTime time.Time
	log       *slog.Logger

	// driverErrors counts SPI transaction failures, busy-line timeouts and
	// failed IRQ reads, from the driver's error handler.
	driverErrors atomic.Uint64
}

func newSx12xxStatsProvider(radio RadioInfo) *sx12xxStatsProvider {
	return &sx12xxStatsProvider{
		radio:     radio,
		startTime: time.Now(),
		log:       slog.Default().With("component", "stats", "type", "spi"),
	}
}

func (p *sx12xxStatsProvider) noteError(err error) {
	p.driverErrors.Add(1)
	p.log.Warn("radio driver error", "error", err)
}

func (p *sx12xxStatsProvider) RadioConfig() RadioInfo { return p.radio }

// Stats reads chip registers, so there is nothing to wait for. Battery and MCU
// temperature stay absent: a Pi has neither sensor on this path.
func (p *sx12xxStatsProvider) Stats(context.Context) DeviceStats {
	ds := DeviceStats{UptimeSecs: uint32(time.Since(p.startTime).Seconds())}
	if m := p.modem.Load(); m != nil {
		ds.NoiseFloor = int16(m.NoiseFloor())
	}
	return ds
}

// LinkStats leaves the KISS framing fields nil: those events cannot occur on
// this path, because the chip hands us a decoded packet with its signal attached.
func (p *sx12xxStatsProvider) LinkStats() LinkStats {
	driver := p.driverErrors.Load()
	m := p.modem.Load()
	if m == nil {
		return LinkStats{DriverErrors: &driver}
	}
	ls := sx12xxLinkStats(m.Stats())
	recoveries := m.RecvRecoveries()
	ls.DriverErrors, ls.RecvRecoveries = &driver, &recoveries
	ls.HandlerSlow = m.HandlerSlow()
	return ls
}

// sx12xxLinkStats maps the driver's counters onto the shared fields.
//
// PacketsRecvErrors goes to RecvErrors, NOT HwDecodeErrors. Both sound like
// "a read failed", but KISS hw_decode_errors is a malformed control frame while
// this is a data packet that raised an interrupt and could not be read from
// the FIFO: the firmware's recv_errors, same operation, same point in the stack.
// PacketsRecv/PacketsSent are the chip's own totals and are deliberately not
// used: packets_recv on the wire is the observer's tally.
func sx12xxLinkStats(s sx12xx.RadioStats) LinkStats {
	crc, recvErrs := s.PacketsCRCErrors, s.PacketsRecvErrors
	return LinkStats{
		InboundDroppedNew: s.PacketsDropped,
		CRCErrors:         &crc,
		RecvErrors:        &recvErrs,
	}
}
