package main

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
	"time"
	// Embedded so a named zone resolves the same on every release target. Windows has no system
	// zoneinfo and an end user has no GOROOT, so LoadLocation would fail there without this.
	_ "time/tzdata"
)

type Templater struct {
	funcMap template.FuncMap
}

func NewTemplater() *Templater {
	return &Templater{funcMap: template.FuncMap{
		"formatPathBytes": formatPathBytes,
		"now":             time.Now,
		"date":            formatDate,
	}}
}

func (t *Templater) Render(event *TriggerEvent, tmplStr string) (string, error) {
	tmpl, err := template.New("trigger").Funcs(t.funcMap).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	params := map[string]any{
		"Type":    event.Type,
		"BotName": event.BotName,
		"Data":    event.Data,
	}
	for k, v := range event.Data {
		if _, exists := params[k]; !exists {
			params[k] = v
		}
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	result := buf.String()
	result = strings.TrimSpace(result)

	return result, nil
}

func formatPathBytes(paths [][]byte) string {
	if len(paths) == 0 {
		return "Direct"
	}
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = fmt.Sprintf("%02X", p)
	}
	return strings.Join(parts, ", ")
}

// formatDate renders a time in a layout, optionally in a named IANA zone rather than the host's.
// Takes the wire's uint32 unix seconds as readily as a time.Time, because a channel trigger's
// .Timestamp arrives as the former and has no .Format of its own.
func formatDate(value any, layout string, zone ...string) (string, error) {
	if len(zone) > 1 {
		return "", fmt.Errorf("date: want at most one zone, got %d", len(zone))
	}

	t, err := asTime(value)
	if err != nil {
		return "", err
	}

	if len(zone) == 1 && zone[0] != "" {
		loc, err := time.LoadLocation(zone[0])
		if err != nil {
			return "", fmt.Errorf("date: unknown time zone %q", zone[0])
		}
		t = t.In(loc)
	}
	return t.Format(layout), nil
}

// asTime reads the numeric forms a timestamp can arrive as. Zero stays zero rather than becoming
// 1970: a template that prints it should show something obviously unset, not a plausible date.
func asTime(value any) (time.Time, error) {
	switch v := value.(type) {
	case time.Time:
		return v, nil
	case uint32:
		return unixOrZero(int64(v)), nil
	case int64:
		return unixOrZero(v), nil
	case int:
		return unixOrZero(int64(v)), nil
	case uint64:
		return unixOrZero(int64(v)), nil
	default:
		return time.Time{}, fmt.Errorf("date: cannot read a time from %T", value)
	}
}

func unixOrZero(secs int64) time.Time {
	if secs == 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}
