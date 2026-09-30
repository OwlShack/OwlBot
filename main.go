package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/meshcore-go/meshcore-go/hardware"
	kissTransport "github.com/meshcore-go/meshcore-go/hardware/transport"
	"github.com/meshcore-go/meshcore-go/node"
	flag "github.com/spf13/pflag"
)

var version = "dev"

const LevelTrace = slog.Level(-8)

var defaultConfigNames = []string{
	"config.toml",
	"config.yaml",
	"config.yml",
	"config.json",
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

type modemState struct {
	modem       node.Modem
	radioConfig *hardware.RadioConfig
	// airtimeFactor is process-wide: one mux serves every bot and the observer.
	airtimeFactor float64
	// txSink is the current observer's tx hook. The modem's outbound handler is
	// registered ONCE per modem and dispatches through here, because
	// AddOutboundHandler has no removal: registering per observer would
	// accumulate handlers pointing at dead observers on every reload.
	txSink atomic.Pointer[func([]byte)]
	stats  StatsProvider
	// parseErrors counts frames that arrived intact and did not decode as a
	// MeshCore packet. Published as packet_parse_errors, NOT recv_errors: the
	// latter is the radio driver's failure-to-receive count and is polled from
	// the firmware in stats.go.
	parseErrors *atomic.Uint64
	closers     []io.Closer

	// watchDone stops this modem's watchers (modem_watch.go).
	watchDone chan struct{}
	closeOnce sync.Once
}

func (m *modemState) setTxSink(f func([]byte)) { m.txSink.Store(&f) }

func (m *modemState) clearTxSink() { m.txSink.Store(nil) }

func (m *modemState) Close() {
	m.closeOnce.Do(func() {
		// Watchers first: closing the modem ends its read loop, which fires
		// Dead(), and a deliberate close is not a fault to reconnect from.
		// watch() relies on this order when Close runs on another goroutine.
		if m.watchDone != nil {
			close(m.watchDone)
		}
		for i := len(m.closers) - 1; i >= 0; i-- {
			m.closers[i].Close()
		}
	})
}

// muxOptions builds the mux options for the given modem state. Centralized so
// startup and the SIGHUP reconnect path stay in sync.
func muxOptions(ms *modemState) []node.MuxOption {
	opts := []node.MuxOption{
		node.WithMuxLogger(slog.Default()),
		node.WithMuxErrorHandler(func(err error) {
			slog.Debug("mux receive error", "component", "modem", "error", err)
			ms.parseErrors.Add(1)
		}),
	}
	if ms.radioConfig != nil {
		// The budget only exists when an estimator is set, so the factor is
		// only meaningful alongside it.
		opts = append(opts,
			node.WithMuxAirtimeEstimator(hardware.LoRaAirtimeEstimator(ms.radioConfig)),
			node.WithMuxAirtimeFactor(ms.airtimeFactor),
		)
	}
	return opts
}

func main() {
	configPath := flag.StringP("config", "c", "", "path to config file (toml, yaml, or json)")
	showVersion := flag.BoolP("version", "V", false, "print version and exit")
	verbosity := flag.CountP("verbose", "v", "increase log verbosity (-v=debug, -vv=trace, -vvv=trace+)")
	flag.Parse()

	if *showVersion {
		fmt.Println("meshcore-bot", version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.Bots) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no bots configured")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *verbosity > 0 {
		level := slog.LevelDebug
		if *verbosity >= 2 {
			level = LevelTrace
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.LevelKey && a.Value.Any().(slog.Level) == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
				return a
			},
		})))
	} else if cfg.LogLevel != nil && *cfg.LogLevel != "" {
		// Debug, Info, Warn, Error
		lower := strings.ToLower(*cfg.LogLevel)
		level := slog.LevelInfo

		switch lower {
		case "debug":
			level = slog.LevelDebug
		case "info":
			level = slog.LevelInfo
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		case "trace":
			level = LevelTrace
		default:
			slog.Info("Invalid logging level provided, Defaulting to Info")
		}

		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.LevelKey && a.Value.Any().(slog.Level) == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
				return a
			},
		})))
	}

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	// died carries the modem a watcher gave up on (modem_watch.go).
	died := make(chan *modemState)

	var (
		ms        *modemState
		mux       *node.RadioMux
		bots      []*Bot
		observers []*MqttObserver
		// retry is armed while the radio is down; nil otherwise.
		retry      <-chan time.Time
		retryDelay = initialRadioRetry
	)

	// bringUp starts the radio and everything that hangs off it, leaving the
	// running set untouched if it cannot.
	bringUp := func(c *Config) error {
		m, err := setupModem(ctx, c)
		if err != nil {
			return err
		}
		x := node.NewRadioMux(m.modem, muxOptions(m)...)
		b, err := startBots(ctx, c, m, x)
		if err != nil {
			m.Close()
			return fmt.Errorf("bot startup: %w", err)
		}
		o, err := startObservers(ctx, c, x, m)
		if err != nil {
			slog.Error("mqtt observer startup failed", "error", err)
		}
		m.watch(died)
		ms, mux, bots, observers = m, x, b, o
		retry, retryDelay = nil, initialRadioRetry
		return nil
	}
	tearDown := func() {
		stopObservers(observers)
		stopBots(bots)
		if ms != nil {
			ms.Close()
		}
		ms, mux, bots, observers = nil, nil, nil, nil
	}
	// radioDown arms the next attempt. Retrying from the select rather than a
	// blocking backoff loop keeps a SIGHUP carrying the operator's fix, a
	// corrected port say, from queueing behind the retries.
	radioDown := func(err error) {
		retry = time.After(retryDelay)
		slog.Warn("radio unavailable, retrying", "error", err, "retry_in", retryDelay)
		retryDelay = min(retryDelay*2, maxRadioRetry)
	}

	// Startup still exits: a radio that never comes up is usually a config
	// mistake, and failing loudly lets a supervisor show it.
	if err := bringUp(cfg); err != nil {
		slog.Error("radio setup failed", "error", err)
		os.Exit(1)
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down...")
			tearDown()
			return

		case m := <-died:
			if m != ms {
				continue // a modem already replaced
			}
			slog.Error("radio lost, reconnecting")
			tearDown()
			if err := bringUp(cfg); err != nil {
				radioDown(err)
			}

		case <-retry:
			if err := bringUp(cfg); err != nil {
				radioDown(err)
			} else {
				slog.Info("radio reconnected")
			}

		case <-sighup:
			slog.Info("SIGHUP received, reloading config...")

			newCfg, err := loadConfig(*configPath)
			if err != nil {
				slog.Error("config reload failed, keeping current config", "error", err)
				continue
			}

			if len(newCfg.Bots) == 0 {
				slog.Error("reloaded config has no bots, keeping current config")
				continue
			}

			// A new radio is needed when there is none (it is down and being
			// retried) or its settings changed. That path retries rather than
			// exits: the config has already passed validation, so a failure
			// here is the device, not the file.
			if ms == nil || modemConfigChanged(cfg, newCfg) {
				slog.Info("reconnecting radio for the new config...")
				tearDown()
				cfg = newCfg
				if err := bringUp(cfg); err != nil {
					radioDown(err)
					continue
				}
				slog.Info("config reloaded successfully")
				continue
			}

			stopObservers(observers)
			stopBots(bots)

			bots, err = startBots(ctx, newCfg, ms, mux)
			if err != nil {
				slog.Error("bot restart failed after reload", "error", err)
				os.Exit(1)
			}

			observers, err = startObservers(ctx, newCfg, mux, ms)
			if err != nil {
				slog.Error("mqtt observer restart failed after reload", "error", err)
			}

			cfg = newCfg
			slog.Info("config reloaded successfully")
		}
	}
}

