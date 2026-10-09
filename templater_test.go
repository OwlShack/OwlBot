package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A fixed instant in a zone that is neither UTC nor the test host's, so a zone argument that is
// silently ignored shows up as a wrong hour rather than passing by luck.
var dateRef = time.Date(2026, 9, 9, 18, 30, 5, 0, time.FixedZone("NZST", 12*3600))

func render(t *testing.T, tmpl string, data map[string]any) (string, error) {
	t.Helper()
	return NewTemplater().Render(&TriggerEvent{Type: "cron", BotName: "bot", Data: data}, tmpl)
}

func TestDate_FormatsInNamedZone(t *testing.T) {
	data := map[string]any{"Time": dateRef}
	cases := []struct{ tmpl, want string }{
		{`{{date .Time "2006-01-02 15:04"}}`, "2026-09-09 18:30"},
		{`{{date .Time "15:04" "UTC"}}`, "06:30"},
		{`{{date .Time "15:04" "Pacific/Auckland"}}`, "18:30"},
		{`{{date .Time "15:04" "America/New_York"}}`, "02:30"},
		{`{{date .Time "2006-01-02" "UTC"}}`, "2026-09-09"},
	}
	for _, c := range cases {
		got, err := render(t, c.tmpl, data)
		if err != nil {
			t.Errorf("%s: %v", c.tmpl, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.tmpl, got, c.want)
		}
	}
}

// The wire hands a channel trigger its timestamp as uint32 seconds, which has no .Format of its own.
func TestDate_ReadsWireTimestamp(t *testing.T) {
	data := map[string]any{"Timestamp": uint32(dateRef.Unix())}
	got, err := render(t, `{{date .Timestamp "15:04" "UTC"}}`, data)
	if err != nil {
		t.Fatal(err)
	}
	if got != "06:30" {
		t.Errorf("got %q, want %q", got, "06:30")
	}
}

// An unset timestamp must not read as a real date in 1970.
func TestDate_ZeroTimestampIsNotEpoch(t *testing.T) {
	got, err := render(t, `{{date .Timestamp "2006"}}`, map[string]any{"Timestamp": uint32(0)})
	if err != nil {
		t.Fatal(err)
	}
	if got == "1970" {
		t.Error("zero rendered as 1970; absent is not a date")
	}
	if got != "0001" {
		t.Errorf("got %q, want %q", got, "0001")
	}
}

func TestDate_RejectsUnknownZone(t *testing.T) {
	_, err := render(t, `{{date .Time "15:04" "Mars/Olympus"}}`, map[string]any{"Time": dateRef})
	if err == nil {
		t.Fatal("want an error for an unknown zone")
	}
	if !strings.Contains(err.Error(), "unknown time zone") {
		t.Errorf("error should name the problem, got %v", err)
	}
}

func TestDate_RejectsUnreadableValue(t *testing.T) {
	_, err := render(t, `{{date .Nope "15:04"}}`, map[string]any{"Nope": "not a time"})
	if err == nil {
		t.Fatal("want an error for a value that is not a time")
	}
}

func TestNow_IsATimeWithMethods(t *testing.T) {
	got, err := render(t, `{{now.Year}}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != time.Now().Format("2006") {
		t.Errorf("got %q, want the current year", got)
	}

	// The point of returning a time.Time rather than a string: it composes with date and zones.
	if _, err := render(t, `{{date now "15:04" "UTC"}}`, nil); err != nil {
		t.Errorf("now should compose with date: %v", err)
	}
}

// The blank time/tzdata import looks unused and is the kind of line that gets tidied away. Losing
// it breaks named zones on Windows and in any minimal container, neither of which CI can run, so
// the guarantee is asserted against the build graph instead of the host's zoneinfo.
func TestEmbeddedTZDataIsLinked(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if dep == "time/tzdata" {
			return
		}
	}
	t.Error("time/tzdata is not linked in; named zones would fail wherever the host has no zoneinfo")
}

func TestFormatPathBytes_Separator(t *testing.T) {
	data := map[string]any{"PathHashes": [][]byte{{0xa1}, {0xb2}, {0xc3}}}
	cases := []struct{ tmpl, want string }{
		{`{{formatPathBytes .PathHashes}}`, "A1, B2, C3"},
		{`{{.PathHashes | formatPathBytes}}`, "A1, B2, C3"}, // the form config.toml.example uses
		{`{{formatPathBytes .PathHashes " > "}}`, "A1 > B2 > C3"},
		{`{{formatPathBytes .PathHashes ""}}`, "A1B2C3"},
		{`{{formatPathBytes .PathHashes "\n"}}`, "A1\nB2\nC3"},
	}
	for _, c := range cases {
		got, err := render(t, c.tmpl, data)
		if err != nil {
			t.Errorf("%s: %v", c.tmpl, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.tmpl, got, c.want)
		}
	}
}

// An empty path is "Direct" whatever the separator: there is nothing to join.
func TestFormatPathBytes_DirectIgnoresSeparator(t *testing.T) {
	data := map[string]any{"PathHashes": [][]byte{}}
	for _, tmpl := range []string{
		`{{formatPathBytes .PathHashes}}`,
		`{{formatPathBytes .PathHashes " > "}}`,
	} {
		got, err := render(t, tmpl, data)
		if err != nil {
			t.Errorf("%s: %v", tmpl, err)
			continue
		}
		if got != "Direct" {
			t.Errorf("%s = %q, want %q", tmpl, got, "Direct")
		}
	}
}

func TestFormatPathBytes_RejectsExtraArgs(t *testing.T) {
	data := map[string]any{"PathHashes": [][]byte{{0xa1}}}
	if _, err := render(t, `{{formatPathBytes .PathHashes ">" "<"}}`, data); err == nil {
		t.Fatal("a second separator must error, matching date's one-zone rule, not be silently dropped")
	}
}
