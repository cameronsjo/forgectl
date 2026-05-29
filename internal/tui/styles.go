// Package tui is the thin Bubble Tea layer over internal/tmux. Screens parse
// no tmux output and build no commands — they call the ops Client and render.
package tui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
)

// Palette — matched to the live tmux status bar + gitmux on screen (lavender
// accent, Tomorrow-family semantics) so forgectl feels of-a-piece with the
// terminal. Lip Gloss degrades truecolor→256→16 and honors NO_COLOR for us.
// Never use raw hex at a call site; reach for a named style below.
var (
	colorAccent = lipgloss.Color("#B0B9F9") // lavender — tmux status / selection
	colorOK     = lipgloss.Color("#b5bd68") // green — attached
	colorDanger = lipgloss.Color("#cc6666") // red — destructive
	colorActive = lipgloss.Color("#f0c674") // yellow — active window/pane
	colorMuted  = lipgloss.Color("#666666") // metadata
	colorCyan   = lipgloss.Color("#8abeb7") // accents
	colorFg     = lipgloss.Color("#c5c8c6") // default text
)

var (
	styleHeader   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleAccent   = lipgloss.NewStyle().Foreground(colorAccent)
	styleSelected = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleOK       = lipgloss.NewStyle().Foreground(colorOK)
	styleDanger   = lipgloss.NewStyle().Foreground(colorDanger).Bold(true)
	styleActive   = lipgloss.NewStyle().Foreground(colorActive)
	styleMuted    = lipgloss.NewStyle().Foreground(colorMuted)
	styleCyan     = lipgloss.NewStyle().Foreground(colorCyan)
	styleFg       = lipgloss.NewStyle().Foreground(colorFg)
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
}

var iconGlyphs = glyphSet{
	Forge:    "", // nf-fa-bolt — the forge spark
	Session:  "", // nf-fa-terminal
	Window:   "", // nf-fa-window_maximize
	Pane:     "", // nf-fa-columns
	Attached: "●",
	Detached: "○",
	Pick:     "", // nf-fa-arrow_circle_right
	Tree:     "", // nf-fa-sitemap
	Last:     "", // nf-fa-undo
	Kill:     "", // nf-fa-trash
	Rename:   "", // nf-fa-pencil
}

var asciiGlyphs = glyphSet{
	Forge:    "#",
	Session:  "s",
	Window:   "w",
	Pane:     "p",
	Attached: "*",
	Detached: " ",
	Pick:     ">",
	Tree:     "T",
	Last:     "-",
	Kill:     "x",
	Rename:   "r",
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
