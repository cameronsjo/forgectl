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
	render(index int, selected, narrow bool, width, col int, g glyphSet, s theme.Styles) string
}

// itemDelegate renders rowItems. Height 1 / spacing 0 → compact, mobile-first.
// Hub and leaf rows lay out against the list's width and align their
// descriptions on col, the name column (hubNameColumn), which the model
// computes when the list's items or size change rather than once per row.
type itemDelegate struct {
	g      glyphSet
	narrow bool
	col    int
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
		_, _ = fmt.Fprint(w, ri.render(index, index == m.Index(), d.narrow, m.Width(), d.col, d.g, d.styles))
	}
}

// cursor + number prefix shared by every row: the row's position, 1-9.
func leader(index int, selected bool, s theme.Styles) string {
	key := 0
	if index < 9 {
		key = index + 1
	}
	return keyLeader(key, index, selected, s)
}

// leaderWidth is how many cells leader and keyLeader draw.
const leaderWidth = 3

// keyLeader is leader for a row whose jump key is fixed rather than its
// position (the hub's top screen); key 0 draws no number, and a negative key
// falls back to the row's position (index).
func keyLeader(key, index int, selected bool, s theme.Styles) string {
	if key < 0 {
		return leader(index, selected, s)
	}
	num := "  "
	if key > 0 {
		num = fmt.Sprintf("%d ", key)
	}
	if selected {
		return s.Accent.Render("▌") + s.Accent.Render(num)
	}
	return " " + s.Muted.Render(num)
}

// --- menu ---

type menuItem struct {
	act   menuAct
	label string
	desc  string
	glyph func(g glyphSet) string
}

// menuAct names what a tmux-jumper row does. activate switches on it rather
// than on the row's list index, because the index is a position in the
// FILTERED list once a filter narrows it (#496).
type menuAct int

const (
	menuPick menuAct = iota
	menuSessions
	menuWindows
	menuTree
	menuLast
	menuCheat
)

func (i menuItem) FilterValue() string { return i.label }
func (i menuItem) render(index int, selected, narrow bool, _, _ int, g glyphSet, s theme.Styles) string {
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
// the styler through termsafe.SafeLineMax (termcap.go), the same boundary
// errStatus and setStatus use, and a path through SafePathMax's middle cut.
// TestScreensDrawNothingUnsafe asserts over the whole drawn screen, so a row
// type that skips it fails without the test needing to know it exists. (menuItem above is exempt: its labels are literals in this file.)

// --- pick (sesh candidate) ---

type pickItem string

func (i pickItem) FilterValue() string { return string(i) }
func (i pickItem) render(index int, selected, narrow bool, _, _ int, g glyphSet, s theme.Styles) string {
	label := g.Session + "  " + termsafe.SafeLineMax(string(i), nameMaxRunes)
	if selected {
		return leader(index, true, s) + s.Selected.Render(label)
	}
	return leader(index, false, s) + s.Fg.Render(label)
}

// --- session ---

type sessionItem struct{ s tmux.Session }

func (i sessionItem) FilterValue() string { return i.s.Name }
func (i sessionItem) render(index int, selected, narrow bool, _, _ int, g glyphSet, s theme.Styles) string {
	marker := s.Muted.Render(g.Detached)
	if i.s.Attached {
		marker = s.OK.Render(g.Attached)
	}
	name := termsafe.SafeLineMax(i.s.Name, nameMaxRunes)
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
	meta := fmt.Sprintf("  %d %s · %s", i.s.Windows, unit, termsafe.SafePathMax(i.s.Path, termsafe.PathEchoMaxRunes))
	return row + s.Muted.Render(meta)
}

// --- window ---

type windowItem struct{ w tmux.Window }

func (i windowItem) FilterValue() string { return i.w.Session + " " + i.w.Name }
func (i windowItem) render(index int, selected, narrow bool, _, _ int, g glyphSet, s theme.Styles) string {
	sess := s.Steel.Render(termsafe.SafeLineMax(i.w.Session, nameMaxRunes))
	name := termsafe.SafeLineMax(i.w.Name, nameMaxRunes)
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

// --- hub (module rows, area rows, recent command rows, and section dividers) ---

// hubItem renders one HubEntry row. Name/Short are program-authored (cobra
// names and Short strings compiled into this binary — the "recent" rows are
// command paths resolved against the registered tree, never shell-history
// text), the same trust level as menuItem's literals, so no termsafe
// boundary is needed here.
type hubItem struct{ entry HubEntry }

// FilterValue is empty for a section divider, so a filter never matches one.
func (i hubItem) FilterValue() string {
	if i.entry.Heading {
		return ""
	}
	return i.entry.Name
}

// label is the row's name as drawn: an area row ends in "›", the sign that
// enter opens a list rather than running something.
func (i hubItem) label() string {
	if i.entry.Members != nil {
		return i.entry.Name + " ›"
	}
	return i.entry.Name
}

func (i hubItem) render(index int, selected, _ bool, width, col int, _ glyphSet, s theme.Styles) string {
	if i.entry.Heading {
		heading := "── " + i.entry.Name + " ──"
		if width > 0 {
			heading = fitWords(heading, width-leaderWidth)
		}
		return "   " + s.Muted.Render(heading)
	}
	return renderHubRow(keyLeader(i.entry.Key, index, selected, s), i.label(), i.entry.Short, selected, width, col, s)
}

// renderHubRow draws a hub or leaf row: leader, name column, description. The
// selected row keeps its description, so the row under the cursor is never
// the one row that says nothing.
func renderHubRow(lead, name, desc string, selected bool, width, col int, s theme.Styles) string {
	nameCell, descCell, _ := hubRow(name, desc, leaderWidth, col, width)
	if selected {
		nameCell = s.Selected.Render(nameCell)
	} else {
		nameCell = s.Fg.Render(nameCell)
	}
	if descCell == "" {
		return lead + nameCell
	}
	return lead + nameCell + "  " + s.Muted.Render(descCell)
}

// --- hub leaf (one runnable verb inside a module's drill-down list) ---

type leafItem struct{ leaf HubLeaf }

func (i leafItem) FilterValue() string { return i.leaf.Name }

// desc is the row's description. A leaf needing an argument shows its Use
// line (Architecture: "pr <ref> — needs a ref") rather than its Short, so
// the row itself states what's missing.
func (i leafItem) desc() string {
	if i.leaf.NeedsArgs {
		return i.leaf.Use
	}
	return i.leaf.Short
}

func (i leafItem) render(index int, selected, _ bool, width, col int, _ glyphSet, s theme.Styles) string {
	return renderHubRow(leader(index, selected, s), i.leaf.Name, i.desc(), selected, width, col, s)
}
