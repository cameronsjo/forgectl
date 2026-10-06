// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/resume"
)

// pickerChrome is the columns huh spends around a Select row: the form's
// border and gutter, the `> ` selector, and one column of slack so a full row
// never reaches the last cell (huh wraps a row at the field width, which is
// what split every entry across two lines at 40 columns, forgectl#1105).
const pickerChrome = 6

// pickerDefaultWidth sizes the rows when the terminal width is unknown.
const pickerDefaultWidth = 80

// Column bounds for the picker row. Title takes what the narrower columns
// leave; at a width that cannot hold all three the branch goes first, then
// the repo, so the title and the age always stay.
const (
	pickerTitleMin  = 14
	pickerTitleMax  = 52
	pickerRepoMax   = 20
	pickerBranchMax = 16
)

// pickerTitle is the one thing that tells sessions of one repo apart: the
// /rename name, else the session's last prompt, else a short id. A truncated
// UUID led every row before (#1105) and said nothing.
func pickerTitle(s resume.Session) string {
	if s.Name != "" {
		return safeTitle(s.Name)
	}
	if p := strings.TrimSpace(firstLine(s.LastPrompt)); p != "" {
		return safeTitle(p)
	}
	id := s.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return safeLabel(id)
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// pickerLayout is one picker render's column widths. They are computed once
// over every row, so the repo, branch, and age columns line up.
type pickerLayout struct{ title, repo, branch int }

// layoutPicker sizes the columns to the content and then to the terminal
// (width 0 means unknown). The row is `title  repo  branch  age`; at a width
// that cannot hold all of them the branch goes first, then the repo, so the
// title and the age always stay.
func layoutPicker(sessions []resume.Session, width int) pickerLayout {
	if width <= 0 {
		width = pickerDefaultWidth
	}
	avail := width - pickerChrome

	var repoW, branchW, ageW int
	for _, s := range sessions {
		repoW = max(repoW, displayWidth(safeLabel(s.Repo)))
		branchW = max(branchW, displayWidth(safeLabel(s.Branch)))
		ageW = max(ageW, displayWidth(pickerAge(s)))
	}
	repoW = min(repoW, pickerRepoMax)
	branchW = min(branchW, pickerBranchMax)

	// Budget for the left columns: the row less the age and its separator.
	budget := avail - ageW - 2
	if repoW == 0 || budget < pickerTitleMin+2+repoW {
		repoW = 0
	} else {
		budget -= 2 + repoW
	}
	if repoW == 0 || branchW == 0 || budget < pickerTitleMin+2+branchW {
		branchW = 0
	} else {
		budget -= 2 + branchW
	}
	return pickerLayout{title: min(max(budget, 1), pickerTitleMax), repo: repoW, branch: branchW}
}

// pickerAge is the right-hand column: how long ago, plus a short state note.
func pickerAge(s resume.Session) string {
	age := relativeTime(s.LastActive)
	switch {
	case s.Live:
		age += "  (running)"
	case len(s.Tasks) > 0:
		age += fmt.Sprintf("  (%d tasks)", len(s.Tasks))
	}
	return age
}

// sessionPickerLabel renders one picker row at the layout, so it never wraps.
func sessionPickerLabel(s resume.Session, l pickerLayout) string {
	parts := []string{cell(pickerTitle(s), l.title)}
	if l.repo > 0 {
		parts = append(parts, cell(safeLabel(s.Repo), l.repo))
	}
	if l.branch > 0 {
		parts = append(parts, cell(safeLabel(s.Branch), l.branch))
	}
	parts = append(parts, pickerAge(s))
	return strings.Join(parts, "  ")
}

// sessionSelect is the picker's Select with three additions, none of which
// huh offers a hook for:
//
//   - a header under the title whose hint follows the mode (browsing or
//     typing a filter), because huh's own footer reads `enter set filter •
//     enter submit` and never says how to leave the filter (#1105);
//   - a note, while browsing, that the list was cut at its limit;
//   - a message when `/` filters the list to nothing, instead of a column of
//     blank rows with the typed filter in place of the title.
//
// Every other method is huh's own through the embedded pointer. The Select is
// built with a placeholder Description of the right line count so huh sizes
// the viewport for the header; View swaps the real text in.
type sessionSelect struct {
	*huh.Select[string]
	shown int    // rows in the list
	note  string // sessionPickerNote, or empty
	width int    // terminal columns, to cut the header lines; 0 = no cut
	// muted styles the injected lines like huh's description. Nil leaves them plain.
	muted func(...string) string
}

const (
	browseHint = "enter resume · / filter · esc cancel"
	filterHint = "type to filter · esc cancels"
)

// headerLines is how many lines the header needs, which the placeholder
// Description must match.
func (s sessionSelect) headerLines() int {
	if s.note != "" {
		return 2
	}
	return 1
}

// placeholderDescription is the Description the Select is built with: one
// short line per header line, so huh never wraps it and the count is exact.
func (s sessionSelect) placeholderDescription() string {
	return strings.Repeat("\n.", s.headerLines())[1:]
}

// Update keeps the wrapper in the form's field slot: huh stores whatever
// Update returns, so returning the bare Select would drop the overrides.
func (s sessionSelect) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	m, cmd := s.Select.Update(msg)
	if sel, ok := m.(*huh.Select[string]); ok {
		s.Select = sel
	}
	return s, cmd
}

