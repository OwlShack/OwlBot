package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type brokerClient struct {
	cfg    BrokerConfig
	mu     sync.Mutex // guards client (swapped by tokenRefreshLoop while the publish worker uses it)
	client mqtt.Client

	pubKeyHx    string
	iata        string
	packetTopic string
	statusTopic string

	disallowed map[byte]bool
	// One cache per direction. A packet we transmit that we already published
	// as rx carries the SAME hash, so a single shared cache would drop every tx
	// row as a duplicate. Each still dedups within its own direction.
	dedupRx *meshcore.DedupCache // nil when dedup disabled for this broker
	dedupTx *meshcore.DedupCache

	publishCh  chan publishJob
	stop       chan struct{} // closed by Stop to halt the worker; publishCh is never closed, so an in-flight send can't panic
	workerDone chan struct{}
	dropped    atomic.Uint64
	// downDropped counts packets dropped since the broker went unreachable.
	// Only publishWorker touches it.
	downDropped uint64
	// retrying is true while a connectAndRetry loop owns this broker. Needed
	// now that BOTH Start and a failed token refresh can spawn one: without it
	// two loops race on the same client pointer and both swapClient.
	retrying atomic.Bool
}

func (b *brokerClient) currentClient() mqtt.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client
}

// swapClient replaces the live client. It deliberately does NOT return the old
// one: returning it made "old.Disconnect()" the natural thing to write, and old
// is nil for a broker that never connected. Callers read it first and nil-check.
func (b *brokerClient) swapClient(c mqtt.Client) {
	b.mu.Lock()
	b.client = c
	b.mu.Unlock()
}

// publishQueueDepth is the per-broker job buffer, absorbing bursts so the
// modem RX path never blocks on a slow broker.
const publishQueueDepth = 256

// publishWaitTimeout bounds how long the worker waits for paho's Publish token.
const publishWaitTimeout = 5 * time.Second

// connectWaitTimeout bounds the initial connect handshake so an unreachable
// broker can't hang Start/reload/token-refresh on the OS TCP timeout.
const connectWaitTimeout = 10 * time.Second

// Backoff bounds for reconnecting a broker that was unreachable at startup.
const (
	connectRetryMin = 2 * time.Second
	connectRetryMax = 5 * time.Minute
)

type publishJob struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

func (b *brokerClient) isAllowed(payloadType byte) bool {
	return !b.disallowed[payloadType]
}