func modemConfigChanged(old, new_ *Config) bool {
	return derefStr(old.Connection) != derefStr(new_.Connection) ||
		derefStr(old.SPIBoard) != derefStr(new_.SPIBoard) ||
		derefStr(old.ModemToken) != derefStr(new_.ModemToken) ||
		derefInt(old.BaudRate) != derefInt(new_.BaudRate) ||
		derefFloat(old.Freq) != derefFloat(new_.Freq) ||
		derefFloat(old.Bw) != derefFloat(new_.Bw) ||
		derefUint8(old.SF) != derefUint8(new_.SF) ||
		derefUint8(old.CR) != derefUint8(new_.CR) ||
		derefUint8(old.TX) != derefUint8(new_.TX) ||
		old.effectiveAirtimeFactor() != new_.effectiveAirtimeFactor()
}

// ponytail: the airtime comparison above is exact float equality, deliberately.
// Two cases, two reasons. An unchanged percent is bitwise identical because both
// sides run the same computation on the same input. An unset config compared
// against the equivalent explicit percent is exact only because
// node.DefaultAirtimeFactor is 1.0, and both 50 and 1.0 are representable.
// Ceiling: if that constant moves to a value whose percent is inexact, the
// no-op case starts reporting a change and churns a modem reconnect on every
// SIGHUP. Upgrade path then is an epsilon comparison, not before.

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefUint8(p *uint8) uint8 {
	if p == nil {
		return 0
	}
	return *p
}

