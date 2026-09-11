// Package keymap holds the key binding forgectl's huh forms share.
//
// It is a leaf so both internal/cli and internal/tui can use it: internal/cli
// imports internal/tui, so the helper cannot live in either.
//
// It briefly also owned the forms' THEME — a dark pin added when the charm v2
// migration exposed huh defaulting to light. internal/theme owns that now, and
// every form takes Theme.Huh(), so the pin is gone rather than kept beside its
// replacement.
package keymap

import (
	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
)

// Cancel is the keymap every forgectl huh form runs with — the one-shot
// pickers in internal/cli and the confirm/rename forms inside the TUI.
//
// huh binds Quit to ctrl+c ALONE (v2.0.3 keymap.go), so Esc is unbound out
// of the box and a form reads as stuck to anyone reaching for the conventional
// cancel key. Every form in this repo needs the same override, and the reason
// it lives here rather than being written out at each site is drift: four
// hand-maintained copies of one literal is four places to miss on the next huh
// bump — and the TUI's footer already promised "esc cancel" while its forms
// did not bind it.
//
// KNOWN SIDE EFFECT, deliberate: huh enables the `/` filter by default on
// MultiSelect (field_multiselect.go:72) and binds ClearFilter to esc
// (v2.0.3 keymap.go:149,164). Re-verified against v2 at the swap. The form
// matches Quit in its own KeyMsg case and returns before the field sees the
// key, so esc during an active filter discards the picker rather than clearing
// the filter. Cancelling is the far more common intent, and the alternative —
// leaving esc dead entirely — is the bug this exists to fix.
func Cancel() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	return km
}
