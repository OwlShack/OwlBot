package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/hardware"
	"github.com/OwlShack/meshcore-go/node"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func decodePacket(t *testing.T, pkt *meshcore.Packet, raw []byte, dir string, radio RadioInfo) map[string]any {
	t.Helper()
	b, err := formatPacket(pkt, raw, "n", "id", dir, radio)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOnMCUTempDecodesSignedTenths(t *testing.T) {
	p := &kissStatsProvider{}
	p.onMCUTemp(0, []byte{0xC0, 0xFF}) // -64 tenths
	if !p.haveMCUTemp || p.mcuTempC != -6.4 {
		t.Fatalf("got %v (have=%v), want -6.4", p.mcuTempC, p.haveMCUTemp)
	}
}

func TestStatusOmitsUnknownMCUTemp(t *testing.T) {
	// A board with no sensor must not report 0 C.
	b, err := formatStatus("up", "n", "id", RadioInfo{}, DeviceStats{}, PacketCounts{}, 0, linkHealth{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "mcu_temp_c") {
		t.Fatalf("unknown temp published: %s", b)
	}

	// A real 0 C reading must be published.
	b, err = formatStatus("up", "n", "id", RadioInfo{}, DeviceStats{HaveMCUTemp: true}, PacketCounts{}, 0, linkHealth{})
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Stats struct {
			MCUTempC *float64 `json:"mcu_temp_c"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Stats.MCUTempC == nil || *msg.Stats.MCUTempC != 0 {
		t.Fatalf("real 0 C reading dropped: %s", b)
	}
}

// The two drop counters mean opposite things to an operator (RF congestion vs
// sending too fast), so a swapped mapping must fail loudly.
func TestStatusMapsTxCountersDistinctly(t *testing.T) {
	tx := node.TxStats{Sent: 7, BusyRequeued: 1, BusyDropped: 2, QueueRejected: 3, Failed: 4}
	b, err := formatStatus("up", "n", "id", RadioInfo{}, DeviceStats{}, PacketCounts{}, 0, linkHealth{tx: tx, queueLen: 5})
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Stats struct {
			PacketsSent    uint64 `json:"sent"`
			QueueLen       int    `json:"queue_len"`
			TxRequeued     uint64 `json:"tx_requeued"`
			TxDroppedBusy  uint64 `json:"tx_dropped_busy"`
			TxDroppedQueue uint64 `json:"tx_dropped_queue"`
			TxFailed       uint64 `json:"tx_failed"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	got := msg.Stats
	if got.PacketsSent != 7 || got.QueueLen != 5 || got.TxRequeued != 1 ||
		got.TxDroppedBusy != 2 || got.TxDroppedQueue != 3 || got.TxFailed != 4 {
		t.Fatalf("counter mapping wrong: %+v", got)
	}
}

// Each modem counter is pinned to its own key with a distinct value, so a
// counter wired to the wrong key, which would report one fault as another,
// fails here.
func TestStatusMapsModemCounters(t *testing.T) {
	// Through kissLinkStats, so the provider's mapping is covered as well as
	// the wire's: a field wired wrong in either one fails here.
	h := linkHealth{link: kissLinkStats(hardware.ModemStats{
		InboundDroppedOldest: 2, InboundDroppedNew: 3,
		RxMetaTimeouts: 4, RxMetaMisattributed: 5,
		HandlerSlow: 6, HwDecodeErrors: 7, HwErrors: 8, TxOutcomeLost: 9,
	}, nil)}
	b, err := formatStatus("up", "n", "id", RadioInfo{}, DeviceStats{}, PacketCounts{}, 0, h)
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Stats struct {
			RxDropped           uint64 `json:"rx_dropped"`
			RxMetaTimeouts      uint64 `json:"rx_meta_timeouts"`
			RxMetaMisattributed uint64 `json:"rx_meta_misattributed"`
			HandlerSlow         uint64 `json:"handler_slow"`
			HwDecodeErrors      uint64 `json:"hw_decode_errors"`
			HwErrors            uint64 `json:"hw_errors"`
			TxOutcomeLost       uint64 `json:"tx_outcome_lost"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	g := msg.Stats
	if g.RxDropped != 5 { // oldest + new
		t.Errorf("rx_dropped = %d, want 5 (2 oldest + 3 new)", g.RxDropped)
	}
	if g.RxMetaTimeouts != 4 || g.RxMetaMisattributed != 5 || g.HandlerSlow != 6 ||
		g.HwDecodeErrors != 7 || g.HwErrors != 8 || g.TxOutcomeLost != 9 {
		t.Errorf("modem counter mapping wrong: %+v", g)
	}
}

// The firmware's derivation (CommonCLI handleSetCmd): factor = (100/dc) - 1.
// Getting this backwards is the trap the dutyCycle knob exists to remove.
func TestDutyCycleDerivesFirmwareFactor(t *testing.T) {
	for _, tc := range []struct {
		dc   float64
		want float64
	}{{100, 0}, {50, 1}, {1, 99}, {0.1, 999}} { // 0.1 = an EU868 sub-band
		c := Config{DutyCycle: &tc.dc}
		if got := c.effectiveAirtimeFactor(); got != tc.want {
			t.Errorf("dutyCycle %v: factor = %v, want %v", tc.dc, got, tc.want)
		}
	}
}

func TestAirtimeConfigValidation(t *testing.T) {
	over, zero, neg, sub := 200.0, 0.0, -1.0, 0.1
	for _, bad := range []float64{over, zero, neg} {
		if err := (&Config{DutyCycle: &bad}).validate(); err == nil {
			t.Errorf("accepted out-of-range dutyCycle %v", bad)
		}
	}
	// Sub-1% must be accepted; firmware's own knob rejects it.
	if err := (&Config{DutyCycle: &sub}).validate(); err != nil {
		t.Errorf("rejected 0.1%% duty cycle: %v", err)
	}
	// Unset must resolve to the library default, not a hardcoded percent.
	if got := (&Config{}).effectiveAirtimeFactor(); got != node.DefaultAirtimeFactor {
		t.Errorf("default factor = %v, want %v", got, node.DefaultAirtimeFactor)
	}
}

// A duty-cycle change only reaches the radio if modemConfigChanged says so:
// the factor is baked into the RadioMux, which SIGHUP rebuilds only on that
// predicate. Miss it and the value persists but never applies until restart.
func TestDutyCycleChangeForcesMuxRebuild(t *testing.T) {
	f := func(pct *float64) *Config { return &Config{DutyCycle: pct} }
	p := func(v float64) *float64 { return &v }

	for _, tc := range []struct {
		name     string
		old, new *Config
		want     bool
	}{
		{"set to set", f(p(50)), f(p(0.1)), true},
		{"set to unset", f(p(0.1)), f(nil), true},
		{"unset to set", f(nil), f(p(0.1)), true},
		{"unchanged", f(p(50)), f(p(50)), false},
		{"both unset", f(nil), f(nil), false},
		// A real change deep in the fractional range must still register.
		{"1% to 0.1%", f(p(1)), f(p(0.1)), true},
		// Unset resolves to the library default, so an explicit 50 matching it
		// is genuinely no change and must not force a needless reconnect.
		// Asserted both directions: comparing resolved values runs each side
		// through a division, so the symmetry is not free.
		{"unset to equivalent explicit", f(nil), f(p(100 / (1 + node.DefaultAirtimeFactor))), false},
		{"equivalent explicit to unset", f(p(100 / (1 + node.DefaultAirtimeFactor))), f(nil), false},
	} {
		if got := modemConfigChanged(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: modemConfigChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Firmware prints (int)pkt->getSNR(), a C cast that truncates toward zero, and
// the upstream regex is SNR=(-?\d+). "%.0f" would round -4.75 to -5 and "%.2f"
// would emit a decimal the regex drops entirely. Both are silent on the wire.
func TestPacketSNRTruncatesTowardZero(t *testing.T) {
	radio := RadioInfo{FreqHz: 917375000, BwHz: 62500, SF: 7, CR: 8}
	for _, tc := range []struct {
		snr  float32
		want string
	}{{-4.75, "-4"}, {4.75, "4"}, {-0.5, "0"}, {0, "0"}, {7, "7"}} {
		pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeAdvert, 0), SNR: tc.snr, HasSignalInfo: true}
		if got := decodePacket(t, pkt, []byte{1, 2, 3}, "rx", radio)["SNR"]; got != tc.want {
			t.Errorf("SNR %v published as %q, want %q", tc.snr, got, tc.want)
		}
	}
}

// A TX row must not carry measurements of our own transmission.
func TestPacketMeasurementsAreRxOnly(t *testing.T) {
	radio := RadioInfo{FreqHz: 917375000, BwHz: 62500, SF: 7, CR: 8}
	pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeAdvert, 0), SNR: 5, RSSI: -80, HasSignalInfo: true}

	tx := decodePacket(t, pkt, []byte{1, 2, 3}, "tx", radio)
	// duration included: the firmware's TX line prints no time= and the bridge
	// sets duration only in its rx-only block, so it is NOT a tx field. hash is
	// the sole deliberate tx extension.
	for _, k := range []string{"SNR", "RSSI", "score", "duration", "path"} {
		if v, ok := tx[k]; ok {
			t.Errorf("tx row published %s = %v; not a tx field per firmware and bridge", k, v)
		}
	}
	if _, ok := tx["hash"]; !ok {
		t.Error("tx row dropped hash; we compute it, so it is not a measurement")
	}

	rx := decodePacket(t, pkt, []byte{1, 2, 3}, "rx", radio)
	for _, k := range []string{"SNR", "RSSI", "score", "duration"} {
		if _, ok := rx[k]; !ok {
			t.Errorf("rx row missing %s", k)
		}
	}
}

// The path trailer is payload[1] -> payload[0], and only for the four payload
// types the firmware prints it for (Dispatcher.cpp:231).
func TestPathTrailerMatchesFirmware(t *testing.T) {
	for _, pt := range []byte{meshcore.PayloadTypePath, meshcore.PayloadTypeReq, meshcore.PayloadTypeResponse, meshcore.PayloadTypeTxtMsg} {
		pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, pt, 0), Payload: []byte{0xAB, 0xCD}}
		if got := pathTrailer(pkt); got != "CD -> AB" {
			t.Errorf("type %d: path = %q, want %q (payload[1] -> payload[0])", pt, got, "CD -> AB")
		}
	}
	// Not printed for other types, and never from a short payload.
	if got := pathTrailer(&meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeAdvert, 0), Payload: []byte{0xAB, 0xCD}}); got != "" {
		t.Errorf("advert got a path trailer: %q", got)
	}
	if got := pathTrailer(&meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTxtMsg, 0), Payload: []byte{0xAB}}); got != "" {
		t.Errorf("1-byte payload got a path trailer: %q", got)
	}
}

// recv_errors is the RADIO driver's failure-to-receive count, polled from the
// firmware, and packet_parse_errors is ours. Before v1.2.0 the parse count was
// published as recv_errors, so this node's value meant something different from
// a firmware node's under the same key on the same topic. Distinct values here
// because equal ones would pass with the two sources swapped.
func TestRecvErrorsIsRadioNotParseFailures(t *testing.T) {
	nine := uint64(9)
	b, err := formatStatus("up", "n", "id", RadioInfo{},
		DeviceStats{}, PacketCounts{}, 4, linkHealth{link: LinkStats{RecvErrors: &nine}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	st, _ := m["stats"].(map[string]any)
	if got := st["recv_errors"]; got != 9.0 {
		t.Errorf("recv_errors = %v, want 9 (the firmware's radio counter)", got)
	}
	if got := st["packet_parse_errors"]; got != 4.0 {
		t.Errorf("packet_parse_errors = %v, want 4 (the mux decode count)", got)
	}
}

// battery_percent was fabricated (defaulting to 100 for a board with no
// battery); canonical is stats.battery_mv, and noise_floor lives in stats.
func TestStatusUsesCanonicalStatsShape(t *testing.T) {
	ds := DeviceStats{BatteryMV: 3700, HaveBattery: true, NoiseFloor: -95}
	b, err := formatStatus("up", "n", "id", RadioInfo{}, ds, PacketCounts{}, 0, linkHealth{lastSNR: 4.25, lastRSSI: -80, rxAirMs: 5000})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["battery_percent"]; ok {
		t.Error("battery_percent is not in the canonical schema and was fabricated")
	}
	if _, ok := m["noise_floor"]; ok {
		t.Error("noise_floor belongs inside stats (stats-radio), not at top level")
	}
	st, _ := m["stats"].(map[string]any)
	for k, want := range map[string]any{
		"battery_mv": 3700.0, "noise_floor": -95.0,
		"last_snr": 4.25, "last_rssi": -80.0, "rx_air_secs": 5.0,
	} {
		if got := st[k]; got != want {
			t.Errorf("stats.%s = %v, want %v", k, got, want)
		}
	}
}

// The bridge forwards path only on RX rows with a direct route, which is a
// tighter gate than the firmware's own payload-type check.
func TestPathPublishedOnlyOnRxDirect(t *testing.T) {
	radio := RadioInfo{FreqHz: 917375000, BwHz: 62500, SF: 7, CR: 8}
	mk := func(route byte) *meshcore.Packet {
		return &meshcore.Packet{
			Header:        meshcore.MakeHeader(route, meshcore.PayloadTypeTxtMsg, 0),
			Payload:       []byte{0xAB, 0xCD},
			HasSignalInfo: true,
		}
	}
	if got := decodePacket(t, mk(meshcore.RouteTypeDirect), []byte{1, 2, 3}, "rx", radio)["path"]; got != "CD -> AB" {
		t.Errorf("rx direct: path = %v, want %q", got, "CD -> AB")
	}
	if _, ok := decodePacket(t, mk(meshcore.RouteTypeFlood), []byte{1, 2, 3}, "rx", radio)["path"]; ok {
		t.Error("rx flood published a path; the bridge forwards it only on direct routes")
	}
	if _, ok := decodePacket(t, mk(meshcore.RouteTypeDirect), []byte{1, 2, 3}, "tx", radio)["path"]; ok {
		t.Error("tx row published a path; the bridge forwards it only on rx")
	}
}

// An rx packet whose signal frame never paired carries SNR/RSSI zero values,
// not measurements. Publishing them would report a real 0 dB / 0 dBm.
func TestRxWithoutSignalInfoOmitsMeasurements(t *testing.T) {
	radio := RadioInfo{FreqHz: 917375000, BwHz: 62500, SF: 7, CR: 8}
	pkt := &meshcore.Packet{
		Header:        meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypeTxtMsg, 0),
		Payload:       []byte{0xAB, 0xCD},
		HasSignalInfo: false,
	}
	m := decodePacket(t, pkt, []byte{1, 2, 3}, "rx", radio)
	for _, k := range []string{"SNR", "RSSI", "score"} {
		if v, ok := m[k]; ok {
			t.Errorf("published %s = %v with no signal info; that is a zero value, not a measurement", k, v)
		}
	}
	// Frame-derived fields survive: they never depended on the signal frame.
	if _, ok := m["duration"]; !ok {
		t.Error("duration dropped; it is frame-derived and always valid")
	}
	if m["path"] != "CD -> AB" {
		t.Errorf("path = %v; it is frame-derived and always valid", m["path"])
	}
}

// The two vocabularies must never disagree: they are the same counter.
func TestPacketCountAliasesAgree(t *testing.T) {
	b, err := formatStatus("up", "n", "id", RadioInfo{}, DeviceStats{},
		PacketCounts{Received: 11}, 0, linkHealth{tx: node.TxStats{Sent: 22}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	st, _ := m["stats"].(map[string]any)
	if st["recv"] != 11.0 || st["packets_recv"] != 11.0 {
		t.Errorf("recv/packets_recv = %v/%v, want 11 both", st["recv"], st["packets_recv"])
	}
	if st["sent"] != 22.0 || st["packets_sent"] != 22.0 {
		t.Errorf("sent/packets_sent = %v/%v, want 22 both", st["sent"], st["packets_sent"])
	}
	// packets_received was never a key any consumer read.
	if _, ok := st["packets_received"]; ok {
		t.Error("packets_received matched nothing on any consumer; do not publish it")
	}
}

// A packet we transmit that we already published as rx carries the SAME hash.
// One shared DedupCache would drop every tx row as a duplicate; separate
// caches must let it through while still deduping within each direction.
func TestDedupIsPerDirection(t *testing.T) {
	pkt := &meshcore.Packet{
		Header:  meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeTxtMsg, 0),
		Payload: []byte{0xAB, 0xCD},
	}
	bc := &brokerClient{dedupRx: &meshcore.DedupCache{}, dedupTx: &meshcore.DedupCache{}}

	// Exercise the real selection, not two caches picked by hand.
	if bc.dedupFor("rx").HasSeen(pkt) {
		t.Fatal("first rx should not be a dup")
	}
	// Same packet, other direction: must not be suppressed by the rx sighting.
	if bc.dedupFor("tx").HasSeen(pkt) {
		t.Error("tx suppressed by an rx sighting of the same hash; that drops every such tx row")
	}
	// ...but a repeat within a direction still dedups.
	if !bc.dedupFor("tx").HasSeen(pkt) {
		t.Error("second tx of the same packet was not deduped")
	}
	if bc.dedupFor("tx") == bc.dedupFor("rx") {
		t.Error("both directions resolved to the same cache")
	}
}

// The tx sink is an indirection because AddOutboundHandler has no removal:
// reloads must retarget the single registered handler, never stack new ones.
func TestTxSinkRetargetsAndClears(t *testing.T) {
	ms := &modemState{}
	if f := ms.txSink.Load(); f != nil {
		t.Fatal("sink set before any observer started")
	}
	var got string
	ms.setTxSink(func([]byte) { got = "first" })
	ms.setTxSink(func([]byte) { got = "second" })
	(*ms.txSink.Load())(nil)
	if got != "second" {
		t.Errorf("sink dispatched to %q; a reload must retarget, not stack", got)
	}
	ms.clearTxSink()
	if f := ms.txSink.Load(); f != nil {
		t.Error("sink survived stopObservers; it would point at a dead observer")
	}
}

// The selector being right is not enough: the CALL SITE must pass the actual
// direction. Hardcoding bc.dedupFor("rx") inside publishPacket leaves every
// unit test green while dropping every tx row, so exercise the real path and
// count what actually reaches the publish queue.
func TestPublishPacketDedupsPerDirectionEndToEnd(t *testing.T) {
	bc := &brokerClient{
		publishCh: make(chan publishJob, 8), // no worker draining: jobs accumulate
		stop:      make(chan struct{}),
		dedupRx:   &meshcore.DedupCache{},
		dedupTx:   &meshcore.DedupCache{},
	}
	o := &MqttObserver{
		originName: "n",
		pubKeyHx:   "id",
		brokers:    []*brokerClient{bc},
		log:        slog.Default(),
	}
	pkt := &meshcore.Packet{
		Header:        meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeTxtMsg, 0),
		Payload:       []byte{0xAB, 0xCD},
		HasSignalInfo: true,
	}
	raw := []byte{1, 2, 3}

	o.publishPacket(pkt, raw, "rx")
	if got := len(bc.publishCh); got != 1 {
		t.Fatalf("after rx: %d queued, want 1", got)
	}
	// Same packet, same hash, other direction: must still publish.
	o.publishPacket(pkt, raw, "tx")
	if got := len(bc.publishCh); got != 2 {
		t.Fatalf("after tx: %d queued, want 2 -- the tx row was dropped as a dup of the rx", got)
	}
	// Repeats within each direction are still deduped.
	o.publishPacket(pkt, raw, "rx")
	o.publishPacket(pkt, raw, "tx")
	if got := len(bc.publishCh); got != 2 {
		t.Errorf("after repeats: %d queued, want 2 -- dedup stopped working within a direction", got)
	}
}

// repeat is read at the TOP LEVEL, not inside stats. Pinning both halves so a
// tidy-up that folds it into statsBlock fails loudly instead of silently
// hiding the field from every consumer.
func TestRepeatIsTopLevelNotInStats(t *testing.T) {
	b, err := formatStatus("up", "n", "id", RadioInfo{}, DeviceStats{}, PacketCounts{}, 0, linkHealth{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["repeat"]; !ok {
		t.Error("repeat missing from the top level, where consumers read it")
	}
	if st, _ := m["stats"].(map[string]any); st != nil {
		if _, ok := st["repeat"]; ok {
			t.Error("repeat also published inside stats; it belongs only at the top level")
		}
	}
}

// slowStats mimics kissStatsProvider: Stats() blocks while polling the board,
// and a packet arriving during that window must be reflected in the SAME
// message, not the next one. Sampling signal counters before the poll published
// "recv: 2" beside "last_rssi: 0" against real hardware.
type slowStats struct{ during func() }

func (s slowStats) RadioConfig() RadioInfo { return RadioInfo{} }
func (s slowStats) LinkStats() LinkStats   { return LinkStats{} }
func (s slowStats) Stats(context.Context) DeviceStats {
	s.during() // a packet lands mid-poll
	return DeviceStats{}
}

func TestStatusSamplesAllCountersAfterTheBoardPoll(t *testing.T) {
	bc := &brokerClient{
		publishCh: make(chan publishJob, 4),
		stop:      make(chan struct{}),
	}
	o := &MqttObserver{
		originName: "n", pubKeyHx: "id",
		brokers: []*brokerClient{bc},
		log:     slog.Default(),
		mux:     node.NewRadioMux(nopModem{}),
	}
	o.radio = o.mux.NewRadio()
	o.parseErrors = &atomic.Uint64{}
	o.stats = slowStats{during: func() {
		o.packetsReceived.Add(1)
		o.lastRSSI.Store(-12)
		o.lastSNRBits.Store(math.Float64bits(11))
	}}

	o.publishStatus(context.Background(), bc, "online")

	var m struct {
		Stats struct {
			Recv     uint64  `json:"recv"`
			LastRSSI int16   `json:"last_rssi"`
			LastSNR  float64 `json:"last_snr"`
		} `json:"stats"`
	}
	// publishStatus enqueues; read the job off the queue.
	job := <-bc.publishCh
	if err := json.Unmarshal(job.payload, &m); err != nil {
		t.Fatal(err)
	}
	if m.Stats.Recv != 1 {
		t.Fatalf("recv = %d, want 1", m.Stats.Recv)
	}
	if m.Stats.LastRSSI != -12 || m.Stats.LastSNR != 11 {
		t.Errorf("recv=%d but last_rssi=%d last_snr=%v: counters sampled at different instants",
			m.Stats.Recv, m.Stats.LastRSSI, m.Stats.LastSNR)
	}
}

type nopModem struct{}

func (nopModem) SendData([]byte) error                            { return nil }
func (nopModem) SetDataHandler(func([]byte, float32, int8, bool)) {}
func (nopModem) AddOutboundHandler(func([]byte))                  {}

// waitLoopsDone blocks until no connectAndRetry loop owns any of these brokers.
// A retry goroutine must never outlive its test: it survives inside
// connectBroker for the full connectWaitTimeout, and anything a later test does
// to shared state races it across test boundaries.
func waitLoopsDone(t *testing.T, bcs ...*brokerClient) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for _, bc := range bcs {
		for bc.retrying.Load() {
			if time.Now().After(deadline) {
				t.Fatal("retry loop still running at test end")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// blackhole accepts TCP and never answers, which is what a firewalled or wedged
// broker looks like. A REFUSED port is useless here: it fails instantly, so the
// test would pass against the very code it exists to catch.
func blackhole(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(done)
				return
			}
			go func() { <-done; c.Close() }() // hold it open, never write
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// Start must not wait on connections. Dialling inline cost connectWaitTimeout
// per unreachable broker, serially, before any broker could publish.
func TestStartDoesNotBlockOnUnreachableBrokers(t *testing.T) {
	addr, closeLn := blackhole(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	brokers := make([]BrokerConfig, 3)
	for i := range brokers {
		brokers[i] = BrokerConfig{
			Name: fmt.Sprintf("dead-%d", i), Enabled: true,
			Transport: "tcp", Host: host, Port: port, AuthType: "none",
		}
	}
	name, iata := "t", "test"
	mux := node.NewRadioMux(nopModem{})
	o, err := NewMqttObserver(
		MqttConfig{Name: &name, IataCode: &iata, Brokers: brokers},
		mux, identityFromName("test-observer"),
		&modemState{parseErrors: &atomic.Uint64{}},
	)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := o.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// Order matters: drop the listener so in-flight dials fail fast, THEN stop,
	// THEN wait for the loops. Deferring these runs them in the wrong order.
	defer func() {
		closeLn()
		o.Stop()
		waitLoopsDone(t, o.brokersSnapshot()...)
	}()

	// Inline dialling would be 3 x connectWaitTimeout = 30s.
	if elapsed > 2*time.Second {
		t.Errorf("Start took %v with 3 unreachable brokers; it is dialling on the startup path", elapsed)
	}
	// Register-first: an unreachable broker must still be held, or nothing
	// retries it and it is invisible to any status view.
	if n := len(o.brokersSnapshot()); n != 3 {
		t.Errorf("registered %d brokers, want 3; an unreachable broker was dropped", n)
	}
	for _, bc := range o.brokersSnapshot() {
		if bc.currentClient() != nil {
			t.Error("broker holds a client it never connected")
		}
	}
}

// Status must go through the publish queue, not inline. A bare token.Wait()
// here was unbounded: Stop passes a 5s context that never reaches the token, so
// an unresponsive broker hung Stop, SIGHUP reload and shutdown with it.
func TestStatusIsQueuedWithItsOwnQoSAndRetain(t *testing.T) {
	bc := &brokerClient{
		cfg:       BrokerConfig{RetainStatus: true},
		publishCh: make(chan publishJob, 4),
		stop:      make(chan struct{}),
	}
	o := &MqttObserver{originName: "n", pubKeyHx: "id", log: slog.Default(),
		brokers: []*brokerClient{bc}, parseErrors: &atomic.Uint64{},
		mux: node.NewRadioMux(nopModem{})}
	o.radio = o.mux.NewRadio()

	done := make(chan struct{})
	go func() { o.publishStatus(context.Background(), bc, "online"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishStatus blocked; it is still publishing inline with an unbounded Wait")
	}

	var job publishJob
	select {
	case job = <-bc.publishCh:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing queued; status is still being published inline")
	}
	if job.qos != 1 {
		t.Errorf("status qos = %d, want 1", job.qos)
	}
	if !job.retain {
		t.Error("status retain = false; it must follow BrokerConfig.RetainStatus")
	}

	// Packets keep QoS 0 / no retain -- the new fields must not change them.
	pkt := &meshcore.Packet{Header: meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeAdvert, 0), HasSignalInfo: true}
	o.publishPacket(pkt, []byte{1, 2, 3}, "rx")
	var pj publishJob
	select {
	case pj = <-bc.publishCh:
	case <-time.After(2 * time.Second):
		t.Fatal("packet was not queued")
	}
	if pj.qos != 0 || pj.retain {
		t.Errorf("packet qos/retain = %d/%v, want 0/false", pj.qos, pj.retain)
	}
}

// Closing the token-refresh gap created a SECOND spawner of connectAndRetry
// (Start, and a failed refresh). Without the CAS guard two loops race on one
// client pointer and both call swapClient.
func TestConnectAndRetryRunsOneLoopPerBroker(t *testing.T) {
	addr, closeLn := blackhole(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	bc := &brokerClient{
		cfg:  BrokerConfig{Name: "b", Transport: "tcp", Host: host, Port: port, AuthType: "none"},
		stop: make(chan struct{}),
	}
	o := &MqttObserver{log: slog.Default(), pubKeyHx: strings.Repeat("A", 64)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running sync.WaitGroup
	for i := 0; i < 5; i++ {
		running.Add(1)
		go func() { defer running.Done(); o.connectAndRetry(ctx, bc) }()
	}
	// One loop takes ownership; the other four must return immediately.
	deadline := time.After(3 * time.Second)
	for !bc.retrying.Load() {
		select {
		case <-deadline:
			t.Fatal("no loop took ownership")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	closeLn()
	close(bc.stop)
	done := make(chan struct{})
	go func() { running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("loops did not exit; more than one was running")
	}
	if bc.retrying.Load() {
		t.Error("retrying still set after every loop returned")
	}
}

// The refresh path can spawn this loop on a broker that already has a healthy
// client, and paho's own auto-reconnect may have restored it first.
func TestConnectAndRetryDoesNotReplaceAHealthyClient(t *testing.T) {
	bc := &brokerClient{
		cfg:    BrokerConfig{Name: "b", Host: "127.0.0.1", Port: 1, AuthType: "none"},
		stop:   make(chan struct{}),
		client: connectedClient{},
	}
	o := &MqttObserver{log: slog.Default(), pubKeyHx: strings.Repeat("A", 64)}

	done := make(chan struct{})
	go func() { o.connectAndRetry(context.Background(), bc); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop dialled despite a healthy client; it would replace a working connection")
	}
	if _, ok := bc.currentClient().(connectedClient); !ok {
		t.Error("healthy client was replaced")
	}
}

type connectedClient struct{ mqtt.Client }

func (connectedClient) IsConnected() bool { return true }

// The refresh-failure retry is INERT unless the stale client is dropped first:
// connectAndRetry refuses to replace a client reporting connected, and a
// just-failed refresh leaves exactly such a client (its token has ~2min left).
// This asserts the loop actually dials rather than returning immediately.
func TestRefreshFailureDropsStaleClientSoRetryDials(t *testing.T) {
	addr, closeLn := blackhole(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	bc := &brokerClient{
		cfg:    BrokerConfig{Name: "b", Transport: "tcp", Host: host, Port: port, AuthType: "none"},
		stop:   make(chan struct{}),
		client: connectedClient{}, // stale: still reports connected
	}
	o := &MqttObserver{log: slog.Default(), pubKeyHx: strings.Repeat("A", 64)}

	// What the refresh-failure path does before spawning the retry.
	if stale := bc.currentClient(); stale != nil {
		bc.swapClient(nil)
	}

	dialing := make(chan struct{})
	go func() {
		for i := 0; i < 400; i++ {
			if bc.retrying.Load() {
				close(dialing)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	go o.connectAndRetry(context.Background(), bc)

	select {
	case <-dialing:
	case <-time.After(3 * time.Second):
		t.Fatal("retry never took ownership; the stale client made it return immediately")
	}
	closeLn()
	close(bc.stop)
	waitLoopsDone(t, bc)
}

// fakeLinkClient is connected, reconnecting or down on demand.
type fakeLinkClient struct {
	mqtt.Client
	up           bool
	reconnecting bool // paho with AutoReconnect: IsConnected true, connection not open
}

func (f *fakeLinkClient) IsConnected() bool      { return f.up || f.reconnecting }
func (f *fakeLinkClient) IsConnectionOpen() bool { return f.up }
func (f *fakeLinkClient) Publish(string, byte, bool, any) mqtt.Token {
	return &mqtt.DummyToken{}
}

// Packets heard while a broker is down are dropped, not buffered. That must be
// visible: one warning when the dropping starts, and the count once publishing
// resumes, not a line per packet.
func TestDoPublishLogsDropsWhileBrokerDown(t *testing.T) {
	var logs strings.Builder
	o := &MqttObserver{log: slog.New(slog.NewTextHandler(&logs, nil))}
	fc := &fakeLinkClient{}
	bc := &brokerClient{cfg: BrokerConfig{Name: "b"}}
	bc.swapClient(fc)

	for range 3 {
		o.doPublish(bc, publishJob{topic: "t"})
	}
	if n := strings.Count(logs.String(), "dropping packets"); n != 1 {
		t.Fatalf("%d drop warnings for one outage, want 1:\n%s", n, logs.String())
	}

	fc.up = true
	o.doPublish(bc, publishJob{topic: "t"})
	o.doPublish(bc, publishJob{topic: "t"})
	if !strings.Contains(logs.String(), "broker publishing again") || !strings.Contains(logs.String(), "dropped=3") {
		t.Fatalf("recovery should report the 3 dropped packets once:\n%s", logs.String())
	}
	if n := strings.Count(logs.String(), "publishing again"); n != 1 {
		t.Errorf("%d recovery lines, want 1", n)
	}

	// An outage mid-run looks like this to paho: still "connected" while it
	// reconnects, and a QoS 0 publish then vanishes. It must warn again.
	fc.up, fc.reconnecting = false, true
	o.doPublish(bc, publishJob{topic: "t"})
	if n := strings.Count(logs.String(), "dropping packets"); n != 2 {
		t.Errorf("%d drop warnings after a mid-run outage, want 2", n)
	}
}

// The first connect publishes its own online; paho calls the handler for it
// too. Every later connect is an auto-reconnect and must say online again.
func TestOnReconnectSkipsFirstConnect(t *testing.T) {
	n := 0
	h := onReconnect(func() { n++ })
	h(nil)
	if n != 0 {
		t.Fatalf("first connect fired the reconnect action (%d)", n)
	}
	h(nil)
	h(nil)
	if n != 2 {
		t.Errorf("two reconnects fired it %d times, want 2", n)
	}
}
