package cli

import (
	"slices"
	"testing"
)

// TestCancelKeyMap_BindsEsc is the assertion the per-picker literals could not
// carry. The keymap is a plain value, so it needs no form and no TTY — only
// letting the FORM run requires one, because huh opens /dev/tty directly.
//
// Without this, the esc binding was verifiable only by hand: a regression on
// the next huh bump would surface as a picker that reads as stuck, with a
// green suite.
func TestCancelKeyMap_BindsEsc(t *testing.T) {
	km := cancelKeyMap()

	keys := km.Quit.Keys()
	for _, want := range []string{"ctrl+c", "esc"} {
		if !slices.Contains(keys, want) {
			t.Errorf("Quit is not bound to %q (bound: %v) — the picker will read as stuck to anyone pressing it", want, keys)
		}
	}

	// The help text is what the picker actually shows, so a binding with no
	// visible hint is only half the fix.
	if got := km.Quit.Help().Key; got != "esc" {
		t.Errorf("Quit help key = %q, want %q", got, "esc")
	}
	if got := km.Quit.Help().Desc; got != "cancel" {
		t.Errorf("Quit help desc = %q, want %q", got, "cancel")
	}
}
