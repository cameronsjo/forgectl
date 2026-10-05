// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package keymap

import "charm.land/bubbles/v2/key"

// Desk is the desk dashboard's key bindings. The dashboard matches keys
// against these and bubbles/help renders them, so the footer and the `?`
// screen cannot drift from what the keys do.
type Desk struct {
	PrevRun, NextRun key.Binding
	Switcher         key.Binding
	Views            key.Binding // 1-6, shown as one entry; the dashboard matches the digits itself
	NextPane         key.Binding
	Up, Down         key.Binding
	Top, Bottom      key.Binding
	Open             key.Binding
	Replay           key.Binding
	StepBack, StepFw key.Binding
	Autoplay         key.Binding
	Approve          key.Binding
	Confirm, Cancel  key.Binding
	Skip, Unskip     key.Binding
	Filter           key.Binding
	Refresh          key.Binding
	RefreshAll       key.Binding
	Mouse            key.Binding
	Help             key.Binding
	Back             key.Binding
	Quit             key.Binding
	ForceQuit        key.Binding
}

// NewDesk returns the dashboard's bindings.
func NewDesk() Desk {
	return Desk{
		PrevRun:    key.NewBinding(key.WithKeys("[", "<"), key.WithHelp("[ <", "prev run")),
		NextRun:    key.NewBinding(key.WithKeys("]", ">"), key.WithHelp("] >", "next run")),
		Switcher:   key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "switch run")),
		Views:      key.NewBinding(key.WithKeys("1", "2", "3", "4", "5", "6"), key.WithHelp("1-6", "view")),
		NextPane:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next pane")),
		Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Top:        key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
		Bottom:     key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom, follow")),
		Open:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Replay:     key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "replay")),
		StepBack:   key.NewBinding(key.WithKeys("left"), key.WithHelp("←", "step back")),
		StepFw:     key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "step forward")),
		Autoplay:   key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "autoplay")),
		Approve:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve")),
		Confirm:    key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "approve (in the approve dialog)")),
		Cancel:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "cancel")),
		Skip:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "skip")),
		Unskip:     key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unskip")),
		Filter:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh run")),
		RefreshAll: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh all")),
		Mouse:      key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mouse on/off")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Back:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Quit:       key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
		ForceQuit:  key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

// All lists every binding once, for tests and the full help.
func (k Desk) All() []key.Binding {
	return []key.Binding{
		k.PrevRun, k.NextRun, k.Switcher, k.Views, k.NextPane, k.Up, k.Down, k.Top, k.Bottom,
		k.Open, k.Replay, k.StepBack, k.StepFw, k.Autoplay, k.Approve, k.Confirm, k.Cancel, k.Skip, k.Unskip,
		k.Filter, k.Refresh, k.RefreshAll, k.Mouse, k.Help, k.Back, k.Quit, k.ForceQuit,
	}
}

// DeskMode says which keys apply now, so the footer lists only those.
type DeskMode int

const (
	// DeskBrowse is the normal screen.
	DeskBrowse DeskMode = iota
	// DeskQueue is the queue view, where the approval verbs apply.
	DeskQueue
	// DeskFilter is the filter prompt.
	DeskFilter
	// DeskHelp is the help screen.
	DeskHelp
	// DeskConfirm is the approve dialog.
	DeskConfirm
	// DeskModal is a read dialog: a step, an event, or the run switcher.
	DeskModal
	// DeskReplay is a replay of the selected run.
	DeskReplay
)

// DeskHelpMap adapts the bindings to bubbles/help for one mode.
type DeskHelpMap struct {
	Keys Desk
	Mode DeskMode
}

// ShortHelp is the one-line footer for the mode.
func (h DeskHelpMap) ShortHelp() []key.Binding {
	k := h.Keys
	switch h.Mode {
	case DeskFilter:
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "keep")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear")),
		}
	case DeskHelp:
		return []key.Binding{k.Help, k.Quit}
	case DeskConfirm:
		return []key.Binding{k.Confirm, k.Cancel, k.Back, k.Down, k.Bottom}
	case DeskModal:
		return []key.Binding{k.Up, k.Open, k.Back, k.Top, k.Bottom}
	case DeskReplay:
		return []key.Binding{k.StepBack, k.StepFw, k.Autoplay, k.Top, k.Bottom, k.Replay, k.Back}
	case DeskQueue:
		return []key.Binding{k.Up, k.Approve, k.Skip, k.Unskip, k.Views, k.Refresh, k.Help, k.Quit}
	default:
		return []key.Binding{k.PrevRun, k.Views, k.NextPane, k.Up, k.Filter, k.Refresh, k.Help, k.Quit}
	}
}

// FullHelp is the grouped list for the help screen.
func (h DeskHelpMap) FullHelp() [][]key.Binding {
	k := h.Keys
	return [][]key.Binding{
		{k.PrevRun, k.NextRun, k.Switcher, k.Views, k.NextPane},
		{k.Up, k.Down, k.Top, k.Bottom, k.Open},
		{k.Replay, k.StepBack, k.StepFw, k.Autoplay},
		{k.Approve, k.Confirm, k.Cancel, k.Skip, k.Unskip, k.Filter},
		{k.Refresh, k.RefreshAll, k.Mouse, k.Help, k.Back, k.Quit, k.ForceQuit},
	}
}
