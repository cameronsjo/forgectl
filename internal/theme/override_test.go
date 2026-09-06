package theme

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestFromConfig_RejectsHostileOverride pins the boundary that actually runs.
//
// config.Load is deliberately tolerant and never calls ThemeConfig.Validate —
// only ValidatePath does, and that runs in doctor, not at startup. So
// FromConfig is the only check between an operator's [theme.colors] value and
// Theme.Hex, whose result `theme show` prints to a terminal. A TOML basic
// string can carry a real ESC via \u, which makes "not a colour" and "a
// control sequence" the same problem.
func TestFromConfig_RejectsHostileOverride(t *testing.T) {
	hostile := map[string]string{
		"escape sequence":   "\x1b[2Kpwned",
		"osc window title":  "\x1b]0;pwned\x07",
		"carriage return":   "#ffffff\rpwned",
		"bidi override":     "#ffffff\u202epwned",
		"newline":           "#ffffff\npwned",
		"not a colour":      "rebeccapurple",
		"shorthand":         "#fff",
		"missing hash":      "ffffff",
		"trailing garbage":  "#ffffff ",
		"eight-digit alpha": "#ffffffff",
	}

	for name, v := range hostile {
		t.Run(name, func(t *testing.T) {
			_, err := FromConfig(config.ThemeConfig{
				Colors: map[string]config.ColorOverride{"accent": {Dark: v, Light: v}},
			})
			if err == nil {
				t.Fatalf("FromConfig accepted %q as a colour", v)
			}
			// The error must name the role so an operator can find the line,
			// and must not echo an unescaped control byte into their terminal
			// while doing it.
			if !strings.Contains(err.Error(), "accent") {
				t.Errorf("error should name the role: %v", err)
			}
			if strings.ContainsAny(err.Error(), "\x1b\r\n") {
				t.Errorf("error echoes a raw control byte: %q", err.Error())
			}
		})
	}

	// Positive control: a real colour still passes, in both spellings.
	for _, good := range []string{"#dbbb6f", "#DBBB6F"} {
		if _, err := FromConfig(config.ThemeConfig{
			Colors: map[string]config.ColorOverride{"accent": {Dark: good, Light: good}},
		}); err != nil {
			t.Errorf("FromConfig rejected the valid colour %q: %v", good, err)
		}
	}

	// An empty side is not a bad colour — it means "no override for that mode".
	if _, err := FromConfig(config.ThemeConfig{
		Colors: map[string]config.ColorOverride{"accent": {Dark: "#dbbb6f"}},
	}); err != nil {
		t.Errorf("FromConfig rejected a dark-only override: %v", err)
	}
}

// TestFromConfig_RejectsDuplicateRoleSpellings pins that one role cannot be
// set twice under different spellings.
//
// Role names match case-insensitively, so `accent` and `ACCENT` are the same
// role — but TOML permits both keys in one table. Without this the winner is
// Go map iteration order, which is randomised: the same config file renders
// different colours run to run, and nothing reports it.
func TestFromConfig_RejectsDuplicateRoleSpellings(t *testing.T) {
	_, err := FromConfig(config.ThemeConfig{Colors: map[string]config.ColorOverride{
		"accent": {Dark: "#111111", Light: "#111111"},
		"ACCENT": {Dark: "#222222", Light: "#222222"},
	}})
	if err == nil {
		t.Fatal("FromConfig accepted the same role under two spellings; map order would pick the winner")
	}
	for _, want := range []string{"accent", "twice"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error should mention %q so the operator can find it: %v", want, err)
		}
	}

	// Two DIFFERENT roles are of course fine.
	if _, err := FromConfig(config.ThemeConfig{Colors: map[string]config.ColorOverride{
		"accent": {Dark: "#111111", Light: "#111111"},
		"fg":     {Dark: "#222222", Light: "#222222"},
	}}); err != nil {
		t.Errorf("FromConfig rejected two distinct roles: %v", err)
	}
}
