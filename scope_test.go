package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	"github.com/OwlShack/meshcore-go/node"
)

func TestResolveScope(t *testing.T) {
	cases := []struct {
		trigger, bot FloodScope
		want         FloodScope
	}{
		{"", "", ScopeEverywhere},
		{"", "region:nz", "region:nz"},
		{ScopeInherit, "region:nz", "region:nz"},
		{"region:akl", "region:nz", "region:akl"},
		{ScopeEverywhere, "region:nz", ScopeEverywhere},
	}
	for _, c := range cases {
		if got := resolveScope(c.trigger, c.bot); got != c.want {
			t.Errorf("trigger %q bot %q: got %q, want %q", c.trigger, c.bot, got, c.want)
		}
	}
}

func TestFloodScopeValidatedOnLoad(t *testing.T) {
	bad := []struct{ bot, trigger FloodScope }{
		{bot: ScopeInherit},       // nothing above a bot to inherit from
		{bot: "nz"},               // missing region: prefix
		{trigger: "region:#nz"},   // # is added when the key is derived
		{trigger: "region:$priv"}, // a private region has no key
		{trigger: "region:"},
	}
	for _, c := range bad {
		name := "b"
		cfg := &Config{Bots: []BotConfig{{Name: &name, FloodScope: c.bot,
			Triggers: []TriggerConfig{{Type: "cron", FloodScope: c.trigger}}}}}
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), `bot "b"`) {
			t.Errorf("bot %q trigger %q: got %v, want an error naming the bot", c.bot, c.trigger, err)
		}
	}
	name := "b"
	ok := &Config{Bots: []BotConfig{{Name: &name, FloodScope: "region:nz",
		Triggers: []TriggerConfig{{Type: "cron", FloodScope: ScopeInherit}, {Type: "cron", FloodScope: ScopeEverywhere}}}}}
	if err := ok.validate(); err != nil {
		t.Errorf("valid scopes refused: %v", err)
	}
}

type recordingModem struct {
	mu   sync.Mutex
	sent [][]byte
}

func (m *recordingModem) SendData(b []byte) error {
	m.mu.Lock()
	m.sent = append(m.sent, append([]byte(nil), b...))
	m.mu.Unlock()
	return nil
}
func (*recordingModem) SetDataHandler(func([]byte, float32, int8, bool)) {}
func (*recordingModem) AddOutboundHandler(func([]byte))                  {}

func (m *recordingModem) waitFor(t *testing.T, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		if len(m.sent) >= n {
			out := m.sent[:n]
			m.mu.Unlock()
			return out
		}
		m.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("modem sent fewer than %d packets", n)
	return nil
}

// The scope has to reach the air: a region-scoped post is a transport flood
// carrying that region's code, and "everywhere" is a plain flood.
func TestBotPostsCarryTheResolvedScope(t *testing.T) {
	modem := &recordingModem{}
	name := "Scope Bot"
	chans := ChannelList{{Name: "#test"}}
	bot, err := NewBot(BotConfig{
		Name:       &name,
		FloodScope: "region:nz",
		Triggers: []TriggerConfig{
			{Type: "cron", Schedule: "@every 1h", Template: "scoped", Channels: &chans},
			{Type: "cron", Schedule: "@every 1h", Template: "unscoped", Channels: &chans, FloodScope: ScopeEverywhere},
		},
	}, node.NewRadioMux(modem))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	evt := TriggerEvent{Type: "cron", BotName: name, Data: map[string]any{}}

	bot.makeCallback(ctx, bot.triggers[0])(evt)
	scoped, err := meshcore.PacketFromBytes(modem.waitFor(t, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if scoped.RouteType() != meshcore.RouteTypeTransportFlood {
		t.Fatalf("bot-level region: route type %d, want a transport flood", scoped.RouteType())
	}
	if want := meshcore.NewRegion("nz").CalcTransportCode(scoped); scoped.TransportCode1 != want {
		t.Errorf("transport code %04x, want nz's %04x", scoped.TransportCode1, want)
	}

	bot.makeCallback(ctx, bot.triggers[1])(evt)
	plain, err := meshcore.PacketFromBytes(modem.waitFor(t, 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	if plain.RouteType() != meshcore.RouteTypeFlood {
		t.Errorf("trigger everywhere: route type %d, want a plain flood", plain.RouteType())
	}
}