// rxHandlerWatchdog bounds how long one frame dispatch may take before the
// modem warns and increments ModemStats.HandlerSlow.
const rxHandlerWatchdog = 500 * time.Millisecond

func setupModem(ctx context.Context, cfg *Config) (*modemState, error) {
	ms := &modemState{
		parseErrors:   &atomic.Uint64{},
		airtimeFactor: cfg.effectiveAirtimeFactor(),
		watchDone:     make(chan struct{}),
	}

	// validate has already checked the scheme; this cannot fail on a loaded config.
	connScheme, connAddr, _ := parseConnection(*cfg.Connection)

	ms.radioConfig = &hardware.RadioConfig{
		FreqHz: uint32(*cfg.Freq * 1000000),
		BwHz:   uint32(*cfg.Bw * 1000),
		SF:     *cfg.SF,
		CR:     *cfg.CR,
	}
	radio := RadioInfo{
		FreqHz:  ms.radioConfig.FreqHz,
		BwHz:    ms.radioConfig.BwHz,
		SF:      ms.radioConfig.SF,
		CR:      ms.radioConfig.CR,
		TxPower: *cfg.TX,
	}

	// Log the duty cycle, not the factor: dutyCycle = 1/(1+factor) is
	// inverted from the intuitive reading, so print the number an operator
	// actually needs to compare against their region's limit.
	slog.Info("airtime budget",
		"factor", ms.airtimeFactor,
		"duty_cycle_pct", 100/(1+ms.airtimeFactor))

	var err error
	switch connScheme {
	case "spi":
		err = setupSPI(ms, cfg, connAddr, radio)
	case "openhop":
		err = setupOpenhop(ctx, ms, cfg, connAddr, radio)
	default:
		err = setupKiss(ctx, ms, cfg, connScheme, connAddr, radio)
	}
	if err != nil {
		return nil, err
	}

	// Registered once, for the life of this modem. Fires for every packet
	// the process transmits, not just one virtual radio's. Every driver runs
	// this on its send path (KISS under its send lock), so it must not block
	// or send: the sink only counts and does a non-blocking enqueue.
	ms.modem.AddOutboundHandler(func(data []byte) {
		if f := ms.txSink.Load(); f != nil {
			(*f)(data)
		}
	})

	return ms, nil
}

// setupKiss connects to MeshCore KISS firmware over serial or TCP and
// configures its radio.
func setupKiss(ctx context.Context, ms *modemState, cfg *Config, connScheme, connAddr string, radio RadioInfo) error {
	baud := 115200 // MeshCore's KISS modem firmware
	if cfg.BaudRate != nil {
		baud = *cfg.BaudRate
	}

	var t hardware.Transport
	switch connScheme {
	case "serial":
		t = kissTransport.NewSerialTransport(kissTransport.SerialConfig{
			Port:     connAddr,
			BaudRate: baud,
		})
	case "tcp":
		t = kissTransport.NewTCPTransport(kissTransport.TCPConfig{
			Address: connAddr,
		})
	}

	kissModem := hardware.NewKissModem(
		t,
		hardware.WithSignalReport(true),
		hardware.WithLogger(slog.Default()),
		// Flow control is already the library default; pinned explicitly so a
		// change of default cannot silently un-serialize TX. The estimator is
		// the part that matters: it sizes the TX_DONE wait from real
		// time-on-air, which DefaultTxTimeout is too short for at high SF.
		hardware.WithTxFlowControl(hardware.DefaultTxTimeout),
		hardware.WithTxAirtimeEstimator(hardware.LoRaAirtimeEstimator(ms.radioConfig)),
		// DATA frames dispatch serially on one goroutine, so a slow handler
		// stalls RX for every bot. Dispatch should be sub-millisecond; this
		// only fires on a real stall.
		hardware.WithHandlerWatchdog(rxHandlerWatchdog),
	)

	kissModem.SetErrorHandler(func(err error) {
		slog.Warn("modem error", "component", "modem", "error", err)
	})

	connectCtx, connectCancel := context.WithTimeout(ctx, 10*time.Second)
	defer connectCancel()

	if err := kissModem.Connect(connectCtx); err != nil {
		return fmt.Errorf("kiss connect: %w", err)
	}
	ms.closers = append(ms.closers, kissModem)

	if err := kissModem.SetRadio(ms.radioConfig); err != nil {
		ms.Close()
		return fmt.Errorf("SET_RADIO: %w", err)
	}
	slog.Info("SET_RADIO", "freq", *cfg.Freq, "bw", *cfg.Bw, "sf", *cfg.SF, "cr", *cfg.CR)

	if err := kissModem.SetTxPower(*cfg.TX); err != nil {
		ms.Close()
		return fmt.Errorf("SET_TX_POWER: %w", err)
	}
	slog.Info("SET_TX_POWER", "tx", *cfg.TX)

	ms.stats = NewKissStatsProvider(kissModem, radio)
	ms.modem = kissModem
	return nil
}

