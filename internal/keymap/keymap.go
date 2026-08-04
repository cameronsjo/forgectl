// Package keymap holds the key bindings forgectl's huh forms share.
//
// It is a leaf so both internal/cli and internal/tui can use it: internal/cli
// imports internal/tui, so the helper cannot live in either.
package keymap

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
)

// Cancel is the keymap every forgectl huh form runs with — the one-shot
// pickers in internal/cli and the confirm/rename forms inside the TUI.
//
// huh v1.0.0 binds Quit to ctrl+c ALONE (keymap.go:109), so Esc is unbound out
// of the box and a form reads as stuck to anyone reaching for the conventional
// cancel key. Every form in this repo needs the same override, and the reason
// it lives here rather than being written out at each site is drift: four
// hand-maintained copies of one literal is four places to miss on the next huh
// bump — and the TUI's footer already promised "esc cancel" while its forms
// did not bind it.
//
// KNOWN SIDE EFFECT, deliberate: huh enables the `/` filter by default on
// MultiSelect and binds ClearFilter to esc (keymap.go:162-164). The form
// matches Quit in its own KeyMsg case and returns before the field sees the
// key, so esc during an active filter discards the picker rather than clearing
// the filter. Cancelling is the far more common intent, and the alternative —
// leaving esc dead entirely — is the bug this exists to fix.
func Cancel() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	return km
}
