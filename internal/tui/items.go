package tui

import (
	"fmt"
	"io"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// rowItem is a list item that knows how to render itself in one line, with the
// glyph set, narrow-mode preference, and theme styles supplied by the
// delegate. Single-line rows keep the lists readable at ~40 columns
// (iPhone/Termius).
type rowItem interface {
	list.Item
	render(index int, selected, narrow bool, g glyphSet, s theme.Styles) string
}

// itemDelegate renders rowItems. Height 1 / spacing 0 → compact, mobile-first.
type itemDelegate struct {
	g      glyphSet
	narrow bool
	styles theme.Styles
}

func (d itemDelegate) Height() int                         { return 1 }
func (d itemDelegate) Spacing() int                        { return 0 }
func (d itemDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	if ri, ok := item.(rowItem); ok {
		// w is Bubble Tea's list renderer writing into an in-memory buffer, not
		// a real sink — a write failure here isn't actionable, and Render's
		// signature (no error return) gives nothing to propagate it to anyway.
		_, _ = fmt.Fprint(w, ri.render(index, index == m.Index(), d.narrow, d.g, d.styles))
	}
}

// cursor + number prefix shared by every row.
func leader(index int, selected bool, s theme.Styles) string {
	num := "  "
	if index < 9 {
		num = fmt.Sprintf("%d ", index+1)
	}
	if selected {
		return s.Accent.Render("▌") + s.Accent.Render(num)
	}
	return " " + s.Muted.Render(num)
}

// --- menu ---

type menuItem struct {
	label string
	desc  string
	glyph func(g glyphSet) string
}

func (i menuItem) FilterValue() string { return i.label }
func (i menuItem) render(index int, selected, narrow bool, g glyphSet, s theme.Styles) string {
	label := i.glyph(g) + "  " + i.label
	if selected {
		return leader(index, true, s) + s.Selected.Render(label)
	}
	if narrow {
		return leader(index, false, s) + s.Fg.Render(label)
	}
	return leader(index, false, s) + s.Fg.Render(label) + "  " + s.Muted.Render(i.desc)
}

// Every row from here down renders text forgectl did not compose: session
// names, window names, and working directories are chosen by whoever created
// the object, which is any same-uid process. The TUI redraws the whole screen
// on every keystroke, so an escape sequence in one name repaints the chrome
// around it and a bidi override reorders the row — which is why each reaches
// the styler through termsafe.SafeLine, the same boundary errStatus and
// setStatus use. TestScreensDrawNothingUnsafe asserts over the whole drawn
// screen, so a row type that skips it fails without the test needing to know
// it exists. (menuItem above is exempt: its labels are literals in this file.)

// --- pick (sesh candidate) ---

type pickItem string

func (i pickItem) FilterValue() string { return string(i) }
func (i pickItem) render(index int, selected, narrow bool, g glyphSet, s theme.Styles) string {
	label := g.Session + "  " + termsafe.SafeLine(string(i))
	if selected {
		return leader(index, true, s) + s.Selected.Render(label)
	}
	return leader(index, false, s) + s.Fg.Render(label)
}

// --- session ---

type sessionItem struct{ s tmux.Session }

func (i sessionItem) FilterValue() string { return i.s.Name }
func (i sessionItem) render(index int, selected, narrow bool, g glyphSet, s theme.Styles) string {
	marker := s.Muted.Render(g.Detached)
	if i.s.Attached {
		marker = s.OK.Render(g.Attached)
	}
	name := termsafe.SafeLine(i.s.Name)
	if selected {
		name = s.Selected.Render(name)
	} else {
		name = s.Fg.Render(name)
	}
	row := leader(index, selected, s) + marker + " " + name
	if narrow {
		return row
	}
	unit := "windows"
	if i.s.Windows == 1 {
		unit = "window"
	}
	meta := fmt.Sprintf("  %d %s · %s", i.s.Windows, unit, termsafe.SafeLine(i.s.Path))
	return row + s.Muted.Render(meta)
}

// --- window ---

type windowItem struct{ w tmux.Window }

func (i windowItem) FilterValue() string { return i.w.Session + " " + i.w.Name }
func (i windowItem) render(index int, selected, narrow bool, g glyphSet, s theme.Styles) string {
	sess := s.Steel.Render(termsafe.SafeLine(i.w.Session))
	name := termsafe.SafeLine(i.w.Name)
	if i.w.Active {
		name = s.Active.Render(name)
	} else if selected {
		name = s.Selected.Render(name)
	} else {
		name = s.Fg.Render(name)
	}
	row := leader(index, selected, s) + g.Window + " " + sess + s.Muted.Render(" · ") + name
	if narrow {
		return row
	}
	unit := "panes"
	if i.w.Panes == 1 {
		unit = "pane"
	}
	return row + s.Muted.Render(fmt.Sprintf("  %d %s", i.w.Panes, unit))
}