func startBots(ctx context.Context, cfg *Config, ms *modemState, mux *node.RadioMux) ([]*Bot, error) {
	nodeOpts := []node.Option{}

	var bots []*Bot
	for _, botCfg := range cfg.Bots {
		b, err := NewBot(botCfg, mux, nodeOpts...)
		if err != nil {
			stopBots(bots)
			return nil, fmt.Errorf("creating bot %q: %w", derefStr(botCfg.Name), err)
		}
		if err := b.Start(ctx); err != nil {
			stopBots(bots)
			return nil, fmt.Errorf("starting bot %q: %w", derefStr(botCfg.Name), err)
		}
		bots = append(bots, b)
		slog.Info("started bot", "bot", *botCfg.Name)
	}

	return bots, nil
}

func stopBots(bots []*Bot) {
	for _, b := range bots {
		b.Stop()
	}
}

func startObservers(ctx context.Context, cfg *Config, mux *node.RadioMux, ms *modemState) ([]*MqttObserver, error) {
	obsCfg := botMqttConfig(cfg)
	if obsCfg == nil {
		return nil, nil
	}

	keyFile := "mqtt_identity.key"
	if obsCfg.KeyFile != nil && *obsCfg.KeyFile != "" {
		keyFile = *obsCfg.KeyFile
	}

	id, err := loadOrCreateIdentity(keyFile)
	if err != nil {
		return nil, fmt.Errorf("mqtt identity: %w", err)
	}

	obs, err := NewMqttObserver(*obsCfg, mux, id, ms)
	if err != nil {
		return nil, fmt.Errorf("creating mqtt observer: %w", err)
	}
	if err := obs.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting mqtt observer: %w", err)
	}
	slog.Info("started mqtt observer", "name", derefStr(obsCfg.Name), "pubkey", publicKeyHex(id)[:16]+"...")
	return []*MqttObserver{obs}, nil
}

// botMqttConfig returns the single bot-owned MQTT config, or nil. Config
// validation guarantees at most one exists.
func botMqttConfig(cfg *Config) *MqttConfig {
	for i := range cfg.Bots {
		if cfg.Bots[i].Mqtt != nil {
			return cfg.Bots[i].Mqtt
		}
	}
	return nil
}

func stopObservers(observers []*MqttObserver) {
	for _, o := range observers {
		if o.ms != nil {
			o.ms.clearTxSink()
		}
		o.Stop()
	}
}

func loadConfig(path string) (*Config, error) {
	if path == "" {
		return loadConfigFromCwd()
	}
	return loadConfigFromPath(path)
}

func loadConfigFromCwd() (*Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting working directory: %w", err)
	}

	for _, name := range defaultConfigNames {
		p := filepath.Join(cwd, name)
		if _, err := os.Stat(p); err == nil {
			slog.Info("using config", "path", p)
			return loadConfigFromPath(p)
		}
	}

	return nil, fmt.Errorf("no config file found in %s (tried %s)", cwd, strings.Join(defaultConfigNames, ", "))
}

func loadConfigFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(path))
	var cfg *Config
	switch ext {
	case ".toml":
		cfg, err = UnmarshalConfigToml(data)
	case ".yaml", ".yml":
		cfg, err = UnmarshalConfigYaml(data)
	case ".json":
		cfg, err = UnmarshalConfigJson(data)
	default:
		err = fmt.Errorf("unsupported config format %q", ext)
	}
	if err != nil {
		return nil, err
	}

	return migrateLegacyObservers(path, data, cfg)
}

func parseConnection(conn string) (scheme, addr string, ok bool) {
	for _, prefix := range []string{"serial://", "tcp://", "spi://", "openhop://"} {
		if strings.HasPrefix(conn, prefix) {
			return strings.TrimSuffix(prefix, "://"), conn[len(prefix):], true
		}
	}
	return "", "", false
}
