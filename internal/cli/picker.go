package cli

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
)

// cancelKeyMap is the keymap every forgectl huh picker runs with.
//
// huh v1.0.0 binds Quit to ctrl+c ALONE (keymap.go:109), so Esc is unbound out
// of the box and a picker reads as stuck to anyone reaching for the
// conventional cancel key. Every picker in this package needs the same
// override, and the reason it lives here rather than being written out at each
// site is drift: three hand-maintained copies of the same literal is three
// places to miss on the next huh bump.
//
// KNOWN SIDE EFFECT, inherited and deliberate: huh enables the `/` filter by
// default on MultiSelect and binds ClearFilter to esc (keymap.go:162-164). The
// form matches Quit in its own KeyMsg case and returns before the field sees
// the key, so esc during an active filter discards the picker rather than
// clearing the filter. Cancelling is the far more common intent, and the
// alternative — leaving esc dead entirely — is the bug this exists to fix.
func cancelKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	return km
}
