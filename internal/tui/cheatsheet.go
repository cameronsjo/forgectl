package tui

import (
	"strings"

	"github.com/cameronsjo/forgectl/internal/meta"
)

// Cheatsheet returns a scannable tmux primer — the three words and the keys
// that actually matter — styled to the forgectl palette. Bindings reflect the
// dotfiles tmux config (prefix = Ctrl+Space). Shared by the TUI cheatsheet
// screen and the `tmux cheat` verb.
func Cheatsheet(noIcons bool) string {
	keyCol := styleAccent.Width(16)

	var b strings.Builder
	title := styleHeader.Render(meta.AppName + " · tmux cheatsheet")
	b.WriteString(title + styleMuted.Render("   (prefix = Ctrl+Space)") + "\n\n")

	section := func(name string) { b.WriteString(styleHeader.Render(name) + "\n") }
	row := func(keys, desc string) {
		b.WriteString("  " + keyCol.Render(keys) + styleMuted.Render(desc) + "\n")
	}

	section("The three words")
	row("session", "a whole workspace — survives disconnect")
	row("window", "a tab inside a session")
	row("pane", "a split inside a window — two things at once")
	b.WriteString("\n")

	section("Split — two things side by side")
	row("prefix |", "split left / right")
	row("prefix -", "split top / bottom")
	row("prefix z", "zoom one pane fullscreen (toggle)")
	b.WriteString("\n")

	section("Move between panes")
	row("prefix h j k l", "← ↓ ↑ →")
	row("prefix H J K L", "resize (hold)")
	b.WriteString("\n")

	section("Tabs (windows)")
	row("prefix c", "new window")
	row("prefix 1…9", "jump to window")
	row("prefix n / p", "next / previous")
	b.WriteString("\n")

	section("Sessions")
	row("prefix T", "session picker (sesh)")
	row("prefix d", "detach — leave it all running")
	row(meta.AppName, "this menu: pick · jump · tree · kill")

	return strings.TrimRight(b.String(), "\n")
}
