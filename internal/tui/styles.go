// Package tui is the thin Bubble Tea layer over internal/tmux. Screens parse
// no tmux output and build no commands — they call the ops Client and render.
package tui

import (
	"os"
)

// glyphSet is the icon vocabulary. iconGlyphs uses Nerd Font glyphs (the
// configured terminal font); asciiGlyphs is the fallback for NO_COLOR or a
// misconfigured Termius profile (--no-icons).
type glyphSet struct {
	Forge    string // header / anvil mark
	Session  string
	Window   string
	Pane     string
	Attached string
	Detached string
	Pick     string
	Tree     string
	Last     string
	Kill     string
	Rename   string
	Cheat    string

	// Run-state marks for the desk dashboard. Each is paired with a word on
	// screen, so colour is never the only carrier. Interrupted marks a step
	// still running when its run ended. Spin holds the running mark's
	// animation frames, one rune each.
	Pending     string
	Running     string
	Done        string
	Failed      string
	Interrupted string
	Rejected    string
	Resumed     string
	Stale       string
	Lost        string
	Skipped     string
	Spin        string
}

var iconGlyphs = glyphSet{
	Pending:     "·",
	Running:     "◐",
	Done:        "✓",
	Failed:      "✗",
	Interrupted: "⊘",
	Rejected:    "↺",
	Resumed:     "⟲",
	Stale:       "!",
	Lost:        "?",
	Skipped:     "–",
	Spin:        "◐◓◑◒",
	Forge:       "", // nf-fa-bolt — the forge spark
	Session:     "", // nf-fa-terminal
	Window:      "", // nf-fa-window_maximize
	Pane:        "", // nf-fa-columns
	Attached:    "●",
	Detached:    "○",
	Pick:        "", // nf-fa-arrow_circle_right
	Tree:        "", // nf-fa-sitemap
	Last:        "", // nf-fa-undo
	Kill:        "", // nf-fa-trash
	Rename:      "", // nf-fa-pencil
	Cheat:       "", // nf-fa-book
}

var asciiGlyphs = glyphSet{
	Pending:     ".",
	Running:     "*",
	Done:        "+",
	Failed:      "x",
	Interrupted: "/",
	Rejected:    "r",
	Resumed:     "~",
	Stale:       "!",
	Lost:        "?",
	Skipped:     "-",
	Spin:        "*",
	Forge:       "#",
	Session:     "s",
	Window:      "w",
	Pane:        "p",
	Attached:    "*",
	Detached:    " ",
	Pick:        ">",
	Tree:        "T",
	Last:        "-",
	Kill:        "x",
	Rename:      "r",
	Cheat:       "?",
}

// pickGlyphs returns the active glyph set, honoring an explicit --no-icons
// preference and NO_COLOR (a no-color terminal almost certainly wants plain
// markers too).
func pickGlyphs(noIcons bool) glyphSet {
	if noIcons || os.Getenv("NO_COLOR") != "" {
		return asciiGlyphs
	}
	return iconGlyphs
}
