package tui

import (
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// rowItem is a list item that knows how to render itself in one line, with the
// glyph set and narrow-mode preference supplied by the delegate. Single-line
// rows keep the lists readable at ~40 columns (iPhone/Termius).
type rowItem interface {
	list.Item
	render(index int, selected, narrow bool, g glyphSet) string
}

// itemDelegate renders rowItems. Height 1 / spacing 0 → compact, mobile-first.
type itemDelegate struct {
	g      glyphSet
	narrow bool
}

func (d itemDelegate) Height() int                         { return 1 }
func (d itemDelegate) Spacing() int                        { return 0 }
func (d itemDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	if ri, ok := item.(rowItem); ok {
		fmt.Fprint(w, ri.render(index, index == m.Index(), d.narrow, d.g))
	}
}

// cursor + number prefix shared by every row.
func leader(index int, selected bool) string {
	num := "  "
	if index < 9 {
		num = fmt.Sprintf("%d ", index+1)
	}
	if selected {
		return styleAccent.Render("▌") + styleAccent.Render(num)
	}
	return " " + styleMuted.Render(num)
}

// --- menu ---

type menuItem struct {
	label string
	desc  string
	glyph func(g glyphSet) string
}

func (i menuItem) FilterValue() string { return i.label }
func (i menuItem) render(index int, selected, narrow bool, g glyphSet) string {
	label := i.glyph(g) + "  " + i.label
	if selected {
		return leader(index, true) + styleSelected.Render(label)
	}
	if narrow {
		return leader(index, false) + styleFg.Render(label)
	}
	return leader(index, false) + styleFg.Render(label) + "  " + styleMuted.Render(i.desc)
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
func (i pickItem) render(index int, selected, narrow bool, g glyphSet) string {
	label := g.Session + "  " + termsafe.SafeLine(string(i))
	if selected {
		return leader(index, true) + styleSelected.Render(label)
	}
	return leader(index, false) + styleFg.Render(label)
}

// --- session ---

type sessionItem struct{ s tmux.Session }

func (i sessionItem) FilterValue() string { return i.s.Name }
func (i sessionItem) render(index int, selected, narrow bool, g glyphSet) string {
	marker := styleMuted.Render(g.Detached)
	if i.s.Attached {
		marker = styleOK.Render(g.Attached)
	}
	name := termsafe.SafeLine(i.s.Name)
	if selected {
		name = styleSelected.Render(name)
	} else {
		name = styleFg.Render(name)
	}
	row := leader(index, selected) + marker + " " + name
	if narrow {
		return row
	}
	unit := "windows"
	if i.s.Windows == 1 {
		unit = "window"
	}
	meta := fmt.Sprintf("  %d %s · %s", i.s.Windows, unit, termsafe.SafeLine(i.s.Path))
	return row + styleMuted.Render(meta)
}

// --- window ---

type windowItem struct{ w tmux.Window }

func (i windowItem) FilterValue() string { return i.w.Session + " " + i.w.Name }
func (i windowItem) render(index int, selected, narrow bool, g glyphSet) string {
	sess := styleCyan.Render(termsafe.SafeLine(i.w.Session))
	name := termsafe.SafeLine(i.w.Name)
	if i.w.Active {
		name = styleActive.Render(name)
	} else if selected {
		name = styleSelected.Render(name)
	} else {
		name = styleFg.Render(name)
	}
	row := leader(index, selected) + g.Window + " " + sess + styleMuted.Render(" · ") + name
	if narrow {
		return row
	}
	unit := "panes"
	if i.w.Panes == 1 {
		unit = "pane"
	}
	return row + styleMuted.Render(fmt.Sprintf("  %d %s", i.w.Panes, unit))
}
