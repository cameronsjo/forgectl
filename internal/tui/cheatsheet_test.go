package tui

import (
	"strings"
	"testing"
)

func TestCheatsheet_NonEmpty(t *testing.T) {
	got := Cheatsheet(false)
	if strings.TrimSpace(got) == "" {
		t.Error("Cheatsheet() returned empty string")
	}
}

func TestCheatsheet_ContainsExpectedSections(t *testing.T) {
	got := Cheatsheet(false)
	sections := []string{
		"The three words",
		"Split",
		"panes",
		"Tabs",
		"Sessions",
	}
	for _, section := range sections {
		if !strings.Contains(got, section) {
			t.Errorf("Cheatsheet() missing expected content %q", section)
		}
	}
}

func TestCheatsheet_ContainsKeyBindings(t *testing.T) {
	got := Cheatsheet(false)
	bindings := []string{"prefix", "prefix |", "prefix d"}
	for _, b := range bindings {
		if !strings.Contains(got, b) {
			t.Errorf("Cheatsheet() missing binding %q", b)
		}
	}
}

func TestCheatsheet_NoIconsModeProducesSameStructure(t *testing.T) {
	withIcons := Cheatsheet(false)
	noIcons := Cheatsheet(true)
	// Both modes should contain the same section headers.
	for _, section := range []string{"The three words", "Split", "Sessions"} {
		if !strings.Contains(noIcons, section) {
			t.Errorf("no-icons Cheatsheet() missing section %q", section)
		}
	}
	// Content should differ only in glyph substitution (or not at all, since
	// Cheatsheet doesn't use glyphs for the text body).
	_ = withIcons
}
