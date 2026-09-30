package main

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

type Templater struct {
	funcMap template.FuncMap
}

func NewTemplater() *Templater {
	return &Templater{funcMap: template.FuncMap{
		"formatPathBytes": formatPathBytes,
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
