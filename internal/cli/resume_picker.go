// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"strings"

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

// sessionSelect is the picker's Select with one addition: when `/` filters
// the list to nothing, it says so. huh draws a column of blank rows then, and
// the typed filter has replaced the title, so the user lost the context
// (#1105). Every other method is huh's own through the embedded pointer.
type sessionSelect struct{ *huh.Select[string] }

const noMatchesMessage = "No match. Backspace to edit."

// Update keeps the wrapper in the form's field slot: huh stores whatever
// Update returns, so returning the bare Select would drop the override.
func (s sessionSelect) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	m, cmd := s.Select.Update(msg)
	if sel, ok := m.(*huh.Select[string]); ok {
		s.Select = sel
	}
	return s, cmd
}

// View swaps the first blank row of an empty list for the message.
func (s sessionSelect) View() string {
	v := s.Select.View()
	if _, ok := s.Hovered(); ok {
		return v
	}
	lines := strings.Split(v, "\n")
	for i := 1; i < len(lines); i++ {
		// A blank row is the field's border and spaces only.
		if strings.Trim(ansi.Strip(lines[i]), "┃ \t") != "" {
			continue
		}
		prefix := ""
		if at := strings.IndexRune(lines[i], '┃'); at >= 0 {
			prefix = lines[i][:at+len("┃")] + ansi.ResetStyle + " "
		}
		lines[i] = prefix + noMatchesMessage
		break
	}
	return strings.Join(lines, "\n")
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
	return fmt.Sprintf("Newest %d shown. Older: `forgectl resume <text>` or --limit N.", shown)
}
