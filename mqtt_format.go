package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/hardware"
	"github.com/meshcore-go/meshcore-go/node"
)

// rxTimeLayout is an RFC3339-style timestamp with microsecond precision; the
// trailing Z07:00 emits "Z" for UTC so values are unambiguously zone-aware.
// Always format a UTC time (time.Now().UTC()) with it.
const rxTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

type packetMessage struct {
	Timestamp  string `json:"timestamp"`
	OriginID   string `json:"origin_id"`
	Origin     string `json:"origin"`
	Type       string `json:"type"`
	Direction  string `json:"direction"`
	Time       string `json:"time"`
	Date       string `json:"date"`
	Len        string `json:"len"`
	PacketType string `json:"packet_type"`
	Route      string `json:"route"`
	PayloadLen string `json:"payload_len"`
	Raw        string `json:"raw"`
	Hash       string `json:"hash"`

	// RX-only. The firmware logs SNR/RSSI/score/time on its RX line only
	// (Dispatcher.cpp:222); emitting them on a TX row would report a measured
	// 0 dB for our own transmission. Omitted rather than zeroed.
	SNR      string `json:"SNR,omitempty"`
	RSSI     string `json:"RSSI,omitempty"`
	Score    string `json:"score,omitempty"`
	Duration string `json:"duration,omitempty"`

	// Path is NOT a route: it reproduces the firmware's "[%02X -> %02X]"
	// trailer, which is payload[1] -> payload[0] -- src hash then dest hash
	// (Mesh.cpp:135-136 reads dest from payload[0], src from payload[1]), so
	// the arrow reads src -> dest. Printed only for PATH/REQ/RESPONSE/TXT_MSG,
	// and forwarded by the bridge only on RX direct rows (both forks agree).
	Path string `json:"path,omitempty"`
}

// pathTrailer mirrors Dispatcher.cpp:231-236: only these four payload types
// carry the "[src -> dst]" trailer, and it is the first two payload bytes.
// Brackets are literal in the bridge's regex, so the published value excludes
// them. The caller applies its RX-only and direct-only gates
// (Cisien/meshcoretomqtt bridge/message_parser.py; the Andrew-a-g fork
// gates it identically).
func pathTrailer(pkt *meshcore.Packet) string {
	switch pkt.PayloadType() {
	case meshcore.PayloadTypePath, meshcore.PayloadTypeReq,
		meshcore.PayloadTypeResponse, meshcore.PayloadTypeTxtMsg:
	default:
		return ""
	}
	if len(pkt.Payload) < 2 {
		return ""
	}
	return fmt.Sprintf("%02X -> %02X", pkt.Payload[1], pkt.Payload[0])
}

func formatPacket(pkt *meshcore.Packet, rawBytes []byte, originName, originID, direction string, radio RadioInfo) ([]byte, error) {
	now := time.Now().UTC()

	route := "F"
	if pkt.IsRouteDirect() {
		route = "D"
	}

	hash := pkt.PacketHash()

	msg := packetMessage{
		Timestamp:  now.Format(rxTimeLayout),
		OriginID:   originID,
		Origin:     originName,
		Type:       "PACKET",
		Direction:  direction,
		Time:       now.Format("15:04:05"),
		Date:       fmt.Sprintf("%d/%d/%d", now.Day(), int(now.Month()), now.Year()),
		Len:        fmt.Sprintf("%d", len(rawBytes)),
		PacketType: fmt.Sprintf("%d", pkt.PayloadType()),
		Route:      route,
		PayloadLen: fmt.Sprintf("%d", len(pkt.Payload)),
		Raw:        strings.ToUpper(hex.EncodeToString(rawBytes)),
		Hash:       strings.ToUpper(hex.EncodeToString(hash[:])),
	}

	if direction == "rx" {
		// The bridge gates path on RX and on a direct route, not just on
		// payload type the firmware gates its own printing on.
		if pkt.IsRouteDirect() {
			msg.Path = pathTrailer(pkt)
		}
		// Firmware prints (int)pkt->getSNR(), a C cast that truncates toward
		// zero; Go's float->int conversion truncates identically. Not "%.0f",
		// which rounds. The upstream regex is SNR=(-?\d+): no decimal survives.
		// Duration is RX-only, matching both the firmware's TX log line
		// (Dispatcher.cpp:343, which prints no time=) and Cisien/meshcoretomqtt,
		// which sets it inside its rx-only block. NOTE: the Andrew-a-g fork has
		// no duration field at all. Only hash is a tx extension.
		msg.Duration = fmt.Sprintf("%d", radio.airtimeMs(len(rawBytes)))
		// SNR and RSSI are measurements: signal info arrives in a separate
		// KISS frame paired to the data frame, and when that pairing fails
		// (ModemStats.RxMetaTimeouts counts it) both are zero values, not
		// measurements. Score is computed FROM the SNR, so it is only as
		// valid as the SNR is.
		if !pkt.HasSignalInfo {
			return json.Marshal(msg)
		}
		msg.SNR = fmt.Sprintf("%d", int(pkt.SNR))
		msg.RSSI = fmt.Sprintf("%d", pkt.RSSI)
		score := hardware.PacketScore(float64(pkt.SNR), radio.SF, len(rawBytes))
		msg.Score = fmt.Sprintf("%d", int(score*1000))
	}

	return json.Marshal(msg)
}

