package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OwlShack/meshcore-go/hardware"
	"github.com/OwlShack/meshcore-go/hardware/openhop"
	"github.com/OwlShack/meshcore-go/hardware/sx12xx"
)

// A hardware-verified board, pinned pin by pin: a preset that parses but maps a
// pin wrongly is a silently dead radio, which is the failure this exists for.
func TestVerifiedBoardPinMap(t *testing.T) {
	b, err := lookupBoard("rak6421-13300x-slot1")
	if err != nil {
		t.Fatal(err)
	}
	o := b.opts
	for _, c := range []struct{ name, got, want string }{
		{"spi port", b.SPIPort, "SPI0.0"},
		{"reset", o.ResetPin, "GPIO16"},
		{"busy", o.BusyPin, "GPIO24"},
		{"dio1", o.Dio1Pin, "GPIO22"},
		{"enables", strings.Join(o.EnablePins, ","), "GPIO12,GPIO13"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !o.UseDIO2AsRfSwitch || o.TCXOVoltage != sx12xx.TCXO1_8V || b.MaxTxPower != 22 {
		t.Errorf("rf switch/tcxo/tx power wrong: dio2=%v tcxo=%#x max=%d",
			o.UseDIO2AsRfSwitch, o.TCXOVoltage, b.MaxTxPower)
	}
}

// Every shipped preset must be drivable: the package-level parse panics on a
// bad list, but only this names the board and the reason.
func TestShippedBoardsAreComplete(t *testing.T) {
	boards, err := parseBoards(boardsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(boards) < 15 {
		t.Errorf("only %d boards; the OwlShack list has 15", len(boards))
	}
	for name, b := range boards {
		if b.opts.ResetPin == "" || b.opts.BusyPin == "" || b.MaxTxPower == 0 {
			t.Errorf("%s: missing reset/busy pin or tx power", name)
		}
	}
}

// Pins are *int because BCM 0 is a real pin: absent must not decode as GPIO0.
func TestBoardPinAbsenceIsNotGPIO0(t *testing.T) {
	b, err := parseBoards([]byte(`{"boards":{"x":{"reset_pin":0,"busy_pin":1,"tx_power":22,"verified":"community"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := b["x"].opts.ResetPin; got != "GPIO0" {
		t.Errorf("reset_pin 0 = %q, want GPIO0", got)
	}
	if got := b["x"].opts.Dio1Pin; got != "" {
		t.Errorf("absent irq_pin = %q, want empty (polled IRQ)", got)
	}
}

// en_pin is openHop's singular spelling. No shipped preset uses it, so it only
// arrives in an operator's boards.json pasted from an openHop board file, and
// dropping it there leaves the radio's power rail off: a silently dead radio.
func TestBoardAcceptsSingularEnPin(t *testing.T) {
	b, err := parseBoards([]byte(`{"boards":{"x":{"reset_pin":1,"busy_pin":2,"en_pin":7,"en_pins":[8],"tx_power":22,"verified":"community"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(b["x"].opts.EnablePins, ","); got != "GPIO8,GPIO7" {
		t.Errorf("enable pins = %q, want both spellings: GPIO8,GPIO7", got)
	}
}

func TestBoardRejections(t *testing.T) {
	for name, entry := range map[string]string{
		"no reset":   `{"busy_pin":1,"tx_power":22,"verified":"community"}`,
		"no busy":    `{"reset_pin":1,"tx_power":22,"verified":"community"}`,
		"no power":   `{"reset_pin":1,"busy_pin":2,"verified":"community"}`,
		"provenance": `{"reset_pin":1,"busy_pin":2,"tx_power":22,"verified":"trust me"}`,
		"gpio chip":  `{"reset_pin":1,"busy_pin":2,"tx_power":22,"verified":"community","gpio_chip":1}`,
		"tcxo volts": `{"reset_pin":1,"busy_pin":2,"tx_power":22,"verified":"community","use_dio3_tcxo":true,"dio3_tcxo_voltage":2.0}`,
	} {
		if _, err := parseBoards([]byte(`{"boards":{"x":` + entry + `}}`)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An operator's boards.json overrides a preset by name, and an unknown name
// lists what is known rather than failing opaquely.
func TestBoardOverrideFile(t *testing.T) {
	t.Chdir(t.TempDir())
	override := `{"boards":{"rak6421-13300x-slot1":{"reset_pin":5,"busy_pin":6,"tx_power":20,"verified":"community"}}}`
	if err := os.WriteFile(filepath.Join(".", boardsOverrideFile), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := lookupBoard("rak6421-13300x-slot1")
	if err != nil {
		t.Fatal(err)
	}
	if b.opts.ResetPin != "GPIO5" || b.MaxTxPower != 20 {
		t.Errorf("override not applied: reset=%s max=%d", b.opts.ResetPin, b.MaxTxPower)
	}
	if _, err := lookupBoard("waveshare"); err != nil {
		t.Errorf("a preset the override does not name was lost: %v", err)
	}
	if _, err := lookupBoard("nope"); err == nil || !strings.Contains(err.Error(), "waveshare") {
		t.Errorf("unknown board error does not list known boards: %v", err)
	}
}

func TestSPIConnectionNeedsBoard(t *testing.T) {
	conn := "spi://"
	if err := (&Config{Connection: &conn}).validate(); err == nil {
		t.Error("spi:// without spiBoard accepted; the pins are unknown")
	}
	board := "waveshare"
	if err := (&Config{Connection: &conn, SPIBoard: &board}).validate(); err != nil {
		t.Errorf("spi:// with spiBoard rejected: %v", err)
	}
	bad := "usb:///dev/ttyACM0"
	if err := (&Config{Connection: &bad}).validate(); err == nil {
		t.Error("unknown scheme accepted")
	}
}

// statusStats decodes the stats block of a status message into a map, so a
// key's absence is distinguishable from a zero.
func statusStats(t *testing.T, ds DeviceStats, link LinkStats) map[string]any {
	t.Helper()
	b, err := formatStatus("up", "n", "id", RadioInfo{}, ds, PacketCounts{}, 0, linkHealth{link: link})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Stats map[string]any `json:"stats"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Stats
}

// The SPI driver's read failure is the firmware's recv_errors. Mapping it into
// hw_decode_errors instead was a shipped defect in the sibling project: KISS
// hw_decode_errors is a malformed control frame, a different failure domain.
func TestSPIReadFailuresAreRecvErrors(t *testing.T) {
	ls := sx12xxLinkStats(sx12xx.RadioStats{
		PacketsRecvErrors: 3, PacketsCRCErrors: 4, PacketsDropped: 5, PacketsRecv: 100,
	})
	st := statusStats(t, DeviceStats{}, ls)
	for k, want := range map[string]float64{
		"recv_errors": 3, "crc_errors": 4, "rx_dropped": 5,
		// KISS-only keys stay on the wire as 0 on SPI; consumers key off them.
		"hw_decode_errors": 0, "rx_meta_timeouts": 0, "tx_outcome_lost": 0, "hw_errors": 0,
	} {
		if got, ok := st[k]; !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, want)
		}
	}
}

// A Pi has no battery, so the SPI path publishes no battery_mv at all rather
// than a 0 that reads as a flat cell.
func TestNoBatteryIsOmittedNotZero(t *testing.T) {
	p := newSx12xxStatsProvider(RadioInfo{})
	st := statusStats(t, p.Stats(context.Background()), p.LinkStats())
	for _, k := range []string{"battery_mv", "mcu_temp_c"} {
		if v, ok := st[k]; ok {
			t.Errorf("%s = %v published by a board that cannot measure it", k, v)
		}
	}
	if st["driver_errors"] != 0.0 {
		t.Errorf("driver_errors = %v before attach, want 0 (still reported, so a radio that never came up says why)", st["driver_errors"])
	}
}

// The SPI-only keys are omitted on KISS rather than published as zeros the
// firmware never measured.
func TestKISSOmitsSPIOnlyKeys(t *testing.T) {
	st := statusStats(t, DeviceStats{}, kissLinkStats(hardware.ModemStats{}, nil))
	for _, k := range []string{"crc_errors", "driver_errors", "recv_recoveries"} {
		if v, ok := st[k]; ok {
			t.Errorf("%s = %v published on KISS", k, v)
		}
	}
}

// A board reading is published only while the modem still answers. The have*
// flags are sticky, so without the freshness check a KISS board whose port went
// away kept publishing its last voltage as if healthy.
func TestKISSBoardReadingsGoStale(t *testing.T) {
	p := &kissStatsProvider{startTime: time.Now()}
	p.onBattery(0, []byte{0xA0, 0x0F}) // 4000 mV
	if ds := p.snapshot(); !ds.HaveBattery || ds.BatteryMV != 4000 {
		t.Fatalf("fresh reading not reported: %+v", ds)
	}
	p.lastReply.Store(time.Now().Add(-staleReadingAfter - time.Second).UnixNano())
	if ds := p.snapshot(); ds.HaveBattery {
		t.Error("battery still reported after the modem stopped answering")
	}
}

// fakeOpenhopBoard speaks the real frame format on one TCP connection. It
// records the radio config the host pushes and answers STATUS with status.
func fakeOpenhopBoard(t *testing.T, status []byte) (addr string, pushed func() openhop.RadioConfig) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var mu sync.Mutex
	var cfg openhop.RadioConfig
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var buf []byte
		chunk := make([]byte, 512)
		for {
			n, err := conn.Read(chunk)
			if err != nil {
				return
			}
			buf = append(buf, chunk[:n]...)
			for len(buf) >= 6 {
				if buf[0] != openhop.Sync {
					buf = buf[1:]
					continue
				}
				size := int(binary.LittleEndian.Uint16(buf[2:4]))
				if len(buf) < 6+size {
					break
				}
				cmd, payload := buf[1], append([]byte(nil), buf[4:4+size]...)
				buf = buf[6+size:]
				var reply []byte
				switch cmd {
				case openhop.CmdPing:
					reply = openhop.EncodeFrame(openhop.CmdPong, nil)
				case openhop.CmdSetConfig:
					if c, err := openhop.ParseRadioConfig(payload); err == nil {
						mu.Lock()
						cfg = c
						mu.Unlock()
					}
					reply = openhop.EncodeFrame(openhop.CmdConfigResp, payload)
				case openhop.CmdStatusReq:
					reply = openhop.EncodeFrame(openhop.CmdStatusResp, status)
				}
				if reply != nil {
					if _, err := conn.Write(reply); err != nil {
						return
					}
				}
			}
		}
	}()
	return ln.Addr().String(), func() openhop.RadioConfig {
		mu.Lock()
		defer mu.Unlock()
		return cfg
	}
}

// statusPayload builds a 26-byte STATUS_RESP (battery field included).
func statusPayload(crc uint32, tempC int8, batteryMV uint16) []byte {
	b := make([]byte, 26)
	binary.LittleEndian.PutUint32(b[12:16], crc)
	b[22] = byte(tempC)
	binary.LittleEndian.PutUint16(b[24:26], batteryMV)
	return b
}

// End to end through setupOpenhop against a board speaking the real protocol.
// The pushed preamble is the load-bearing part: it is what keeps every airtime
// estimate in this process exact on openHop, and a wrong one takes the node off
// the air with the rest of the mesh.
func TestOpenhopSetupPushesMeshCoreRadioConfig(t *testing.T) {
	addr, pushed := fakeOpenhopBoard(t, statusPayload(7, 31, 3950))
	cfg := DefaultConfig()
	ms := &modemState{}
	radio := RadioInfo{FreqHz: 917_375_000, BwHz: 62_500, SF: 7, CR: 5, TxPower: 22}
	ms.radioConfig = &hardware.RadioConfig{FreqHz: radio.FreqHz, BwHz: radio.BwHz, SF: radio.SF, CR: radio.CR}

	if err := setupOpenhop(t.Context(), ms, &cfg, addr, radio); err != nil {
		t.Fatalf("setupOpenhop: %v", err)
	}
	t.Cleanup(ms.Close)

	got := pushed()
	if got.SyncWord != openhopSyncWord || got.PreambleLen != uint8(sx12xx.PreambleForSF(7)) {
		t.Errorf("pushed sync=%#x preamble=%d, want %#x and %d (MeshCore's)",
			got.SyncWord, got.PreambleLen, openhopSyncWord, sx12xx.PreambleForSF(7))
	}
	if got.FreqHz != radio.FreqHz || got.SF != 7 || got.TxPower != 22 {
		t.Errorf("pushed radio config wrong: %+v", got)
	}

	// With the preamble pinned, the driver's estimator and ours must agree,
	// which is the claim RadioInfo.airtimeMs rests on.
	m := ms.modem.(*openhop.Modem)
	for _, n := range []int{10, 100, 255} {
		if a, b := m.AirtimeEstimator()(n), radio.airtimeMs(n); a != b {
			t.Errorf("airtime(%d): driver %d ms, ours %d ms", n, a, b)
		}
	}

	if ls := ms.stats.LinkStats(); ls.CRCErrors != nil {
		t.Errorf("crc_errors = %d before the board answered, want absent", *ls.CRCErrors)
	}
	st := statusStats(t, ms.stats.Stats(t.Context()), ms.stats.LinkStats())
	for k, want := range map[string]float64{"battery_mv": 3950, "mcu_temp_c": 31, "crc_errors": 7} {
		if st[k] != want {
			t.Errorf("%s = %v, want %v", k, st[k], want)
		}
	}
}

// openHop marks a missing sensor with a sentinel (temp -128, battery 0xFFFF),
// which must become an absent key, not a published -128 or 65535.
func TestOpenhopSentinelsAreOmitted(t *testing.T) {
	addr, _ := fakeOpenhopBoard(t, statusPayload(0, -128, 0xFFFF))
	cfg := DefaultConfig()
	ms := &modemState{}
	radio := RadioInfo{FreqHz: 917_375_000, BwHz: 62_500, SF: 7, CR: 5, TxPower: 22}
	if err := setupOpenhop(t.Context(), ms, &cfg, addr, radio); err != nil {
		t.Fatalf("setupOpenhop: %v", err)
	}
	t.Cleanup(ms.Close)

	st := statusStats(t, ms.stats.Stats(t.Context()), ms.stats.LinkStats())
	for _, k := range []string{"battery_mv", "mcu_temp_c"} {
		if v, ok := st[k]; ok {
			t.Errorf("%s = %v published for a board without that sensor", k, v)
		}
	}
}

// A SIGHUP that only swaps the hat or the openHop token must rebuild the modem:
// neither is in the connection string, so the old comparison could not see it
// and the process would keep driving the old board.
func TestModemKeysOutsideConnectionForceReconnect(t *testing.T) {
	s := func(v string) *string { return &v }
	conn := s("spi://")
	for _, tc := range []struct {
		name     string
		old, new *Config
	}{
		{"spiBoard", &Config{Connection: conn, SPIBoard: s("waveshare")}, &Config{Connection: conn, SPIBoard: s("zebra")}},
		{"modemToken", &Config{Connection: conn, ModemToken: s("a")}, &Config{Connection: conn, ModemToken: s("b")}},
	} {
		if !modemConfigChanged(tc.old, tc.new) {
			t.Errorf("%s change not detected", tc.name)
		}
	}
	// nodeType is ignored now, so changing it alone must NOT churn the radio.
	if modemConfigChanged(&Config{Connection: conn, NodeType: s("kiss")}, &Config{Connection: conn}) {
		t.Error("an ignored nodeType change forced a modem reconnect")
	}
}

// fakeWatchModem is a node.Modem with a Dead() channel the test controls.
type fakeWatchModem struct{ dead chan struct{} }

func (fakeWatchModem) SendData([]byte) error                            { return nil }
func (fakeWatchModem) SetDataHandler(func([]byte, float32, int8, bool)) {}
func (fakeWatchModem) AddOutboundHandler(func([]byte))                  {}
func (f fakeWatchModem) Dead() <-chan struct{}                          { return f.dead }

// fakeProbeStats answers a status poll by moving LastReply, until hung.
type fakeProbeStats struct {
	mu     sync.Mutex
	last   time.Time
	hung   bool
	polls  int
	silent bool // never answers at all
}

func (f *fakeProbeStats) RadioConfig() RadioInfo { return RadioInfo{} }
func (f *fakeProbeStats) LinkStats() LinkStats   { return LinkStats{} }
func (f *fakeProbeStats) Stats(context.Context) DeviceStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if !f.hung && !f.silent {
		f.last = time.Now()
	}
	return DeviceStats{}
}
func (f *fakeProbeStats) LastReply() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func fastProbe(t *testing.T) {
	old := probeInterval
	probeInterval = 5 * time.Millisecond
	t.Cleanup(func() { probeInterval = old })
}

func expectReport(t *testing.T, died <-chan *modemState, want *modemState, within time.Duration) {
	t.Helper()
	select {
	case got := <-died:
		if got != want {
			t.Fatal("reported a different modem than the one that died")
		}
	case <-time.After(within):
		t.Fatal("dead modem not reported")
	}
}

func expectNoReport(t *testing.T, died <-chan *modemState, within time.Duration) {
	t.Helper()
	select {
	case <-died:
		t.Fatal("reported a modem that had not died")
	case <-time.After(within):
	}
}

// A transport whose read loop exits (USB unplugged, TCP dropped) is reported,
// and it reports the modem itself so the main loop can ignore a stale one.
func TestWatchReportsDeadTransport(t *testing.T) {
	dead := make(chan struct{})
	ms := &modemState{modem: fakeWatchModem{dead}, stats: newSx12xxStatsProvider(RadioInfo{}), watchDone: make(chan struct{})}
	defer ms.Close()
	died := make(chan *modemState)
	ms.watch(died)
	close(dead)
	expectReport(t, died, ms, time.Second)
}

// Closing a modem ends its read loop, which fires Dead(). A deliberate close,
// on shutdown or SIGHUP, must not be taken for a fault and reconnected, even
// when Close runs on another goroutine while a receiver is waiting: the main
// loop receives and closes on one goroutine, so there a report could not be
// delivered mid-Close anyway, and this is the case where close order matters.
func TestWatchIgnoresDeliberateClose(t *testing.T) {
	dead := make(chan struct{})
	ms := &modemState{modem: fakeWatchModem{dead}, stats: newSx12xxStatsProvider(RadioInfo{}), watchDone: make(chan struct{}),
		// Closing the modem is what fires Dead(), exactly as a real KISS
		// transport's read loop exits on Close. The pause stands in for the
		// rest of a real close, and gives the watcher time to act on Dead()
		// before Close returns, which is the window the close order protects.
		closers: []io.Closer{closerFunc(func() { close(dead); time.Sleep(10 * time.Millisecond) })}}
	died := make(chan *modemState)
	ms.watch(died)
	go ms.Close()
	expectNoReport(t, died, 100*time.Millisecond)
}

// A board that stays plugged in but stops answering never fires Dead(); the
// probe catches it after probeMisses unanswered polls, and not before.
func TestWatchProbeCatchesSilentBoard(t *testing.T) {
	fastProbe(t)
	st := &fakeProbeStats{}
	ms := &modemState{modem: fakeWatchModem{make(chan struct{})}, stats: st, watchDone: make(chan struct{})}
	defer ms.Close()
	died := make(chan *modemState)
	ms.watch(died)
	expectNoReport(t, died, 50*time.Millisecond) // answering: no report

	st.mu.Lock()
	st.hung = true
	hungAt := st.polls
	st.mu.Unlock()
	expectReport(t, died, ms, time.Second)
	st.mu.Lock()
	defer st.mu.Unlock()
	if n := st.polls - hungAt; n < probeMisses {
		t.Errorf("reported after %d unanswered polls, want at least %d", n, probeMisses)
	}
}

// A modem that has never answered may simply not implement the queries: a
// probe that reconnected it would reconnect a working radio forever.
func TestWatchProbeLeavesNeverAnsweredModem(t *testing.T) {
	fastProbe(t)
	st := &fakeProbeStats{silent: true}
	ms := &modemState{modem: fakeWatchModem{make(chan struct{})}, stats: st, watchDone: make(chan struct{})}
	defer ms.Close()
	died := make(chan *modemState)
	ms.watch(died)
	expectNoReport(t, died, 20*probeInterval*probeMisses)
}

// Close is called from error paths and teardown alike; a second call must not
// panic on the already-closed watcher channel.
func TestModemCloseIsIdempotent(t *testing.T) {
	ms := &modemState{watchDone: make(chan struct{})}
	ms.Close()
	ms.Close()
}
