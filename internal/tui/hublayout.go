package tui

import (
	"sort"
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

// fitName shortens a command name to width cells. A subcommand path ("pr
// reviewed mark") keeps its head and its tail, cut in the middle, because its
// last word is what tells it from its siblings ("pr reviewed sync", "pr
// reviewed clear"): cutting the tail left all three as "pr reviewed…"
// (forgectl#1102). A single word falls back to fitWords.
func fitName(name string, width int) string {
	if width <= 0 || ansi.StringWidth(name) <= width {
		return fitWords(name, width)
	}
	if !strings.Contains(name, " ") || width < 5 {
		return fitWords(name, width)
	}
	room := width - 1
	head := room / 2
	tail := room - head
	return strings.TrimRight(ansi.Truncate(name, head, ""), " ") + "…" + strings.TrimLeft(ansi.TruncateLeft(name, ansi.StringWidth(name)-tail, ""), " ")
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
			if keep[i] && h != "" {
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
// dropped, or the name was cut (the detail line under the list then shows the
// description whole; the "$ forgectl …" line above it names the command).
func hubRow(name, desc string, used, col, width int) (nameCell, descCell string, cut bool) {
	nameCell = padTo(fitName(name, col), col)
	if desc == "" {
		return strings.TrimRight(nameCell, " "), "", false
	}
	room := width - used - col - 2
	if room < hubDescMin {
		return strings.TrimRight(nameCell, " "), "", true
	}
	descCell = fitWords(desc, room)
	return nameCell, descCell, descCell != desc || ansi.StringWidth(name) > col
}

// rankFilter is the list's filter for every screen: the default fuzzy match,
// with the order made a function of the query and the names only. An exact
// name comes first, then names that start with the query, then the rest in
// fuzzy order. The default order alone left the exact "pr" sixth behind
// "pr repair" and "pip remove", whose fuzzy scores tie with it
// (forgectl#1102).
func rankFilter(term string, targets []string) []list.Rank {
	ranks := list.DefaultFilter(term, targets)
	q := strings.ToLower(strings.TrimSpace(term))
	tier := func(r list.Rank) int {
		name := strings.ToLower(strings.TrimSpace(targets[r.Index]))
		switch {
		case q == "":
			return 2
		case name == q:
			return 0
		case strings.HasPrefix(name, q):
			return 1
		}
		return 2
	}
	sort.SliceStable(ranks, func(i, j int) bool { return tier(ranks[i]) < tier(ranks[j]) })
	return ranks
}

// refilter recomputes the list's filter for the query as it stands now,
// keeping the list in the state it was in (still typing, or accepted).
func (m *model) refilter() {
	state := m.l.FilterState()
	if state == list.Unfiltered {
		return
	}
	m.l.SetFilterText(m.l.FilterValue())
	if state == list.Filtering {
		m.l.SetFilterState(list.Filtering)
	}
}