type statsBlock struct {
	// stats-core (StatsFormatHelper::formatCoreStats). Battery is millivolts:
	// there is no percentage anywhere in the canonical schema. Omitted when the
	// board cannot measure one (a Pi on the SPI path has no battery), rather
	// than publishing a 0 that reads as a flat cell.
	BatteryMV  *uint16 `json:"battery_mv,omitempty"`
	UptimeSecs uint32  `json:"uptime_secs"`

	// stats-radio (formatRadioStats). last_snr/last_rssi are the most recently
	// received packet's values; firmware prints last_snr as %.2f.
	NoiseFloor int16   `json:"noise_floor"`
	LastRSSI   int16   `json:"last_rssi"`
	LastSNR    float64 `json:"last_snr"`
	RxAirSecs  uint32  `json:"rx_air_secs"`
	TxAirSecs  uint32  `json:"tx_air_secs"`

	// stats-packets (formatPacketStats).
	// The same two values under two vocabularies, deliberately. recv/sent are
	// the firmware's stats-packets names; packets_recv/packets_sent are what
	// CoreScope's observer-status ingest reads (Kpa-clawbot/CoreScope on
	// master, cmd/ingestor/main.go:1320-1332, via a single-key lookup with no
	// fallback list), and are NOT interchangeable. packets_received, our old
	// name, matched nothing anywhere. Aliases must always agree.
	PacketsReceived uint64 `json:"recv"`
	PacketsSent     uint64 `json:"sent"`
	PacketsRecvAlt  uint64 `json:"packets_recv"`
	PacketsSentAlt  uint64 `json:"packets_sent"`
	FloodRx         uint64 `json:"flood_rx"`
	FloodTx         uint64 `json:"flood_tx"`
	DirectTx        uint64 `json:"direct_tx"`
	DirectRx        uint64 `json:"direct_rx"`
	FloodDups       uint64 `json:"flood_dups"`
	DirectDups      uint64 `json:"direct_dups"`
	// Two different failures, deliberately two keys. recv_errors is the RADIO
	// driver's failure-to-receive count, polled from the modem firmware, and is
	// the same measurement firmware nodes publish under this key through the
	// meshcoretomqtt bridge. packet_parse_errors is ours: bytes that arrived
	// intact and did not decode as a MeshCore packet, an application-layer
	// failure. Until v1.2.0 recv_errors carried the parse count, which made the
	// two publishers' values incomparable under one name.
	RecvErrors        uint64   `json:"recv_errors"`
	PacketParseErrors uint64   `json:"packet_parse_errors"`
	MCUTempC          *float64 `json:"mcu_temp_c,omitempty"`

	// One RadioMux is shared by every bot and the observer, so the fields
	// below are PROCESS-wide, not per-node. "sent" is everything this process
	// transmitted, not everything this observer sent.
	QueueLen       int    `json:"queue_len"`
	TxRequeued     uint64 `json:"tx_requeued"`
	TxDroppedBusy  uint64 `json:"tx_dropped_busy"`  // radio stayed busy; RF congestion, not locally fixable
	TxDroppedQueue uint64 `json:"tx_dropped_queue"` // enqueued faster than the radio drains; send less
	TxFailed       uint64 `json:"tx_failed"`

	RxDropped           uint64 `json:"rx_dropped"`      // inbound frames discarded on buffer overflow
	HwErrors            uint64 `json:"hw_errors"`       // HW_RESP_ERROR frames from the firmware
	HandlerSlow         uint64 `json:"handler_slow"`    // dispatches over the watchdog; RX runs serially, so these stall everyone
	TxOutcomeLost       uint64 `json:"tx_outcome_lost"` // TX_DONE waits abandoned by a reconnect: sent or not is unknown
	RxMetaMisattributed uint64 `json:"rx_meta_misattributed"`
	RxMetaTimeouts      uint64 `json:"rx_meta_timeouts"`
	HwDecodeErrors      uint64 `json:"hw_decode_errors"`

	// Counters that only exist below the firmware, so they are OMITTED on a
	// KISS modem rather than published as zeros it never measured. Same key
	// names as OwlShack: this block is a schema shared across both repos.
	CRCErrors      *uint64 `json:"crc_errors,omitempty"`      // chip CRC/header errors: a noisy channel, not a fault
	DriverErrors   *uint64 `json:"driver_errors,omitempty"`   // SPI transaction failures, busy timeouts, failed IRQ reads
	RecvRecoveries *uint64 `json:"recv_recoveries,omitempty"` // watchdog re-arming a stuck receiver
}

