package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/node"
)

const DefaultMaxRetries = 3
const DefaultRetryTimeout = int64(5)

type triggerEntry struct {
	trigger  Trigger
	config   TriggerConfig
	channels []*meshcore.ChannelEntry
}

type Bot struct {
	name      string
	node      *node.Node
	radio     node.MuxRadio
	sender    *NodeSender
	templater *Templater
	triggers  []triggerEntry
	log       *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
}

func NewBot(cfg BotConfig, mux *node.RadioMux, nodeOpts ...node.Option) (*Bot, error) {
	if cfg.Name == nil || *cfg.Name == "" {
		return nil, fmt.Errorf("bot name is required")
	}
	botName := *cfg.Name

	identity := identityFromName(botName)
	radio := mux.NewRadio()
	log := slog.Default().With("component", "bot", "bot", botName)
	opts := append([]node.Option{
		node.WithErrorHandler(func(err error) {
			log.Error("node error", "error", err)
		}),
	}, nodeOpts...)
	n := node.New(identity, radio, opts...)

	sender := NewNodeSender(n)

	b := &Bot{
		name:      botName,
		node:      n,
		radio:     radio,
		sender:    sender,
		templater: NewTemplater(),
		log:       log,
	}

	channelIdx := 0
	for _, trigCfg := range cfg.Triggers {
		// Apply Trigger defaults
		if trigCfg.MaxRetries == nil {
			retries := DefaultMaxRetries
			trigCfg.MaxRetries = &retries
		}
		if trigCfg.RetryTimeout == nil {
			retry := DefaultRetryTimeout
			trigCfg.RetryTimeout = &retry
		}

		var channels []*meshcore.ChannelEntry
		if trigCfg.Channels != nil {
			for _, chRef := range *trigCfg.Channels {
				ch, err := channelFromRef(chRef)
				if err != nil {
					return nil, fmt.Errorf("invalid channel %q: %w", chRef.Name, err)
				}
				channels = append(channels, ch)
				n.SetChannel(channelIdx, ch)
				channelIdx++
			}
		}

		entry, err := b.buildTrigger(trigCfg, channels)
		if err != nil {
			return nil, fmt.Errorf("bot %q trigger %q: %w", botName, trigCfg.Type, err)
		}

		b.triggers = append(b.triggers, *entry)
	}

	return b, nil
}

func (b *Bot) buildTrigger(cfg TriggerConfig, channels []*meshcore.ChannelEntry) (*triggerEntry, error) {
	var t Trigger
	var err error

	switch cfg.Type {
	case "channel", "group":
		t, err = NewChannelTrigger(b.name, cfg, b.node, channels, b.log)
	case "cron":
		t, err = NewCronTrigger(b.name, cfg, b.log)
	default:
		return nil, fmt.Errorf("unknown trigger type %q", cfg.Type)
	}
	if err != nil {
		return nil, err
	}

	return &triggerEntry{
		trigger:  t,
		config:   cfg,
		channels: channels,
	}, nil
}

func (b *Bot) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.cancel = cancel
	b.mu.Unlock()

	for i, entry := range b.triggers {
		e := entry
		if err := e.trigger.Start(ctx, b.makeCallback(ctx, e)); err != nil {
			cancel()
			return fmt.Errorf("starting trigger %d (%s): %w", i, e.config.Type, err)
		}
	}

	return nil
}

func (b *Bot) Stop() error {
	b.mu.Lock()
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()

	for _, entry := range b.triggers {
		entry.trigger.Stop()
	}
	b.node.Stop()
	return nil
}

func (b *Bot) makeCallback(ctx context.Context, entry triggerEntry) TriggerCallback {
	return func(evt TriggerEvent) {
		rendered, err := b.templater.Render(&evt, entry.config.Template)
		if err != nil {
			b.log.Error("template error", "error", err)
			return
		}

		b.log.Log(ctx, LevelTrace, "template rendered",
			"trigger", evt.Type, "output", rendered)

		hashSize := resolvePathHashSize(entry.config.PathHashSize, evt)

		retryTimeout := time.Duration(*entry.config.RetryTimeout) * time.Second

		switch evt.Type {
		case "channel":
			ch, _ := evt.Data["ChannelEntry"].(*meshcore.ChannelEntry)
			b.log.Debug("sending group txt", "channel", ch.Name, "pathHashSize", hashSize)
			if err := b.sender.SendGroupText(ctx, ch, b.name, rendered, hashSize, retryTimeout, *entry.config.MaxRetries); err != nil {
				b.log.Error("send error", "error", err)
			}
		case "cron":
			for _, ch := range entry.channels {
				b.log.Debug("sending group txt", "channel", ch.Name, "pathHashSize", hashSize)
				if err := b.sender.SendGroupText(ctx, ch, b.name, rendered, hashSize, retryTimeout, *entry.config.MaxRetries); err != nil {
					b.log.Error("send error", "error", err)
				}
			}
		}
	}
}

func resolvePathHashSize(configured *uint8, evt TriggerEvent) uint8 {
	if configured == nil {
		return 1
	}
	if *configured >= 1 && *configured <= 4 {
		return *configured
	}
	if incoming, ok := evt.Data["PathHashSize"].(uint8); ok && incoming >= 1 && incoming <= 4 {
		return incoming
	}
	return 1
}

func channelFromRef(ref ChannelRef) (*meshcore.ChannelEntry, error) {
	if ref.PrivateKey != "" {
		psk, err := hex.DecodeString(ref.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("invalid hex privateKey for channel %q: %w", ref.Name, err)
		}
		return meshcore.NewChannelFromPSK(ref.Name, psk)
	}
	if strings.EqualFold(ref.Name, "Public") {
		return meshcore.NewChannelFromBase64("Public", "izOH6cXN6mrJ5e26oRXNcg==")
	}
	nCh := meshcore.NormalizeHashtag(ref.Name)
	return meshcore.NewChannelFromHashtag(nCh), nil
}

func identityFromName(name string) meshcore.LocalIdentity {
	hash := sha256.Sum256([]byte(name))
	return meshcore.NewLocalIdentityFromSeed(hash)
}