type MqttObserver struct {
	radio node.MuxRadio
	mux   *node.RadioMux
	ms    *modemState
	id    meshcore.LocalIdentity
	stats StatsProvider
	log   *slog.Logger

	cfg             MqttConfig
	originName      string
	pubKeyHx        string
	brokers         []*brokerClient
	packetsReceived atomic.Uint64
	floodRx         atomic.Uint64
	directRx        atomic.Uint64
	floodDups       atomic.Uint64
	directDups      atomic.Uint64
	parseErrors     *atomic.Uint64

	// Most recent RX signal and cumulative RX airtime, written on the serial
	// receive goroutine and read by the status ticker.
	lastSNRBits atomic.Uint64
	lastRSSI    atomic.Int32
	rxAirMs     atomic.Uint64
	floodTx     atomic.Uint64
	directTx    atomic.Uint64
	txAirMs     atomic.Uint64

	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewMqttObserver(cfg MqttConfig, mux *node.RadioMux, id meshcore.LocalIdentity, ms *modemState) (*MqttObserver, error) {
	name := "mqtt-observer"
	if cfg.Name != nil && *cfg.Name != "" {
		name = *cfg.Name
	}

	pkHex := publicKeyHex(id)
	radio := mux.NewRadio()

	obs := &MqttObserver{
		radio:       radio,
		mux:         mux,
		id:          id,
		cfg:         cfg,
		ms:          ms,
		stats:       ms.stats,
		parseErrors: ms.parseErrors,
		originName:  name,
		pubKeyHx:    pkHex,
		log:         slog.Default().With("component", "mqtt", "observer", name),
	}

	return obs, nil
}

func (o *MqttObserver) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	o.mu.Lock()
	o.cancel = cancel
	o.mu.Unlock()

	iata := "test"
	if o.cfg.IataCode != nil && *o.cfg.IataCode != "" {
		iata = *o.cfg.IataCode
	}

	// An unnamed advert would broadcast garbage identity info; fail loudly.
	if o.cfg.Advert != nil && o.cfg.Advert.Enabled && (o.cfg.Name == nil || *o.cfg.Name == "") {
		return fmt.Errorf("observer advert is enabled but no name is configured")
	}

	for _, bcfg := range o.cfg.Brokers {
		if !bcfg.Enabled {
			continue
		}

		disallowed := parseDisallowed(bcfg.DisallowedPacketTypes)
		packetTopic, statusTopic := resolveTopics(bcfg, iata, o.pubKeyHx, o.originName)

		bc := &brokerClient{
			cfg:         bcfg,
			pubKeyHx:    o.pubKeyHx,
			iata:        iata,
			packetTopic: packetTopic,
			statusTopic: statusTopic,
			disallowed:  disallowed,
			publishCh:   make(chan publishJob, publishQueueDepth),
			stop:        make(chan struct{}),
			workerDone:  make(chan struct{}),
		}
		if bcfg.Dedup {
			bc.dedupRx = &meshcore.DedupCache{}
			bc.dedupTx = &meshcore.DedupCache{}
		}
		// Worker first, so the queue has a consumer from the moment the broker
		// exists; then register, so an unreachable broker stays visible and
		// something holds a reference to keep retrying; then dial OFF the
		// startup path. Dialling inline cost connectWaitTimeout per dead
		// broker, serially, before any broker could publish.
		go o.publishWorker(bc)
		o.brokers = append(o.brokers, bc)
		go o.connectAndRetry(ctx, bc)
	}

	o.radio.SetPacketFilter(func(_ *meshcore.Packet) bool { return true })
	o.radio.SetRawDataHandler(o.onData)

	if o.ms != nil {
		o.ms.setTxSink(o.onOutbound)
	}

	go o.heartbeatLoop(ctx)
	go o.tokenRefreshLoop(ctx)
	if o.cfg.Advert != nil && o.cfg.Advert.Enabled {
		go o.advertLoop(ctx)
	}

	return nil
}

// connectAndRetry dials bc until it succeeds or the observer stops, then adopts
// the client and publishes the first "online". It owns the entire success path,
// because Start no longer waits for a connection.
func (o *MqttObserver) connectAndRetry(ctx context.Context, bc *brokerClient) {
	if !bc.retrying.CompareAndSwap(false, true) {
		return // a loop already owns this broker
	}
	defer bc.retrying.Store(false)

	delay := time.Duration(0) // first attempt immediate
	firstFailure := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-bc.stop:
			return
		case <-time.After(delay):
		}

		// paho's own auto-reconnect can win this race once a broker has
		// connected at least once, which is reachable now that the refresh
		// path spawns this loop. Don't replace a healthy client.
		if c := bc.currentClient(); c != nil && c.IsConnected() {
			return
		}

		client, err := o.connectBroker(bc.cfg, bc.iata)
		if err == nil {
			// Stop can run while a dial is in flight. Adopting the client then
			// would hand a live connection to a broker nobody will disconnect.
			select {
			case <-bc.stop:
				client.Disconnect(250)
				return
			default:
			}
			// Reachable from the refresh path, where a stale client exists.
			if old := bc.currentClient(); old != nil {
				old.Disconnect(0)
			}
			bc.swapClient(client)
			o.publishStatus(ctx, bc, "online")
			o.log.Info("connected", "broker", bc.cfg.Name)
			return
		}

		// Moving the dial off Start would otherwise demote this to Debug and
		// make an unreachable broker at boot invisible. Loud once, quiet after.
		if firstFailure {
			o.log.Error("broker connect failed, retrying", "broker", bc.cfg.Name, "error", err)
			firstFailure = false
		} else {
			o.log.Debug("broker reconnect failed", "broker", bc.cfg.Name, "error", err)
		}
		// A zero delay doubled is still zero, which busy-loops against a dead
		// broker as fast as connects can fail. Floor it after the first miss.
		delay = min(max(delay*2, connectRetryMin), connectRetryMax)
	}
}

