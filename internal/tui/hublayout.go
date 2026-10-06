package tui

import (
	"strings"

	"charm.land/bubbles/v2/list"
	"github.com/charmbracelet/x/ansi"
)

// Width-aware layout for the hub screens (forgectl#1074). Every width here is
// a display width in terminal cells, measured per grapheme cluster
// (ansi.StringWidth), never a byte or rune count.

const (
	// hubNameColumnMax caps the name column, so one long recent row ("projects
	// worktree") cannot push every description off a narrow screen.
	hubNameColumnMax = 20
	// hubDescMin is the narrowest description worth drawing. Below it a row
	// shows its name alone and the detail line under the list carries the
	// description instead.
	hubDescMin = 12
	// hubMinWidth and hubMinHeight are the smallest terminal the screens draw
	// in. Below either, View says so instead of drawing a broken layout.
	hubMinWidth  = 20
	hubMinHeight = 8
)

// fitWords shortens s, plain text, to at most width cells. It cuts at the last word
// boundary that fits and ends with "…"; a single word longer than half the
// width is cut mid-word instead, so a row never shows only an ellipsis. A
// trailing comma, colon, or dash before the cut is dropped with it.
func fitWords(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	hard := ansi.Truncate(s, width-1, "")
	cut := strings.LastIndexByte(hard, ' ')
	if rest := s[len(hard):]; strings.HasPrefix(rest, " ") {
		// hard already ends at a word boundary: keep its last word.
		cut = len(hard)
	}
	if cut <= 0 || ansi.StringWidth(hard[:cut]) < width/2 {
		return strings.TrimRight(hard, " ") + "…"
	}
	return strings.TrimRight(hard[:cut], " ,;:—–-·") + "…"
}

// fitHints joins footer hints with " · ", dropping the ones furthest down the
// priority order until the line fits width. hints are in display order; prio
// lists their indexes from most to least important. The first hint in prio is
// kept even when it alone overflows.
func fitHints(width int, hints []string, prio []int) string {
	keep := make([]bool, len(hints))
	for _, i := range prio {
		keep[i] = true
	}
	join := func() string {
		var parts []string
		for i, h := range hints {
			if keep[i] {
				parts = append(parts, h)
			}
		}
		return strings.Join(parts, " · ")
	}
	for n := len(prio) - 1; n > 0 && ansi.StringWidth(join()) > width; n-- {
		keep[prio[n]] = false
	}
	return join()
}

// hubNameColumnMin is the floor under hubNameColumn's width/3 cap, so a
// narrow terminal still shows a whole short name and an area's "›".
const hubNameColumnMin = 10

// hubNameColumn is the name column's width for a list of hub rows: the
// longest name among all of the list's items (not just the visible ones, so
// the column holds still while a filter narrows them), capped. A width of 0
// (no size yet) caps nothing.
func hubNameColumn(items []list.Item, width int) int {
	col := 0
	for _, it := range items {
		var name string
		switch t := it.(type) {
		case hubItem:
			if t.entry.Heading {
				continue
			}
			name = t.label()
		case leafItem:
			name = t.leaf.Name
		default:
			continue
		}
		if w := ansi.StringWidth(name); w > col {
			col = w
		}
	}
	limit := hubNameColumnMax
	if third := max(width/3, hubNameColumnMin); width > 0 && third < limit {
		limit = third
	}
	if col > limit {
		col = limit
	}
	return col
}

// hubRow lays out one hub or leaf row after its leader, which is used cells
// wide: the name padded to col, then the description cut to what is left of
// width. It returns the name and description cells separately so the caller
// can style each, and reports whether the description was shortened or
// dropped (the detail line under the list then shows it whole).
func hubRow(name, desc string, used, col, width int) (nameCell, descCell string, cut bool) {
	nameCell = padTo(fitWords(name, col), col)
	if desc == "" {
		return strings.TrimRight(nameCell, " "), "", false
	}
	room := width - used - col - 2
	if room < hubDescMin {
		return strings.TrimRight(nameCell, " "), "", true
	}
	descCell = fitWords(desc, room)
	return nameCell, descCell, descCell != desc
}
