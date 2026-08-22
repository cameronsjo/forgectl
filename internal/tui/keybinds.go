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
		trigger := sanitizeControlBytes(row.Trigger)
		desc = sanitizeControlBytes(desc)
		b.WriteString("  " + triggerCol.Render(trigger) + styleMuted.Render(desc) + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// sanitizeControlBytes strips terminal-control bytes from ghostty CLI output
// (Trigger/Action/Comment all come from the `+list-keybinds` process, itself
// echoing the user's config file verbatim) before it reaches the TTY, so a
// crafted config entry can't inject cursor-control sequences or extra
// physical lines into the rendered sheet. Mirrors forgectl#162's original
// terminal hardening — duplicated rather than
// exported because tui and cli don't share a util package and the two
// render different untrusted-input classes (a remote PR title vs. a local
// config file) for the same reason (raw terminal output). Every C0 control
// byte (0x00-0x1F, including ESC/tab/newline/CR) plus DEL (0x7F) becomes a
// space; everything else, including printable Unicode, passes through.
func sanitizeControlBytes(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