func (o *MqttObserver) Stop() {
	o.mu.Lock()
	if o.cancel != nil {
		o.cancel()
	}
	brokers := o.brokers
	o.mu.Unlock()

	// Detach from the radio mux so no new packets reach onData. A deliver
	// already in-flight is still safe: enqueuePublish selects on stop and
	// publishCh is never closed.
	o.radio.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, bc := range brokers {
		o.publishStatus(ctx, bc, "offline")
		close(bc.stop)
		select {
		case <-bc.workerDone:
		case <-time.After(publishWaitTimeout):
			o.log.Warn("publish worker did not drain in time", "broker", bc.cfg.Name)
		}
		if c := bc.currentClient(); c != nil {
			c.Disconnect(500)
		}
	}
	o.mu.Lock()
	o.brokers = nil
	o.mu.Unlock()
}

// onOutbound publishes a row for every packet this PROCESS transmits. Called
// from the modem's single outbound handler, on the tx engine goroutine.
func (o *MqttObserver) onOutbound(data []byte) {
	pkt, err := meshcore.PacketFromBytes(data)
	if err != nil {
		o.log.Log(context.Background(), LevelTrace, "tx packet parse failed", "error", err)
		return
	}
	if pkt.IsRouteDirect() {
		o.directTx.Add(1)
	} else {
		o.floodTx.Add(1)
	}
	if o.stats != nil {
		o.txAirMs.Add(uint64(o.stats.RadioConfig().airtimeMs(len(data))))
	}
	o.publishPacket(pkt, data, "tx")
}

func (o *MqttObserver) onData(data []byte, snr float32, rssi int8, hasSignalInfo bool) {
	o.log.Log(context.Background(), LevelTrace, "raw radio data",
		"len", len(data), "hex", strings.ToUpper(hex.EncodeToString(data)),
		"snr", snr, "rssi", rssi)

	pkt, err := meshcore.PacketFromBytes(data)
	if err != nil {
		o.log.Log(context.Background(), LevelTrace, "packet parse failed", "error", err)
		return
	}
	pkt.SNR = snr
	pkt.RSSI = rssi
	pkt.HasSignalInfo = hasSignalInfo

	o.packetsReceived.Add(1)
	o.lastSNRBits.Store(math.Float64bits(float64(snr)))
	o.lastRSSI.Store(int32(rssi))
	if o.stats != nil {
		o.rxAirMs.Add(uint64(o.stats.RadioConfig().airtimeMs(len(data))))
	}
	if pkt.IsRouteDirect() {
		o.directRx.Add(1)
	} else {
		o.floodRx.Add(1)
	}
	o.publishPacket(pkt, data, "rx")
}

// dedupFor picks the cache for this direction. A packet we transmit that we
// already published as rx carries the SAME hash, so sharing one cache would
// drop every such tx row as a duplicate.
func (b *brokerClient) dedupFor(direction string) *meshcore.DedupCache {
	if direction == "tx" {
		return b.dedupTx
	}
	return b.dedupRx
}

func (o *MqttObserver) publishPacket(pkt *meshcore.Packet, rawBytes []byte, direction string) {
	o.log.Log(context.Background(), LevelTrace, "new packet accepted",
		"direction", direction, "type", pkt.PayloadType(),
		"payload_len", len(pkt.Payload))

	var radio RadioInfo
	if o.stats != nil {
		radio = o.stats.RadioConfig()
	}
	payload, err := formatPacket(pkt, rawBytes, o.originName, o.pubKeyHx, direction, radio)
	if err != nil {
		o.log.Error("format error", "error", err)
		return
	}

	for _, bc := range o.brokersSnapshot() {
		if !bc.isAllowed(pkt.PayloadType()) {
			o.log.Log(context.Background(), LevelTrace, "packet type filtered",
				"broker", bc.cfg.Name, "type", pkt.PayloadType())
			continue
		}
		dedup := bc.dedupFor(direction)
		if dedup != nil && dedup.HasSeen(pkt) {
			o.log.Log(context.Background(), LevelTrace, "dedup hit, skipping",
				"broker", bc.cfg.Name, "type", pkt.PayloadType())
			if pkt.IsRouteDirect() {
				o.directDups.Add(1)
			} else {
				o.floodDups.Add(1)
			}
			continue
		}
		o.log.Log(context.Background(), LevelTrace, "queuing packet",
			"broker", bc.cfg.Name, "topic", bc.packetTopic, "direction", direction)
		o.enqueuePublish(bc, publishJob{topic: bc.packetTopic, payload: payload})
	}
}

