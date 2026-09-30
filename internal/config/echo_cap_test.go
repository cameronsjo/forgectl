package config

import (
	"strings"
	"testing"
)

// TestValidationEchoIsCapped is #706: a rejected config value is echoed so
// the operator can see what they wrote, but quoted with control characters
// escaped and capped, so a pasted blob cannot flood the terminal or the log.
func TestValidationEchoIsCapped(t *testing.T) {
	long := "\x1b[2J" + strings.Repeat("A", 500)
	cases := map[string]error{
		"theme preset":     ThemeConfig{Preset: long}.Validate(),
		"theme mode":       ThemeConfig{Mode: long}.Validate(),
		"theme colour key": ThemeConfig{Colors: map[string]ColorOverride{long: {Dark: "#000000"}}}.Validate(),
		"theme colour":     ThemeConfig{Colors: map[string]ColorOverride{"accent": {Dark: long, Light: "#000000"}}}.Validate(),
		"search backend":   DocsConfig{SearchBackend: long}.Validate(),
		"root kind":        DocsConfig{RootKinds: map[string]string{"/x": long}}.Validate(),
		"launch profile": func() error {
			_, _, err := ProxyConfig{LaunchProfile: long}.ResolveLaunchProfile()
			return err
		}(),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: validation passed a %d-rune value", name, len(long))
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "\x1b") || strings.Contains(msg, strings.Repeat("A", 81)) {
			t.Errorf("%s: error echoes the value uncapped or unescaped: %q", name, msg)
		}
		if !strings.Contains(msg, `\x1b[2J`) || !strings.Contains(msg, "…") {
			t.Errorf("%s: error = %q, want the escaped, capped value", name, msg)
		}
	}
}

// TestColorOverrideUnknownKeysAreCapped: the unknown-key list in a colour
// table is quoted, capped per key, and cut after a few keys (#706).
func TestColorOverrideUnknownKeysAreCapped(t *testing.T) {
	table := map[string]any{"dark": "#000000"}
	for i := 0; i < 20; i++ {
		table[string(rune('a'+i))+"\x1b"+strings.Repeat("K", 200)] = "#ffffff"
	}
	var co ColorOverride
	err := co.UnmarshalTOML(table)
	if err == nil {
		t.Fatal("UnmarshalTOML accepted unknown keys")
	}
	msg := err.Error()
	if strings.Contains(msg, "\x1b") || strings.Contains(msg, strings.Repeat("K", 81)) {
		t.Errorf("error echoes a key uncapped or unescaped: %q", msg)
	}
	if n := strings.Count(msg, `\x1b`); n != 5 {
		t.Errorf("error shows %d keys, want 5 then an ellipsis: %q", n, msg)
	}
}
