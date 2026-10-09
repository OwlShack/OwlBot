package main

import (
	"encoding/json"
	"fmt"

	"github.com/OwlShack/meshcore-go/node"
	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

type ChannelRef struct {
	Name       string `json:"name" yaml:"name" toml:"name"`
	PrivateKey string `json:"privateKey,omitempty" yaml:"privateKey,omitempty" toml:"privateKey,omitempty"`
}

func (cr *ChannelRef) UnmarshalText(text []byte) error {
	cr.Name = string(text)
	return nil
}

type ChannelList []ChannelRef

func (cl *ChannelList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("channels must be an array: %w", err)
	}

	result := make(ChannelList, 0, len(raw))
	for _, item := range raw {
		var s string
		if err := json.Unmarshal(item, &s); err == nil {
			result = append(result, ChannelRef{Name: s})
			continue
		}
		var ref ChannelRef
		if err := json.Unmarshal(item, &ref); err != nil {
			return fmt.Errorf("channel entry must be a string or {name, privateKey} object: %w", err)
		}
		result = append(result, ref)
	}
	*cl = result
	return nil
}

func (cl *ChannelList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("channels must be a sequence")
	}

	result := make(ChannelList, 0, len(value.Content))
	for _, node := range value.Content {
		switch node.Kind {
		case yaml.ScalarNode:
			result = append(result, ChannelRef{Name: node.Value})
		case yaml.MappingNode:
			var ref ChannelRef
			if err := node.Decode(&ref); err != nil {
				return fmt.Errorf("channel entry decode error: %w", err)
			}
			result = append(result, ref)
		default:
			return fmt.Errorf("channel entry must be a string or mapping")
		}
	}
	*cl = result
	return nil
}

type TriggerConfig struct {
	Type     string `json:"type" yaml:"type" toml:"type"` // group, private, dm, cron, cap, etc
	Template string `json:"template" yaml:"template" toml:"template"`

	// Message Overflow behaviour
	CharLimitBehaviour *string `json:"charLimitBehaviour" yaml:"charLimitBehaviour" toml:"charLimitBehaviour"` // e.g. truncate or split

	// Messages/DMs
	Match    *[]string    `json:"match" yaml:"match" toml:"match"`          // Patterns to match against (supports wildcards/regex)
	Channels *ChannelList `json:"channels" yaml:"channels" toml:"channels"` // Channels to listen on (strings or {name, privateKey} objects)
	Contacts *[]string    `json:"contacts" yaml:"contacts" toml:"contact"`  // What Contacts to listen in for DMs

	// Retry Settings
	RetryTimeout *int64 `json:"retryTimeout" yaml:"retryTimeout" toml:"retryTimeout"` // Stored as seconds
	MaxRetries   *int   `json:"maxRetries" yaml:"maxRetries" toml:"maxRetries"`

	// Path Hash Size: 1-4 = fixed size, 0 = mirror incoming packet's hash size, nil = default (1)
	PathHashSize *uint8 `json:"pathHashSize,omitempty" yaml:"pathHashSize,omitempty" toml:"pathHashSize,omitempty"`

	// Cron Trigger
	Schedule string `json:"schedule,omitempty" yaml:"schedule,omitempty" toml:"schedule,omitempty"`

	// FloodScope is the region this trigger's posts are scoped to; unset or
	// inherit takes the bot's.
	FloodScope FloodScope `json:"floodScope,omitempty" yaml:"floodScope,omitempty" toml:"floodScope,omitempty"`
}

type BotConfig struct {
	Name *string `json:"name" yaml:"name" toml:"name"` // Name of the Node - Used in Channel Messages

	Triggers []TriggerConfig `json:"triggers" yaml:"triggers" toml:"trigger"`

	// FloodScope is the region the bot's posts are scoped to unless a trigger
	// says otherwise; unset sends unscoped.
	FloodScope FloodScope `json:"floodScope,omitempty" yaml:"floodScope,omitempty" toml:"floodScope,omitempty"`

	// MQTT publishing, owned by this bot. At most one bot in the whole app
	// may define it (validated after load).
	Mqtt *MqttConfig `json:"mqtt,omitempty" yaml:"mqtt,omitempty" toml:"mqtt,omitempty"`
}

