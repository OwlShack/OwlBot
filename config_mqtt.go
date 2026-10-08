package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

type MqttConfig struct {
	Name           *string        `json:"name" yaml:"name" toml:"name"`
	IataCode       *string        `json:"iataCode" yaml:"iataCode" toml:"iataCode"`
	KeyFile        *string        `json:"keyFile" yaml:"keyFile" toml:"keyFile"`
	StatusInterval *int           `json:"statusInterval" yaml:"statusInterval" toml:"statusInterval"`
	Owner          *string        `json:"owner" yaml:"owner" toml:"owner"`
	Email          *string        `json:"email" yaml:"email" toml:"email"`
	Brokers        []BrokerConfig `json:"brokers" yaml:"brokers" toml:"broker"`
	Advert         *MqttAdvert    `json:"advert" yaml:"advert" toml:"advert"`
}

type BrokerConfig struct {
	Name        string `json:"name" yaml:"name" toml:"name"`
	Enabled     bool   `json:"enabled" yaml:"enabled" toml:"enabled"`
	Dedup       bool   `json:"dedup" yaml:"dedup" toml:"dedup"`             // Do we enable dedup checks
	Transport   string `json:"transport" yaml:"transport" toml:"transport"` // websockets or tcp
	Host        string `json:"host" yaml:"host" toml:"host"`
	Port        int    `json:"port" yaml:"port" toml:"port"`
	TopicPrefix string `json:"topicPrefix" yaml:"topicPrefix" toml:"topicPrefix"` // e.g. "meshcore" for LetsMesh, or custom
	// Placeholders {iata} {pubkey} {name}, plus meshcoretomqtt's uppercase
	// tokens; empty = "<topicPrefix>/{iata}/{pubkey}/packets" (resp. "/status").
	PacketTopic           string   `json:"packetTopic,omitempty" yaml:"packetTopic,omitempty" toml:"packetTopic,omitempty"`
	StatusTopic           string   `json:"statusTopic,omitempty" yaml:"statusTopic,omitempty" toml:"statusTopic,omitempty"`
	DisallowedPacketTypes []string `json:"disallowedPacketTypes" yaml:"disallowedPacketTypes" toml:"disallowedPacketTypes"`
	RetainStatus          bool     `json:"retainStatus" yaml:"retainStatus" toml:"retainStatus"`
	TlsEnabled            bool     `json:"tlsEnabled" yaml:"tlsEnabled" toml:"tlsEnabled"`
	TlsInsecure           bool     `json:"tlsInsecure" yaml:"tlsInsecure" toml:"tlsInsecure"`
	AuthType              string   `json:"authType" yaml:"authType" toml:"authType"` // token, basic, or none
	Username              string   `json:"username" yaml:"username" toml:"username"`
	Password              string   `json:"password" yaml:"password" toml:"password"`
	Path                  string   `json:"path" yaml:"path" toml:"path"` // WebSocket path (default: /)
	Audience              string   `json:"audience" yaml:"audience" toml:"audience"`
}

type MqttAdvert struct {
	Enabled  bool     `json:"enabled" yaml:"enabled" toml:"enabled"`
	Interval *int     `json:"interval,omitempty" yaml:"interval,omitempty" toml:"interval,omitempty"`
	Lat      *float64 `json:"lat,omitempty" yaml:"lat,omitempty" toml:"lat,omitempty"`
	Lon      *float64 `json:"lon,omitempty" yaml:"lon,omitempty" toml:"lon,omitempty"`
}

func (a *MqttAdvert) hasLatLon() bool {
	if a.Lat == nil || a.Lon == nil {
		return false
	}

	return *a.Lat != 0 && *a.Lon != 0
}

func (c *MqttConfig) statusIntervalSeconds() int {
	if c.StatusInterval != nil && *c.StatusInterval > 0 {
		return *c.StatusInterval
	}
	return 300
}

// topicPlaceholders are the tokens a topic template may use, lowercase or
// meshcoretomqtt's uppercase. Same set as OwlShack.
var topicPlaceholders = []string{
	"iata", "IATA",
	"pubkey", "PUBKEY", "publicKey", "PUBLIC_KEY",
	"name", "NAME", "origin",
}

var topicTokenRe = regexp.MustCompile(`\{([^{}]*)\}`)

// validateTopicTemplate rejects unknown placeholders and MQTT wildcards
// (publish topics may not contain + or #).
func validateTopicTemplate(t string) error {
	if t == "" {
		return nil
	}
	if strings.ContainsAny(t, "+#") {
		return fmt.Errorf("publish topics may not contain MQTT wildcards (+/#)")
	}
	for _, m := range topicTokenRe.FindAllStringSubmatch(t, -1) {
		if !slices.Contains(topicPlaceholders, m[1]) {
			return fmt.Errorf("unknown placeholder {%s} (supported: {iata} {pubkey} {name})", m[1])
		}
	}
	return nil
}

// resolveTopics expands a broker's topic templates. An unset template keeps
// the topicPrefix layout, so existing configs publish where they always have.
func resolveTopics(bcfg BrokerConfig, iata, pubKeyHx, origin string) (packetTopic, statusTopic string) {
	prefix := bcfg.TopicPrefix
	if prefix == "" {
		prefix = "meshcore"
	}
	expand := func(tmpl, kind string) string {
		if tmpl == "" {
			tmpl = prefix + "/{iata}/{pubkey}/" + kind
		}
		return strings.NewReplacer(
			"{iata}", iata, "{IATA}", iata,
			"{pubkey}", pubKeyHx, "{PUBKEY}", pubKeyHx,
			"{publicKey}", pubKeyHx, "{PUBLIC_KEY}", pubKeyHx,
			"{name}", origin, "{NAME}", origin, "{origin}", origin,
		).Replace(tmpl)
	}
	return expand(bcfg.PacketTopic, "packets"), expand(bcfg.StatusTopic, "status")
}