// publishWorker drains a broker's queue. On stop it first drains whatever is
// already buffered (best-effort), then exits.
func (o *MqttObserver) publishWorker(bc *brokerClient) {
	defer close(bc.workerDone)
	for {
		select {
		case <-bc.stop:
			for {
				select {
				case job := <-bc.publishCh:
					o.doPublish(bc, job)
				default:
					return
				}
			}
		case job := <-bc.publishCh:
			o.doPublish(bc, job)
		}
	}
}

func (o *MqttObserver) doPublish(bc *brokerClient, job publishJob) {
	client := bc.currentClient()
	// IsConnected stays true while paho auto-reconnects, and paho then
	// completes a QoS 0 publish without sending or storing it. Drop those here
	// too, so the outage is counted and logged rather than silent.
	if client == nil || !client.IsConnected() || (job.qos == 0 && !client.IsConnectionOpen()) {
		// Publishing to a disconnected client blocks for the full
		// publishWaitTimeout per job and stalls the worker. No buffer: a
		// packet heard while the broker is down is not sent later.
		if bc.downDropped++; bc.downDropped == 1 {
			o.log.Warn("broker not connected, dropping packets until it is", "broker", bc.cfg.Name)
		}
		return
	}
	token := client.Publish(job.topic, job.qos, job.retain, job.payload)
	if !token.WaitTimeout(publishWaitTimeout) {
		o.log.Warn("publish timed out", "broker", bc.cfg.Name, "topic", job.topic)
		return
	}
	if err := token.Error(); err != nil {
		o.log.Error("publish error", "broker", bc.cfg.Name, "error", err)
		return
	}
	if bc.downDropped > 0 {
		o.log.Info("broker publishing again", "broker", bc.cfg.Name, "dropped", bc.downDropped)
		bc.downDropped = 0
	}
}

// enqueuePublish hands a job to the broker's worker without blocking. Called
// from the modem RX path, so this MUST never block; if the queue is full the
// job is dropped and counted.
func (o *MqttObserver) enqueuePublish(bc *brokerClient, job publishJob) {
	select {
	case bc.publishCh <- job:
	case <-bc.stop:
		// Shutting down; drop silently instead of queuing behind an exiting worker.
	default:
		n := bc.dropped.Add(1)
		if n == 1 || n%100 == 0 {
			o.log.Warn("publish queue full, dropping packets", "broker", bc.cfg.Name, "dropped", n)
		}
	}
}

func (o *MqttObserver) advertLoop(ctx context.Context) {
	// Send initial advert
	err := o.advert()
	if err != nil {
		o.log.Error("initial advert error", "error", err)
	}

	advertInterval := o.cfg.Advert.Interval
	if advertInterval == nil || *advertInterval < 1 {
		oneDay := 86400
		advertInterval = &oneDay
	}

	// Get tick
	interval := time.Duration(*advertInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Start loop
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := o.advert()
			if err != nil {
				o.log.Error("advert error", "error", err)
			}
		}
	}
}

func (o *MqttObserver) advert() error {
	appData := meshcore.AdvertAppData{
		Type: "CHAT",
		Name: *o.cfg.Name,
		Lat:  0,
		Lon:  0,
	}

	if o.cfg.Advert.hasLatLon() {
		appData.Lat = int32(math.Round(*o.cfg.Advert.Lat * 1_000_000.0))
		appData.Lon = int32(math.Round(*o.cfg.Advert.Lon * 1_000_000.0))
	}

	rawAppData, err := appData.ToBytes()
	if err != nil {
		return err
	}

	advert := meshcore.Advert{
		PublicKey:  o.id.Identity,
		Timestamp:  uint32(time.Now().Unix()),
		RawAppData: rawAppData,
	}
	advert.SignWith(o.id)

	payload, err := advert.ToBytes()
	if err != nil {
		return err
	}

	pkt := meshcore.Packet{
		Header:     meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypeAdvert, 0),
		PathLength: meshcore.PathHashSize - 1,
		Payload:    payload,
	}

	data, err := pkt.ToBytes()
	if err != nil {
		return err
	}

	return o.radio.SendData(data)
}