type Config struct {
	// Logging
	LogLevel *string `json:"logLevel" yaml:"logLevel" toml:"logLevel"`

	// NodeType is IGNORED and kept only so existing configs still load: the
	// connection scheme picks the modem. It was only ever "kiss", and a user
	// moving to SPI would carry nodeType = "kiss" over from the old example,
	// so honouring it would contradict the connection it sits beside.
	NodeType *string `json:"nodeType" yaml:"nodeType" toml:"nodeType"`

	// Connection picks the modem by scheme, the same strings OwlShack takes:
	//   serial://<path> or tcp://<host:port>   MeshCore KISS firmware
	//   openhop://<path> or openhop://<host:port>   openHop Modem firmware
	//   spi://[<port>]   a bare SX1262 on this host's SPI bus; needs spiBoard
	Connection *string `json:"connection" yaml:"connection" toml:"connection"`
	// BaudRate is for KISS serial only, defaulting to 115200 there. openHop
	// ignores it, because its firmware fixes 921600.
	BaudRate *int `json:"baudRate" yaml:"baudRate" toml:"baudRate"`

	// SPIBoard names the SPI hat, so its pins come from a vetted preset rather
	// than being typed in one by one. Wrong pins give a silently dead radio.
	SPIBoard *string `json:"spiBoard" yaml:"spiBoard" toml:"spiBoard"`

	// ModemToken authenticates to an openHop modem over TCP. A password: kept
	// out of Connection so it is not logged along with the connection string.
	ModemToken *string `json:"modemToken" yaml:"modemToken" toml:"modemToken"`

	// Radio Settings
	Freq *float64 `json:"freq" yaml:"freq" toml:"freq"` // e.g. 917.375
	Bw   *float64 `json:"bw" yaml:"bw" toml:"bw"`       // e.g. 62.50
	SF   *uint8   `json:"sf" yaml:"sf" toml:"sf"`       // e.g. 7
	CR   *uint8   `json:"cr" yaml:"cr" toml:"cr"`       // e.g. 8
	TX   *uint8   `json:"tx" yaml:"tx" toml:"tx"`       // TX Power e.g. 22

	// DutyCycle is the transmit duty cycle as a percentage, named after the
	// firmware's "set dutycycle". Unlike firmware it accepts fractions of a
	// percent, so an EU868 0.1% sub-band is expressible here; firmware can only
	// reach that through its raw, unvalidated "set af". Because percent covers
	// the whole factor range continuously, there is no raw-factor key: a second
	// setting could only restate this one, or disagree with it. Applies
	// process-wide, since one mux is shared by every bot and the observer.
	DutyCycle *float64 `json:"dutyCycle" yaml:"dutyCycle" toml:"dutyCycle"`

	// Bots
	Bots []BotConfig `json:"bots" yaml:"bots" toml:"bot"`

	// MQTT Observers/Publishers
	Observers []MqttConfig `json:"observers" yaml:"observers" toml:"observer"`
}

func DefaultConfig() Config {
	connection := "serial:///dev/ttyACM0"
	freq := 917.375
	bw := 62.50
	sf := uint8(7)
	cr := uint8(8)
	tx := uint8(2)

	return Config{
		Connection: &connection,
		Freq:       &freq,
		Bw:         &bw,
		SF:         &sf,
		CR:         &cr,
		TX:         &tx,
	}
}

// effectiveAirtimeFactor resolves the configured duty cycle to the factor the
// radio mux takes, using the firmware's derivation (CommonCLI handleSetCmd).
// Unset defers to the library constant rather than a hardcoded percent, so the
// default stays exactly at firmware parity even if that constant moves.
func (c *Config) effectiveAirtimeFactor() float64 {
	if c.DutyCycle != nil {
		return (100.0 / *c.DutyCycle) - 1.0
	}
	return node.DefaultAirtimeFactor
}

func (c *Config) applyDefaults() {
	defaults := DefaultConfig()
	if c.Connection == nil {
		c.Connection = defaults.Connection
	}
	if c.Freq == nil {
		c.Freq = defaults.Freq
	}
	if c.Bw == nil {
		c.Bw = defaults.Bw
	}
	if c.SF == nil {
		c.SF = defaults.SF
	}
	if c.CR == nil {
		c.CR = defaults.CR
	}
	if c.TX == nil {
		c.TX = defaults.TX
	}
}

func UnmarshalConfigJson(data []byte) (*Config, error) {
	var cfg Config
	err := json.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func UnmarshalConfigYaml(data []byte) (*Config, error) {
	var cfg Config
	err := yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func UnmarshalConfigToml(data []byte) (*Config, error) {
	var cfg Config
	err := toml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces cross-field invariants after load: MQTT is app-global, so
// at most one bot may own an [mqtt] section.
func (c *Config) validate() error {
	n := 0
	for _, b := range c.Bots {
		if b.Mqtt != nil {
			n++
		}
	}
	if n > 1 {
		return fmt.Errorf("at most one bot may define an mqtt section, found %d", n)
	}
	for _, b := range c.Bots {
		if err := b.FloodScope.validate(false); err != nil {
			return fmt.Errorf("bot %s: %w", botLabel(b), err)
		}
		for i, t := range b.Triggers {
			if err := t.FloodScope.validate(true); err != nil {
				return fmt.Errorf("bot %s trigger %d: %w", botLabel(b), i+1, err)
			}
		}
		if b.Mqtt == nil {
			continue
		}
		for _, br := range b.Mqtt.Brokers {
			if err := validateTopicTemplate(br.PacketTopic); err != nil {
				return fmt.Errorf("broker %q packetTopic: %w", br.Name, err)
			}
			if err := validateTopicTemplate(br.StatusTopic); err != nil {
				return fmt.Errorf("broker %q statusTopic: %w", br.Name, err)
			}
		}
	}
	// Checked here, not at connect, so a bad connection on SIGHUP is a reload
	// that keeps the running config rather than a modem setup that exits.
	if c.Connection != nil {
		scheme, _, ok := parseConnection(*c.Connection)
		if !ok {
			return fmt.Errorf("invalid connection %q: must start with serial://, tcp://, spi:// or openhop://", *c.Connection)
		}
		// This process drives the radio on SPI, so its pins are unknown
		// rather than defaultable.
		if scheme == "spi" && (c.SPIBoard == nil || *c.SPIBoard == "") {
			return fmt.Errorf("connection %q needs spiBoard set", *c.Connection)
		}
	}
	// Wider than firmware's 1-100 so sub-1% sub-bands are reachable, but still
	// bounded: 0 or negative would divide by zero or invert the budget.
	if c.DutyCycle != nil && (*c.DutyCycle <= 0 || *c.DutyCycle > 100) {
		return fmt.Errorf("dutyCycle is a percentage, must be >0 and <=100, got %v", *c.DutyCycle)
	}
	return nil
}

func botLabel(b BotConfig) string {
	if b.Name == nil {
		return "(unnamed)"
	}
	return fmt.Sprintf("%q", *b.Name)
}