// View renders the Select, then swaps in the header and, for an empty list,
// the no-match message.
func (s sessionSelect) View() string {
	lines := strings.Split(s.Select.View(), "\n")
	if len(lines) < 1+s.headerLines() {
		return strings.Join(lines, "\n")
	}
	prefix := borderPrefix(lines[0])
	typing := s.GetFiltering()
	filtered := typing || strings.HasPrefix(strings.TrimLeft(strings.Trim(ansi.Strip(lines[0]), "┃ \t"), " "), "/")

	header := []string{browseHint}
	switch {
	case typing:
		header = []string{filterHint}
		if s.note != "" {
			header = append(header, fmt.Sprintf("searches the newest %d only", s.shown))
		}
	case s.note != "" && !filtered:
		header = append(header, s.note)
	case s.note != "":
		header = append(header, "")
	}
	for i := range header {
		lines[1+i] = prefix + s.cut(header[i])
	}

	if _, ok := s.Hovered(); !ok {
		msg := "No match."
		if s.note != "" {
			msg = fmt.Sprintf("No match in the newest %d.", s.shown)
		}
		for i := 1 + s.headerLines(); i < len(lines); i++ {
			// A blank row is the field's border and spaces only.
			if strings.Trim(ansi.Strip(lines[i]), "┃ \t") == "" {
				lines[i] = prefix + s.cut(msg)
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// cut keeps an injected line inside the terminal so it never wraps, and
// styles it like the description it replaces.
func (s sessionSelect) cut(line string) string {
	if s.width > 0 {
		line = truncate(line, s.width-pickerChrome+2)
	}
	if s.muted != nil && line != "" {
		line = s.muted(line)
	}
	return line
}

// KeyBinds is the footer. While a filter is being typed huh's own footer reads
// `enter set filter • enter submit` (two enter verbs, no esc), so it is
// replaced by what the keys do in that mode.
func (s sessionSelect) KeyBinds() []key.Binding {
	if !s.GetFiltering() {
		return s.Select.KeyBinds()
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "move")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "keep filter")),
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	}
}

// borderPrefix is the start of a field line up to and including its left
// border, plus a style reset and a space, so an injected line sits in the
// same gutter. Empty when the line has no border.
func borderPrefix(line string) string {
	if at := strings.IndexRune(line, '┃'); at >= 0 {
		return line[:at+len("┃")] + ansi.ResetStyle + " "
	}
	return ""
}

// sessionPickerNote says what the picker is not showing: the list is the
// newest `limit` sessions, and `/` filters only those rows. Without it a user
// who types a filter for an older session sees no match and cannot tell the
// list was cut.
func sessionPickerNote(shown, limit int) string {
	if limit <= 0 {
		limit = resume.DefaultLimit
	}
	if shown < limit {
		return ""
	}
	return fmt.Sprintf("%d newest · older: resume <text>", shown)
}
