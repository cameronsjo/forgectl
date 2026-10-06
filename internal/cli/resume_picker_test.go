package cli

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/resume"
)

func pickerSessions() []resume.Session {
	now := time.Now()
	return []resume.Session{
		{ID: "00000000-0000-4000-8000-000000000000", Repo: "alpha", Branch: "main", LastPrompt: "Fix the flaky retry logic in the scheduler", LastActive: now.Add(-13 * time.Minute)},
		{ID: "00000001-0000-4000-8000-000000000001", Repo: "beta", Branch: "feat/a-long-branch-name", Name: "release prep", LastPrompt: "other", LastActive: now.Add(-4 * time.Hour)},
		{ID: "00000002-0000-4000-8000-000000000002", Repo: "gamma-with-a-very-long-repository-name", LastPrompt: "日本語のプロンプトがここにありますよ 日本語のプロンプトがここにありますよ", LastActive: now.Add(-50 * time.Hour), Live: true, Pid: 42},
		{ID: "00000003-0000-4000-8000-000000000003", LastActive: now.Add(-100 * time.Hour)},
	}
}

// TestSessionPickerLabel_FitsAndDescribes pins #1105 problems 1 and 4: a row
// leads with something that tells sessions apart (name, else prompt) and is
// never wider than the terminal allows, so huh does not wrap it.
func TestSessionPickerLabel_FitsAndDescribes(t *testing.T) {
	sessions := pickerSessions()
	for _, width := range []int{40, 60, 80, 120, 0} {
		l := layoutPicker(sessions, width)
		limit := width - pickerChrome
		if width == 0 {
			limit = pickerDefaultWidth - pickerChrome
		}
		for _, s := range sessions {
			row := sessionPickerLabel(s, l)
			if w := ansi.StringWidth(row); w > limit {
				t.Errorf("width %d: row is %d cells, limit %d: %q", width, w, limit, row)
			}
			if !strings.Contains(row, "ago") {
				t.Errorf("width %d: row lost its age: %q", width, row)
			}
		}
	}

	l := layoutPicker(sessions, 100)
	first := sessionPickerLabel(sessions[0], l)
	if !strings.HasPrefix(first, "Fix the flaky retry logic") {
		t.Errorf("an unnamed session must lead with its prompt, got %q", first)
	}
	if strings.Contains(first, "00000000") {
		t.Errorf("the id must not lead the row: %q", first)
	}
	if got := sessionPickerLabel(sessions[1], l); !strings.HasPrefix(got, "release prep") {
		t.Errorf("a named session must lead with its name, got %q", got)
	}
	if got := sessionPickerLabel(sessions[3], l); !strings.HasPrefix(got, "00000003") {
		t.Errorf("a session with no name or prompt falls back to a short id, got %q", got)
	}
}

func TestLayoutPicker_DropsBranchThenRepo(t *testing.T) {
	sessions := []resume.Session{{ID: "x", Repo: "repository", Branch: "feature/abcdefg", LastPrompt: "p", LastActive: time.Now()}}
	wide := layoutPicker(sessions, 120)
	if wide.repo == 0 || wide.branch == 0 {
		t.Errorf("120 cols should hold every column, got %+v", wide)
	}
	mid := layoutPicker(sessions, 50)
	if mid.branch != 0 || mid.repo == 0 {
		t.Errorf("50 cols should drop the branch and keep the repo, got %+v", mid)
	}
	narrow := layoutPicker(sessions, 40)
	if narrow.repo != 0 || narrow.branch != 0 || narrow.title < 1 {
		t.Errorf("40 cols should keep only the title and age, got %+v", narrow)
	}
}

func TestLayoutPicker_ColumnsAlign(t *testing.T) {
	sessions := pickerSessions()
	l := layoutPicker(sessions, 120)
	want := -1
	for _, s := range sessions {
		left := strings.TrimSuffix(sessionPickerLabel(s, l), pickerAge(s))
		if want >= 0 && ansi.StringWidth(left) != want {
			t.Errorf("the age column moved between rows: %d vs %d", ansi.StringWidth(left), want)
		}
		want = ansi.StringWidth(left)
	}
}

// TestSessionPickerNote pins #1105 problem 3: a list cut at the limit says so.
func TestSessionPickerNote(t *testing.T) {
	if got := sessionPickerNote(25, 25); !strings.Contains(got, "25") || !strings.Contains(got, "--limit") {
		t.Errorf("a list cut at its limit must say so and name --limit, got %q", got)
	}
	if got := sessionPickerNote(25, 0); got == "" {
		t.Error("limit 0 means the default of 25; a full list must still say so")
	}
	if got := sessionPickerNote(7, 25); got != "" {
		t.Errorf("a list under its limit needs no note, got %q", got)
	}
}

func typeKeys(m huh.Model, keys ...string) huh.Model {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		m, _ = m.Update(msg)
	}
	return m
}

// TestSessionSelect_EmptyFilterSaysSo pins #1105 problem 5: a filter with no
// match shows a message instead of a column of blank rows, and the wrapper
// survives huh's Update round trip (huh stores whatever Update returns).
func TestSessionSelect_EmptyFilterSaysSo(t *testing.T) {
	var chosen string
	sel := huh.NewSelect[string]().Title("Recent sessions").
		Options(huh.NewOption("alpha one", "a"), huh.NewOption("beta two", "b")).
		Value(&chosen).WithKeyMap(keymap.Cancel()).WithWidth(60).WithHeight(8)
	var m huh.Model = sessionSelect{sel.(*huh.Select[string])}
	sel.Focus()

	m = typeKeys(m, "/", "z", "z")
	if _, ok := m.(sessionSelect); !ok {
		t.Fatalf("Update returned %T; the wrapper was dropped from the form", m)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, noMatchesMessage) {
		t.Errorf("an empty filter must show %q, got:\n%s", noMatchesMessage, v)
	}

	m = typeKeys(m, "backspace", "backspace", "a")
	if v := ansi.Strip(m.View()); strings.Contains(v, noMatchesMessage) || !strings.Contains(v, "alpha one") {
		t.Errorf("a matching filter must show its rows and no message, got:\n%s", v)
	}
}

// TestKeymapCancel_FilterFooterNamesEnter pins the footer fix from #1105:
// Esc ends the form (Quit matches first), so the filter hint must not say
// `esc set filter`.
func TestKeymapCancel_FilterFooterNamesEnter(t *testing.T) {
	km := keymap.Cancel()
	if got := km.Select.SetFilter.Help().Key; got != "enter" {
		t.Errorf("Select set-filter hint = %q, want enter", got)
	}
	if got := km.MultiSelect.SetFilter.Help().Key; got != "enter" {
		t.Errorf("MultiSelect set-filter hint = %q, want enter", got)
	}
}
