package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/node"
)

type ChannelTrigger struct {
	cfg      TriggerConfig
	botName  string
	node     *node.Node
	patterns []*regexp.Regexp
	channels map[string]bool // channel names this trigger listens on; nil = all
	log      *slog.Logger

	mu       sync.Mutex
	callback TriggerCallback
	cancel   context.CancelFunc
}

func NewChannelTrigger(botName string, cfg TriggerConfig, n *node.Node, channels []*meshcore.ChannelEntry, log *slog.Logger) (*ChannelTrigger, error) {
	var patterns []*regexp.Regexp
	if cfg.Match != nil {
		patterns = make([]*regexp.Regexp, 0, len(*cfg.Match))
		for _, m := range *cfg.Match {
			re, err := regexp.Compile(m)
			if err != nil {
				return nil, fmt.Errorf("invalid match pattern %q: %w", m, err)
			}
			patterns = append(patterns, re)
		}
	}

	var channelFilter map[string]bool
	if len(channels) > 0 {
		channelFilter = make(map[string]bool, len(channels))
		for _, ch := range channels {
			channelFilter[ch.Name] = true
		}
	}

	return &ChannelTrigger{
		cfg:      cfg,
		botName:  botName,
		node:     n,
		patterns: patterns,
		channels: channelFilter,
		log:      log.With("trigger", "channel"),
	}, nil
}

func (t *ChannelTrigger) Start(ctx context.Context, callback TriggerCallback) error {
	ctx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	t.callback = callback
	t.cancel = cancel
	t.mu.Unlock()

	t.node.OnPacket(meshcore.PayloadTypeGrpTxt, func(pkt *meshcore.Packet) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		t.handlePacket(pkt)
	})

	return nil
}

func (t *ChannelTrigger) Stop() error {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Unlock()
	return nil
}

func (t *ChannelTrigger) handlePacket(pkt *meshcore.Packet) {
	msg, ch, err := t.node.DecryptGroupText(pkt)
	if err != nil {
		t.log.Log(context.Background(), LevelTrace, "group decrypt failed", "error", err)
		return
	}

	t.log.Log(context.Background(), LevelTrace, "group message received",
		"channel", ch.Name, "sender", msg.Sender,
		"text", msg.Text, "snr", pkt.SNR, "rssi", pkt.RSSI)

	// Ignore our own messages: repeaters echo our sends back to us, and
	// matching them would re-trigger the bot forever.
	if msg.Sender == t.botName {
		t.log.Log(context.Background(), LevelTrace, "ignoring own message")
		return
	}

	if t.channels != nil && !t.channels[ch.Name] {
		t.log.Log(context.Background(), LevelTrace, "channel not matched, skipping",
			"received", ch.Name, "listening", t.channelNames())
		return
	}

	captures := t.matchesAny(msg.Text)
	if captures == nil {
		t.log.Log(context.Background(), LevelTrace, "no pattern matched",
			"channel", ch.Name, "text", msg.Text, "patterns", t.patternStrings())
		return
	}

	t.log.Log(context.Background(), LevelTrace, "trigger matched", "captures", captures)

	t.mu.Lock()
	cb := t.callback
	t.mu.Unlock()
	if cb == nil {
		return
	}

	cb(TriggerEvent{
		Type:    "channel",
		BotName: t.botName,
		Data: map[string]any{
			"Sender":       msg.Sender,
			"Channel":      ch.Name,
			"ChannelEntry": ch,
			"Message":      msg.Text,
			"Match":        captures,
			"Timestamp":    msg.Timestamp,
			"SNR":          pkt.SNR,
			"RSSI":         pkt.RSSI,
			"Hops":         pkt.PathHashCount(),
			"PathHashes":   pkt.PathHashes(),
			"PathHashSize": pkt.PathHashSize(),
		},
	})
}

// matchesAny returns the first matching pattern's named capture groups, or nil
// if no pattern matches. When there are no patterns, it returns an empty
// (non-nil) map to indicate a match-all.
func (t *ChannelTrigger) matchesAny(text string) map[string]string {
	if len(t.patterns) == 0 {
		return map[string]string{} // no patterns = match everything
	}
	for _, re := range t.patterns {
		m := re.FindStringSubmatch(text)
		t.log.Log(context.Background(), LevelTrace, "regex check",
			"pattern", re.String(),
			"text", text, "matched", m != nil)
		if m == nil {
			continue
		}
		captures := make(map[string]string)
		for i, name := range re.SubexpNames() {
			if i == 0 || name == "" {
				continue
			}
			captures[name] = m[i]
		}
		return captures
	}
	return nil
}

func (t *ChannelTrigger) channelNames() []string {
	names := make([]string, 0, len(t.channels))
	for name := range t.channels {
		names = append(names, name)
	}
	return names
}

func (t *ChannelTrigger) patternStrings() []string {
	strs := make([]string, len(t.patterns))
	for i, re := range t.patterns {
		strs[i] = re.String()
	}
	return strs
}

var _ Trigger = (*ChannelTrigger)(nil)
