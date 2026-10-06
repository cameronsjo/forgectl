// Package keymap holds the key bindings and Bubble Tea program setup every
// forgectl TUI and huh form shares.
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
	"context"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
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

// SuspendFilter turns Ctrl+Z into tea.SuspendMsg, so the program releases the
// terminal (leaves the alt screen, shows the cursor, resets paste and keyboard
// modes), stops the process, and redraws in full after `fg` (forgectl#1101).
// Bubble Tea v2 does not bind Ctrl+Z on its own: the key reaches the model as
// an ordinary key press and the screen stays up.
//
// It is a program-level filter, not a per-model key case, so a screen added
// later and the huh forms embedded in a model are covered without each one
// remembering to handle the key.
func SuspendFilter(_ tea.Model, msg tea.Msg) tea.Msg {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "ctrl+z" {
		return tea.SuspendMsg{}
	}
	return msg
}

// ProgramOptions is the setup every forgectl tea.Program starts from: the
// context and the suspend filter. A new program takes these rather than
// calling tea.NewProgram bare.
func ProgramOptions(ctx context.Context) []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithContext(ctx), tea.WithFilter(SuspendFilter)}
}

// Form is the program option a huh form passes to WithProgramOptions so a
// one-shot picker suspends on Ctrl+Z like the full-screen TUIs do.
func Form() tea.ProgramOption { return tea.WithFilter(SuspendFilter) }
