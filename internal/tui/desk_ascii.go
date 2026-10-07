package tui

import "strings"

// deskASCII maps every mark, border, bar and arrow the desk dashboard draws
// to one ASCII character, for --no-icons (#1107). Each glyph is one cell and
// so is its stand-in, so a frame keeps its layout exactly. Typographic
// characters (·, …, ±) stay: they are punctuation, not icons, and render in
// any terminal font.
//
// It is applied to the drawn frame, so it also changes these glyphs where
// they appear in an item's own text (its WHAT, WHY or a script preview line).
// That is display only; v shows a script's bytes unchanged.
var deskASCII = strings.NewReplacer(
	// row and step marks
	"◌", "o", "●", "*", "◐", "*", "✓", "+", "✗", "x", "⊘", "/", "–", "-",
	"▸", ">", "▶", ">", "⌨", "t", "┆", "|", "↳", ">", "→", ">", "←", "<",
	// panel borders
	"╭", "+", "╮", "+", "╰", "+", "╯", "+", "─", "-", "│", "|", "┤", "|", "├", "|",
	// bars: fill, running, empty, shimmer
	"█", "#", "▓", "=", "░", ".", "▒", "~",
	// sparkline ramp, lowest first (█ is above)
	"▁", "_", "▂", ".", "▃", ":", "▄", "-", "▅", "=", "▆", "+", "▇", "*",
)

// asciiFrame is s with the desk's glyphs in ASCII when ascii is set.
func asciiFrame(s string, ascii bool) string {
	if !ascii {
		return s
	}
	return deskASCII.Replace(s)
}
