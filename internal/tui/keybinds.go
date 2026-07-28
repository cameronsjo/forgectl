package tui

import (
	"strings"

	"github.com/cameronsjo/forgectl/internal/ghostty"
	"github.com/cameronsjo/forgectl/internal/meta"
)

// KeybindSheet renders a Ghostty keybind cheatsheet — trigger, action, and
// (where the config annotates it) what it's actually for. Cameron's config
// pairs several bindings with a trailing "# ..." comment naming the
// tmux-passthrough command they send (e.g. "text:\x00c  # New tmux window"),
// which is the one place this sheet can show ghostty-native vs
// tmux-via-keysend side by side (forgectl#7's "ideally" note) — a binding
// with a Comment is annotated with it; one without renders its raw Action.
// Built beside Cheatsheet (tmux), not a refactor of it — that one is
// hard-coded tmux content; this one is driven entirely by rows a caller
// parsed from a live `ghostty +list-keybinds` run.
func KeybindSheet(rows []ghostty.Keybind, noIcons bool) string {
	glyphs := pickGlyphs(noIcons)
	triggerCol := styleAccent.Width(24)

	var b strings.Builder
	title := styleHeader.Render(meta.AppName + " · ghostty keybinds")
	b.WriteString(glyphs.Cheat + " " + title + "\n\n")

	for _, row := range rows {
		desc := row.Action
		if row.Comment != "" {
			desc = row.Comment
		}
		b.WriteString("  " + triggerCol.Render(row.Trigger) + styleMuted.Render(desc) + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}
