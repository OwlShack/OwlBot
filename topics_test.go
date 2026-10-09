package main

import (
	"strings"
	"testing"
)

func TestResolveTopics(t *testing.T) {
	cases := []struct {
		name               string
		cfg                BrokerConfig
		wantPacket, wantSt string
	}{
		// An existing config must keep publishing exactly where it did.
		{"default", BrokerConfig{}, "meshcore/akl/AB12/packets", "meshcore/akl/AB12/status"},
		{"prefix", BrokerConfig{TopicPrefix: "mesh"}, "mesh/akl/AB12/packets", "mesh/akl/AB12/status"},
		{"templates", BrokerConfig{TopicPrefix: "ignored", PacketTopic: "obs/{name}/{iata}/pkt", StatusTopic: "obs/{pubkey}/up"},
			"obs/Obs One/akl/pkt", "obs/AB12/up"},
		{"meshcoretomqtt tokens", BrokerConfig{PacketTopic: "x/{IATA}/{PUBLIC_KEY}/{NAME}"},
			"x/akl/AB12/Obs One", "meshcore/akl/AB12/status"},
	}
	for _, c := range cases {
		p, s := resolveTopics(c.cfg, "akl", "AB12", "Obs One")
		if p != c.wantPacket || s != c.wantSt {
			t.Errorf("%s: got %q %q, want %q %q", c.name, p, s, c.wantPacket, c.wantSt)
		}
	}
}

func TestTopicTemplatesValidatedOnLoad(t *testing.T) {
	for _, bad := range []BrokerConfig{
		{Name: "b", PacketTopic: "meshcore/+/{pubkey}"},
		{Name: "b", StatusTopic: "meshcore/#"},
		{Name: "b", PacketTopic: "meshcore/{region}/packets"},
	} {
		name := "bot"
		cfg := &Config{Bots: []BotConfig{{Name: &name, Mqtt: &MqttConfig{Brokers: []BrokerConfig{bad}}}}}
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), `broker "b"`) {
			t.Errorf("%+v: got %v, want an error naming the broker", bad, err)
		}
	}
	name := "bot"
	ok := &Config{Bots: []BotConfig{{Name: &name, Mqtt: &MqttConfig{Brokers: []BrokerConfig{
		{Name: "b", PacketTopic: "x/{iata}/{pubkey}/{name}", StatusTopic: "x/{IATA}/{origin}"}}}}}}
	if err := ok.validate(); err != nil {
		t.Errorf("valid templates refused: %v", err)
	}
}
