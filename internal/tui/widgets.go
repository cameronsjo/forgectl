package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// The helpers in this file are the desk dashboard's reusable drawing parts.
// They name no colour: every style comes from the theme.Styles the caller
// passes, and each glyph carries its meaning alone, so a no-colour render
// (NO_COLOR, a pipe) still reads correctly.
//
// They do not sanitize. Text that came from a script or a name must go through
// termsafe.SafeLine before it reaches Panel; the helpers measure with display
// width and never interpret what they are given.

// minPanelWidth is the narrowest Panel that can still draw its two corners and
// one border cell; anything smaller is clamped up to it.
const minPanelWidth = 4

// cut shortens s to at most w display cells, ending in "…" when it had to
// cut. SGR sequences in s are preserved and never counted. w <= 0 yields "".
func cut(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// padTo right-pads s with spaces to exactly w display cells (s is assumed to
// be no wider already).
func padTo(s string, w int) string {
	if gap := w - ansi.StringWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// Panel draws a rounded box exactly width cells wide with title set into the
// top border:
//
//	╭ queue ─┤ ● 1 running  ◌ 2 waiting ├────────╮
//	│ line                                        │
//	╰─────────────────────────────────────────────╯
//
// strip is optional (pass "" for none) and sits after the title between ┤ and
// ├; the caller may pre-style it. Each content line is cut with "…" to fit,
// never wrapped, so the height is len(lines)+2 rows. A line must be a single
// row: a newline inside a line is the caller's bug and breaks the box. A
// width under 4 is raised to 4. The title and strip are cut first when the
// top border is too short for them.
func Panel(st theme.Styles, width int, title, strip string, lines []string) string {
	return PanelPadded(st, width, 1, title, strip, lines)
}

// PanelPadded is Panel with pad blank cells between each side border and the
// content, at least 1. Each content line is cut to width-2-2*pad cells.
func PanelPadded(st theme.Styles, width, pad int, title, strip string, lines []string) string {
	pad = max(pad, 1)
	if width < minPanelWidth+2*(pad-1) {
		width = minPanelWidth + 2*(pad-1)
	}
	inner := width - 2
	border := st.Dim

	top := " " + st.Header.Render(title) + " "
	if strip != "" {
		top += border.Render("─┤") + " " + strip + " " + border.Render("├")
	}
	if ansi.StringWidth(top) > inner {
		top = cut(top, inner)
	}
	top += border.Render(strings.Repeat("─", inner-ansi.StringWidth(top)))

	var b strings.Builder
	b.WriteString(border.Render("╭") + top + border.Render("╮"))
	side := border.Render("│")
	gap := strings.Repeat(" ", pad)
	for _, l := range lines {
		b.WriteString("\n" + side + gap + padTo(cut(l, inner-2*pad), inner-2*pad) + gap + side)
	}
	b.WriteString("\n" + border.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return b.String()
}

// sparkRamp is the eight-level ramp, lowest first.
var sparkRamp = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders values as exactly width cells of the ▁▂▃▄▅▆▇█ ramp, scaled
// so the largest value is █. It is unstyled; the caller picks the colour.
//
//   - Zero (and any negative) draws ▁, the baseline, so an empty bucket still
//     shows. A positive value never draws ▁: it is raised to ▂ at minimum, so
//     "none" and "a little" stay distinguishable.
//   - More than width values: the most recent width are kept (the last element
//     is the newest).
//   - Fewer than width values: the series is right-aligned and the missing
//     older cells are spaces, so "no data yet" differs from "zero".
func Sparkline(values []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}
	top := 0.0
	for _, v := range values {
		top = max(top, v)
	}
	out := make([]rune, 0, width)
	for range width - len(values) {
		out = append(out, ' ')
	}
	for _, v := range values {
		idx := 0
		if v > 0 && top > 0 {
			idx = min(1+int(v/top*float64(len(sparkRamp)-2)), len(sparkRamp)-1)
		}
		out = append(out, sparkRamp[idx])
	}
	return string(out)
}

// Seg is one step's state in a segmented Bar.
type Seg int

// Segment states, each with its own glyph so the bar reads without colour.
const (
	SegWaiting Seg = iota // ░ dim
	SegRunning            // ▓ active
	SegDone               // █ ok
	SegFailed             // ✗ danger
)

// look picks a segment's look; unknown states read as waiting.
func (s Seg) look(st theme.Styles) (lipgloss.Style, string) {
	switch s {
	case SegDone:
		return st.OK, "█"
	case SegRunning:
		return st.Active, "▓"
	case SegFailed:
		return st.Danger, "✗"
	default:
		return st.Dim, "░"
	}
}

// barCell is one cell; kind names its style so runs can be merged without
// comparing lipgloss styles.
type barCell struct {
	kind  int
	style lipgloss.Style
	glyph string
}

// Cell kinds. Seg values double as kinds; these follow them.
const (
	kindFill = iota + 100
	kindEmpty
	kindHi
)

// renderCells joins cells, wrapping each run of one kind in a single SGR pair.
func renderCells(cells []barCell) string {
	var b, run strings.Builder
	for i, c := range cells {
		run.WriteString(c.glyph)
		if i == len(cells)-1 || cells[i+1].kind != c.kind {
			b.WriteString(c.style.Render(run.String()))
			run.Reset()
		}
	}
	return b.String()
}

// BarSolid draws a width-cell bar with frac of it filled: █ (Active) then ░
// (Dim). frac is clamped to [0,1] and the filled count rounds to nearest.
func BarSolid(st theme.Styles, width int, frac float64) string {
	if width <= 0 {
		return ""
	}
	frac = min(max(frac, 0), 1)
	filled := int(frac*float64(width) + 0.5)
	cells := make([]barCell, width)
	for i := range cells {
		if i < filled {
			cells[i] = barCell{kindFill, st.Active, "█"}
		} else {
			cells[i] = barCell{kindEmpty, st.Dim, "░"}
		}
	}
	return renderCells(cells)
}

// BarThin draws a width-cell gauge with frac of it filled: a heavy ━ run
// (Active) then a light ─ run (Dim). It carries the same reading as BarSolid
// in lighter glyphs, so a row's gauge does not read as a block. frac is
// clamped to [0,1] and the filled count rounds to nearest.
func BarThin(st theme.Styles, width int, frac float64) string {
	if width <= 0 {
		return ""
	}
	frac = min(max(frac, 0), 1)
	filled := int(frac*float64(width) + 0.5)
	cells := make([]barCell, width)
	for i := range cells {
		if i < filled {
			cells[i] = barCell{kindFill, st.Active, "━"}
		} else {
			cells[i] = barCell{kindEmpty, st.Dim, "─"}
		}
	}
	return renderCells(cells)
}

// BarSegments draws a width-cell bar divided across segs in order, one glyph
// per state (see Seg). Cells are shared out proportionally, so with more
// segments than cells some segments get no cell. No segments reads as all
// waiting.
func BarSegments(st theme.Styles, width int, segs []Seg) string {
	if width <= 0 {
		return ""
	}
	cells := make([]barCell, width)
	for i := range cells {
		state := SegWaiting
		if len(segs) > 0 {
			state = segs[i*len(segs)/width]
		}
		cells[i].kind = int(state)
		cells[i].style, cells[i].glyph = state.look(st)
	}
	return renderCells(cells)
}

// shimmerSpan is the highlight: a ▒▓▒ band, three cells wide.
var shimmerSpan = [3]string{"▒", "▓", "▒"}

// BarShimmer draws an indeterminate width-cell bar: a ▒▓▒ highlight over ░
// that travels left to right, entering and leaving through the edges, then
// repeats. The position is a pure function of frame (any integer, negatives
// included), so a given frame always draws the same bar.
func BarShimmer(st theme.Styles, width, frame int) string {
	if width <= 0 {
		return ""
	}
	cycle := width + len(shimmerSpan)
	pos := (frame%cycle+cycle)%cycle - len(shimmerSpan) // leftmost highlight cell
	cells := make([]barCell, width)
	for i := range cells {
		cells[i] = barCell{kindEmpty, st.Dim, "░"}
		if k := i - pos; k >= 0 && k < len(shimmerSpan) {
			cells[i] = barCell{kindHi + k, st.Active, shimmerSpan[k]}
		}
	}
	return renderCells(cells)
}