// u64 flattens a counter this transport cannot measure to 0, for the keys that
// have always been on the wire: consumers may key off them, so they stay present
// on every transport rather than appearing and disappearing with the modem type.
func u64(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

// linkHealth is the process-wide radio and modem health published in statsBlock.
// Bundled because these three always travel together to formatStatus.
type linkHealth struct {
	tx       node.TxStats
	queueLen int
	link     LinkStats

	// Most recent received packet's signal, and cumulative RX airtime. The
	// firmware accumulates rx_air_time from the same per-packet estimate
	// (Dispatcher.cpp:209), so this is faithful rather than a stand-in.
	lastSNR  float64
	lastRSSI int8
	rxAirMs  uint64
	txAirMs  uint64
}

type statusMessage struct {
	Status          string `json:"status"`
	Timestamp       string `json:"timestamp"`
	Origin          string `json:"origin"`
	OriginID        string `json:"origin_id"`
	Model           string `json:"model"`
	FirmwareVersion string `json:"firmware_version"`
	Radio           string `json:"radio"`
	ClientVersion   string `json:"client_version"`

	// Repeat is whether this node relays traffic. Structurally false here: the
	// bot never sets an allow-forward handler, and node's canForward returns
	// false when that handler is nil (router.go:249-256). A consumer excluding
	// non-relaying nodes from path-hop disambiguation is correct to skip us.
	Repeat bool       `json:"repeat"`
	Stats  statsBlock `json:"stats"`
}

type PacketCounts struct {
	Received   uint64
	FloodTx    uint64
	DirectTx   uint64
	FloodRx    uint64
	DirectRx   uint64
	FloodDups  uint64
	DirectDups uint64
}

func formatStatus(status, originName, originID string, radio RadioInfo, ds DeviceStats, packets PacketCounts, parseErrors uint64, health linkHealth) ([]byte, error) {
	var radioStr string
	if radio.FreqHz > 0 {
		radioStr = fmt.Sprintf("%.3f,%.1f,%d,%d",
			float64(radio.FreqHz)/1_000_000,
			float64(radio.BwHz)/1_000,
			radio.SF,
			radio.CR,
		)
	}

	var mcuTemp *float64
	if ds.HaveMCUTemp {
		mcuTemp = &ds.MCUTempC
	}
	var batteryMV *uint16
	if ds.HaveBattery {
		batteryMV = &ds.BatteryMV
	}

	msg := statusMessage{
		Status:          status,
		Timestamp:       time.Now().UTC().Format(rxTimeLayout),
		Origin:          originName,
		OriginID:        originID,
		Model:           "meshcore-bot",
		FirmwareVersion: version,
		Radio:           radioStr,
		ClientVersion:   "meshcore-bot/" + version,
		Stats: statsBlock{
			BatteryMV:  batteryMV,
			UptimeSecs: ds.UptimeSecs,

			NoiseFloor: ds.NoiseFloor,
			LastRSSI:   int16(health.lastRSSI),
			LastSNR:    health.lastSNR,
			RxAirSecs:  uint32(health.rxAirMs / 1000),
			TxAirSecs:  uint32(health.txAirMs / 1000),

			PacketsReceived:   packets.Received,
			PacketsRecvAlt:    packets.Received,
			PacketsSentAlt:    health.tx.Sent,
			FloodRx:           packets.FloodRx,
			FloodTx:           packets.FloodTx,
			DirectTx:          packets.DirectTx,
			DirectRx:          packets.DirectRx,
			FloodDups:         packets.FloodDups,
			DirectDups:        packets.DirectDups,
			RecvErrors:        u64(health.link.RecvErrors),
			PacketParseErrors: parseErrors,
			MCUTempC:          mcuTemp,

			PacketsSent:    health.tx.Sent,
			QueueLen:       health.queueLen,
			TxRequeued:     health.tx.BusyRequeued,
			TxDroppedBusy:  health.tx.BusyDropped,
			TxDroppedQueue: health.tx.QueueRejected,
			TxFailed:       health.tx.Failed,

			RxDropped:           u64(health.link.InboundDroppedOldest) + health.link.InboundDroppedNew,
			HwErrors:            u64(health.link.HwErrors),
			HandlerSlow:         health.link.HandlerSlow,
			TxOutcomeLost:       u64(health.link.TxOutcomeLost),
			RxMetaMisattributed: u64(health.link.RxMetaMisattributed),
			RxMetaTimeouts:      u64(health.link.RxMetaTimeouts),
			HwDecodeErrors:      u64(health.link.HwDecodeErrors),

			CRCErrors:      health.link.CRCErrors,
			DriverErrors:   health.link.DriverErrors,
			RecvRecoveries: health.link.RecvRecoveries,
		},
	}
	return json.Marshal(msg)
}
