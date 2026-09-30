package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// migrateLegacyObservers converts a pre-0.4 config with root-level
// [[observers]] into the bot-owned mqtt layout: the observers merge into a
// single MqttConfig attached to the first bot (a name-only bot is created when
// the config has none), and the original file is rewritten in place. The raw
// original is backed up alongside first, since the rewrite loses comments.
func migrateLegacyObservers(path string, raw []byte, cfg *Config) (*Config, error) {
	if len(cfg.Observers) == 0 {
		return cfg, nil
	}

	backupPath := path + ".bak-" + time.Now().Format("20060102-150405")
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(backupPath, raw, mode); err != nil {
		return nil, fmt.Errorf("backing up config before migration: %w", err)
	}

	mqtt := cfg.Observers[0]
	for _, obs := range cfg.Observers[1:] {
		for _, b := range obs.Brokers {
			slog.Warn("merging legacy observer broker into bot mqtt config",
				"from_observer", derefStr(obs.Name), "broker", b.Name)
			mqtt.Brokers = append(mqtt.Brokers, b)
		}
	}

	botName := "mqtt"
	for _, b := range cfg.Bots {
		if b.Mqtt != nil {
			return nil, fmt.Errorf("config has both root observers and a bot mqtt section; consolidate manually (backup at %s)", backupPath)
		}
	}
	if len(cfg.Bots) == 0 {
		cfg.Bots = append(cfg.Bots, BotConfig{Name: &botName})
	}
	cfg.Bots[0].Mqtt = &mqtt
	cfg.Observers = nil

	out, err := marshalConfig(strings.ToLower(filepath.Ext(path)), cfg)
	if err != nil {
		return nil, fmt.Errorf("writing migrated config: %w", err)
	}
	if err := os.WriteFile(path, out, mode); err != nil {
		return nil, fmt.Errorf("writing migrated config: %w", err)
	}

	slog.Info("migrated root observers into bot mqtt config",
		"config", path, "backup", backupPath,
		"brokers_merged", len(mqtt.Brokers), "bot", derefStr(cfg.Bots[0].Name))
	return cfg, nil
}

func marshalConfig(ext string, cfg *Config) ([]byte, error) {
	switch ext {
	case ".yaml", ".yml":
		return yaml.Marshal(cfg)
	case ".json":
		return json.MarshalIndent(cfg, "", "  ")
	case ".toml":
		return toml.Marshal(cfg)
	default:
		return nil, fmt.Errorf("unsupported config format %q", ext)
	}
}