func (o *MqttObserver) heartbeatLoop(ctx context.Context) {
	interval := time.Duration(o.cfg.statusIntervalSeconds()) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, bc := range o.brokersSnapshot() {
				o.publishStatus(ctx, bc, "online")
			}
		}
	}
}

// brokersSnapshot copies the live broker list under the mutex so callers can
// iterate it while Stop() nils the original.
func (o *MqttObserver) brokersSnapshot() []*brokerClient {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.brokers
}

func (o *MqttObserver) tokenRefreshLoop(ctx context.Context) {
	refreshAt := time.Duration(float64(tokenLifetime) * 0.8)
	ticker := time.NewTicker(refreshAt)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, bc := range o.brokersSnapshot() {
				if !strings.EqualFold(bc.cfg.AuthType, "token") {
					continue
				}
				o.log.Debug("refreshing token", "broker", bc.cfg.Name)

				newClient, err := o.connectBroker(bc.cfg, bc.iata)
				if err != nil {
					// Without a retry the broker keeps a client whose token
					// expires in ~2 minutes and nothing tries again until the
					// next tick 8 minutes later, so it goes dead for 6+ minutes
					// and stays dead if failures persist.
					//
					// Drop the stale client FIRST. connectAndRetry refuses to
					// replace a client that reports connected, and this one
					// still does -- its token has ~2 minutes left -- so leaving
					// it in place makes the retry return immediately and the
					// fix inert. We have already decided to replace it.
					if stale := bc.currentClient(); stale != nil {
						stale.Disconnect(250)
						bc.swapClient(nil)
					}
					o.log.Error("token refresh reconnect failed, retrying in background",
						"broker", bc.cfg.Name, "error", err)
					go o.connectAndRetry(ctx, bc)
					continue
				}
				old := bc.currentClient()
				bc.swapClient(newClient)
				// Disconnect the old client BEFORE publishing status, not
				// after. connectBroker builds a deterministic ClientID
				// (pubkey+host), so the refresh connection reuses it and the
				// broker kicks the old one per the duplicate-ClientID rule.
				// The old client has SetAutoReconnect, so if it is still around
				// when that fires it reconnects and kicks the NEW one: a flap.
				// paho's first reconnect sleep is 1s (client.go:311) and
				// publishStatus polls the board for 500ms, so publishing first
				// left only a 500ms margin -- safe, but invisibly so, and one
				// added line would have eaten it. Disconnecting first makes the
				// window the swap itself. The 250ms quiesce still lets
				// in-flight publishes drain on the old client.
				if old != nil {
					old.Disconnect(250)
				}
				o.publishStatus(ctx, bc, "online")
				o.log.Info("token refreshed", "broker", bc.cfg.Name)
			}
		}
	}
}

func (o *MqttObserver) publishStatus(ctx context.Context, bc *brokerClient, status string) {
	var radio RadioInfo
	var ds DeviceStats
	var link LinkStats
	if o.stats != nil {
		radio = o.stats.RadioConfig()
		ds = o.stats.Stats(ctx) // polls the board; blocks ~500ms for the reply
		link = o.stats.LinkStats()
	}

	// Sampled AFTER the poll, alongside packets, so every counter in one
	// message comes from one instant. Sampling before it let a packet arrive
	// during the poll and publish "recv: 2" beside "last_rssi: 0".
	health := linkHealth{
		tx:       o.mux.TxStats(),
		queueLen: o.radio.TxQueueLen(),
		lastSNR:  math.Float64frombits(o.lastSNRBits.Load()),
		lastRSSI: int8(o.lastRSSI.Load()),
		rxAirMs:  o.rxAirMs.Load(),
		txAirMs:  o.txAirMs.Load(),
		link:     link,
	}

	packets := PacketCounts{
		Received:   o.packetsReceived.Load(),
		FloodRx:    o.floodRx.Load(),
		FloodTx:    o.floodTx.Load(),
		DirectTx:   o.directTx.Load(),
		DirectRx:   o.directRx.Load(),
		FloodDups:  o.floodDups.Load(),
		DirectDups: o.directDups.Load(),
	}

	payload, err := formatStatus(status, o.originName, o.pubKeyHx, radio, ds, packets,
		o.parseErrors.Load(), health)
	if err != nil {
		o.log.Error("status format error", "error", err)
		return
	}

	o.log.Log(ctx, LevelTrace, "publishing status",
		"broker", bc.cfg.Name, "topic", bc.statusTopic,
		"json", string(payload))
	// Queued, not published inline. A bare token.Wait() here was unbounded:
	// Stop passes a 5s context but it never reaches the token, so an
	// unresponsive broker hung Stop, and with it SIGHUP reload and shutdown.
	// The worker path is bounded by publishWaitTimeout, and it is also the one
	// place the nil/disconnected client is handled, so no guard is needed here.
	o.enqueuePublish(bc, publishJob{
		topic:   bc.statusTopic,
		payload: payload,
		qos:     1,
		retain:  bc.cfg.RetainStatus,
	})
}

