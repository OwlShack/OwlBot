package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyToml = `
nodeType = "kiss"
connection = "serial:///dev/ttyACM0"

[[bot]]
name = "Ping Bot"

[[bot.trigger]]
type = "channel"
template = "pong"
channels = ["#testing"]

[[observer]]
name = "Obs One"
iataCode = "akl"

[[observer.broker]]
name = "letsmesh"
enabled = true
host = "mqtt.letsmesh.net"
port = 1883

[[observer]]
name = "Obs Two"
iataCode = "wlg"

[[observer.broker]]
name = "second"
enabled = true
host = "mqtt2.example.net"
port = 8883
`

// TestMigrateLegacyObservers pins the automatic conversion of root-level
// [[observers]] into the single bot-owned [bot.mqtt]: backup first, brokers
// merged into bot[0], observers cleared, and the rewritten file still loads.
func TestMigrateLegacyObservers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(legacyToml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(cfg.Bots) != 1 || cfg.Bots[0].Mqtt == nil {
		t.Fatalf("mqtt not attached to bot[0]: %+v", cfg.Bots)
	}
	if len(cfg.Observers) != 0 {
		t.Fatalf("observers not cleared: %+v", cfg.Observers)
	}
	mqtt := cfg.Bots[0].Mqtt
	if mqtt.Name == nil || *mqtt.Name != "Obs One" {
		t.Fatalf("scalar fields not taken from first observer: %+v", mqtt.Name)
	}
	if len(mqtt.Brokers) != 2 || mqtt.Brokers[1].Host != "mqtt2.example.net" {
		t.Fatalf("brokers not merged: %+v", mqtt.Brokers)
	}

	entries, _ := filepath.Glob(path + ".bak-*")
	if len(entries) != 1 {
		t.Fatalf("want exactly one backup, got %v", entries)
	}
	backup, _ := os.ReadFile(entries[0])
	if !strings.Contains(string(backup), "[[observer]]") {
		t.Fatalf("backup does not contain original config")
	}

	// The rewritten config must be valid on its own (no legacy blocks left).
	reread, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("reloading migrated config: %v", err)
	}
	if reread.Bots[0].Mqtt == nil || len(reread.Bots[0].Mqtt.Brokers) != 2 {
		t.Fatalf("migrated config lost mqtt on reread: %+v", reread.Bots[0].Mqtt)
	}
}

func TestMigrateNoObservers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[[bot]]\nname = \"b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFromPath(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if entries, _ := filepath.Glob(path + ".bak-*"); len(entries) != 0 {
		t.Fatalf("backup written without legacy observers: %v", entries)
	}
}

// TestMigrateBothFormatsErrors: root observers alongside a bot that already
// owns mqtt is ambiguous; it must fail loudly rather than guess.
func TestMigrateBothFormatsErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := legacyToml + "\n[[bot]]\nname = \"Already Mqtt\"\n[bot.mqtt]\nname = \"x\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFromPath(path); err == nil || !strings.Contains(err.Error(), "consolidate manually") {
		t.Fatalf("want ambiguous-config error, got %v", err)
	}
}

func TestValidateSingleMqtt(t *testing.T) {
	dup := `
[[bot]]
name = "a"
[bot.mqtt]
name = "one"

[[bot]]
name = "b"
[bot.mqtt]
name = "two"
`
	if _, err := UnmarshalConfigToml([]byte(dup)); err == nil || !strings.Contains(err.Error(), "at most one bot") {
		t.Fatalf("want single-mqtt violation, got %v", err)
	}
}

// TestSynthesizedBotForOrphanObservers: a legacy config with only observers
// (no bots) gets a name-only bot to own the mqtt section.
func TestSynthesizedBotForOrphanObservers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	only := `
nodeType = "kiss"
connection = "serial:///dev/ttyACM0"

[[observer]]
name = "Solo"
iataCode = "akl"

[[observer.broker]]
name = "letsmesh"
enabled = true
host = "mqtt.letsmesh.net"
port = 1883
`
	if err := os.WriteFile(path, []byte(only), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfigFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Bots) != 1 || cfg.Bots[0].Name == nil || *cfg.Bots[0].Name != "mqtt" || cfg.Bots[0].Mqtt == nil {
		t.Fatalf("synthesized bot wrong: %+v", cfg.Bots)
	}
}