func (o *MqttObserver) connectBroker(bcfg BrokerConfig, iata string) (mqtt.Client, error) {
	var scheme string
	switch strings.ToLower(bcfg.Transport) {
	case "websockets", "ws", "wss":
		if bcfg.TlsEnabled {
			scheme = "wss"
		} else {
			scheme = "ws"
		}
	default:
		if bcfg.TlsEnabled {
			scheme = "tls"
		} else {
			scheme = "tcp"
		}
	}

	brokerURL := fmt.Sprintf("%s://%s:%d%s", scheme, bcfg.Host, bcfg.Port, bcfg.Path)
	clientID := fmt.Sprintf("meshcore_%s_%s", o.pubKeyHx[:16], bcfg.Host)

	opts := mqtt.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(clientID)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(5 * time.Minute)

	if bcfg.TlsEnabled {
		opts.SetTLSConfig(&tls.Config{
			InsecureSkipVerify: bcfg.TlsInsecure,
			MinVersion:         tls.VersionTLS12,
		})
	}

	switch strings.ToLower(bcfg.AuthType) {
	case "token":
		audience := bcfg.Audience
		if audience == "" {
			audience = bcfg.Host
		}
		token, _, err := generateToken(o.id, audience, derefStr(o.cfg.Email), derefStr(o.cfg.Owner))
		if err != nil {
			return nil, fmt.Errorf("generating auth token: %w", err)
		}
		opts.SetUsername(tokenUsername(o.id))
		opts.SetPassword(token)
	case "basic":
		opts.SetUsername(bcfg.Username)
		opts.SetPassword(bcfg.Password)
	}

	_, statusTopic := resolveTopics(bcfg, iata, o.pubKeyHx, o.originName)

	// LWT uses minimal status (no live stats — we're about to disconnect).
	offlinePayload, _ := formatStatus("offline", o.originName, o.pubKeyHx, RadioInfo{}, DeviceStats{}, PacketCounts{}, 0, linkHealth{})
	opts.SetWill(statusTopic, string(offlinePayload), 1, bcfg.RetainStatus)

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(connectWaitTimeout) {
		client.Disconnect(0)
		return nil, fmt.Errorf("connecting to %s: timeout after %s", brokerURL, connectWaitTimeout)
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", brokerURL, err)
	}

	return client, nil
}

var payloadTypeNames = map[string]byte{
	"req":        meshcore.PayloadTypeReq,
	"response":   meshcore.PayloadTypeResponse,
	"txt_msg":    meshcore.PayloadTypeTxtMsg,
	"ack":        meshcore.PayloadTypeAck,
	"advert":     meshcore.PayloadTypeAdvert,
	"grp_txt":    meshcore.PayloadTypeGrpTxt,
	"grp_data":   meshcore.PayloadTypeGrpData,
	"anon_req":   meshcore.PayloadTypeAnonReq,
	"path":       meshcore.PayloadTypePath,
	"trace":      meshcore.PayloadTypeTrace,
	"multi_part": meshcore.PayloadTypeMultiPart,
	"control":    meshcore.PayloadTypeControl,
	"raw_custom": meshcore.PayloadTypeRawCustom,
}

func parseDisallowed(names []string) map[byte]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[byte]bool, len(names))
	for _, name := range names {
		if v, ok := payloadTypeNames[strings.ToLower(name)]; ok {
			m[v] = true
		}
	}
	return m
}
