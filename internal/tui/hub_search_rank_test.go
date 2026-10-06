package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// searchHub is a hub whose subcommand names reproduce forgectl#1102: an exact
// "pr", subcommands whose fuzzy scores tie with it, and three siblings that
// differ only in their last word.
func searchHub() []HubEntry {
	return []HubEntry{
		{Name: "pip", Short: "pip.conf editor", Key: 1, Leaves: []HubLeaf{
			{Name: "pip", Short: "pip.conf editor", Self: true},
			{Name: "remove", Short: "Comment out a mirror"},
			{Name: "restore", Short: "Un-comment whatever was removed"},
		}},
		{Name: "pr", Short: "Clean-room review of a pull request", Key: 2, Leaves: []HubLeaf{
			{Name: "pr", Short: "Clean-room review of a pull request", Self: true},
			{Name: "repair", Short: "Inspect and settle review sessions"},
			{Name: "reviewed", Short: "Reviewed marks", Leaves: []HubLeaf{
				{Name: "mark", Short: "Mark a PR reviewed"},
				{Name: "sync", Short: "Prune reviewed marks"},
				{Name: "clear", Short: "Clear a PR's reviewed mark"},
			}},
		}},
		{Name: "proxy", Short: "Emit proxy-profile changes", Key: 3},
		{Name: "resume", Short: "Resume a session", Key: 5, Leaves: []HubLeaf{
			{Name: "resume", Short: "Resume a session", Self: true},
			{Name: "hooks", Short: "Session hooks", Leaves: []HubLeaf{
				{Name: "install", Short: "Install the capture hooks"},
				{Name: "uninstall", Short: "Remove the capture hooks"},
			}},
		}},
		{Name: "y", Short: "Clipboard helpers", Key: 4, Leaves: []HubLeaf{
			{Name: "y", Short: "Clipboard helpers", Self: true},
			{Name: "paste", Short: "Print the clipboard"},
		}},
	}
}

func searchModel(w, h int) model {
	return sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{
		Hub:     searchHub(),
		NoIcons: true,
		Theme:   theme.Default(),
	}), w, h)
}

// filterResults runs the commands one key returned and collects the filter
// results they answer with. bubbles filters in a returned command, one per
// keystroke; the cursor blink in the same batch never answers and is dropped.
func filterResults(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	select {
	case msg := <-got:
		switch t := msg.(type) {
		case tea.BatchMsg:
			var out []tea.Msg
			for _, c := range t {
				out = append(out, filterResults(c)...)
			}
			return out
		case filterRun:
			return []tea.Msg{t}
		}
	case <-time.After(100 * time.Millisecond):
	}
	return nil
}

// typeSearch opens the search and types q, then delivers every key's filter
// result in the order deliver picks: the order the runs finish in is the
// scheduler's, so a test has to try both.
func typeSearch(t *testing.T, m model, q string, deliver func([][]tea.Msg) []tea.Msg) model {
	t.Helper()
	out, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = out.(model)
	var perKey [][]tea.Msg
	for _, r := range q {
		out, cmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = out.(model)
		perKey = append(perKey, filterResults(cmd))
	}
	for _, msg := range deliver(perKey) {
		out, _ := m.Update(msg)
		m = out.(model)
	}
	return m
}

func visibleNames(m model) []string {
	var names []string
	for _, it := range m.l.VisibleItems() {
		names = append(names, it.FilterValue())
	}
	return names
}

// TestHubSearchOrderDoesNotDependOnWhichRunFinishesLast pins forgectl#1102's
// width-dependent ranking: typing "pr" starts a run for "p" and one for "pr",
// and when the "p" run's result landed last it replaced the "pr" one, so the
// same query showed different rows depending on timing (which moved with the
// terminal width). The list must show the current query's result whichever
// order the results arrive in.
//
// Mutation that turns it red: delete the run.gen != m.filterGen return in
// updateListMsg, so an older query's result is applied.
func TestHubSearchOrderDoesNotDependOnWhichRunFinishesLast(t *testing.T) {
	inOrder := func(perKey [][]tea.Msg) []tea.Msg {
		var out []tea.Msg
		for _, r := range perKey {
			out = append(out, r...)
		}
		return out
	}
	reversed := func(perKey [][]tea.Msg) []tea.Msg {
		out := inOrder(perKey)
		slices.Reverse(out)
		return out
	}
	want := visibleNames(typeSearch(t, searchModel(80, 24), "pr", inOrder))
	got := visibleNames(typeSearch(t, searchModel(80, 24), "pr", reversed))
	if !slices.Equal(got, want) {
		t.Errorf("results arriving newest-first changed the list:\n in order: %v\n reversed: %v", want, got)
	}
	if slices.Contains(got, "y paste") {
		t.Errorf("the stale result for \"p\" is on screen: %v", got)
	}
}

// TestHubSearchRanksTheExactNameFirst pins the other half of forgectl#1102:
// "pr" is the exact name, and it sat sixth behind rows whose fuzzy score
// ties with it.
//
// Mutation that turns it red: leave l.Filter at the list's default in
// newModel.
func TestHubSearchRanksTheExactNameFirst(t *testing.T) {
	for _, w := range []int{120, 80, 40} {
		got := visibleNames(typeSearch(t, searchModel(w, 24), "pr", func(p [][]tea.Msg) []tea.Msg {
			var out []tea.Msg
			for _, r := range p {
				out = append(out, r...)
			}
			return out
		}))
		if len(got) == 0 || got[0] != "pr" {
			t.Errorf("width %d: search for pr lists %v, want pr first", w, got)
		}
	}
}

func TestRankFilterIsDeterministicAndTiered(t *testing.T) {
	targets := []string{"pr repair", "pip remove", "pip restore", "pr reviewed mark", "pr", "proxy", "Pr"}
	rank := func() []string {
		var names []string
		for _, r := range rankFilter("pr", targets) {
			names = append(names, targets[r.Index])
		}
		return names
	}
	names := rank()
	for i := 0; i < 50; i++ {
		if got := rank(); !slices.Equal(got, names) {
			t.Fatalf("run %d ranked differently:\n%v\n%v", i, names, got)
		}
	}
	// Exact names (any case) first, then names that start with the query,
	// then the rest.
	isPr := func(n string) bool { return strings.EqualFold(n, "pr") }
	if !isPr(names[0]) || !isPr(names[1]) {
		t.Errorf("exact names are not first: %v", names)
	}
	last := names[len(names)-2:]
	if !strings.HasPrefix(last[0], "pip") || !strings.HasPrefix(last[1], "pip") {
		t.Errorf("non-prefix matches are not last: %v", names)
	}
}

// TestHubSearchNarrowNamesStayDistinct pins forgectl#1102's collision: at 40
// columns "pr reviewed mark", "pr reviewed sync", and "pr reviewed clear" all
// drew as "pr reviewed…". The rows must differ, and none may overflow.
//
// Mutation that turns it red: call fitWords in place of fitName in hubRow.
func TestHubSearchNarrowNamesStayDistinct(t *testing.T) {
	for _, c := range []struct {
		w     int
		query string
		rows  string
		want  int
	}{{40, "reviewed", "pr re", 3}, {48, "reviewed", "pr re", 3}, {40, "hooks", "res", 2}} {
		w := c.w
		m := typeSearch(t, searchModel(w, 24), c.query, func(p [][]tea.Msg) []tea.Msg {
			var out []tea.Msg
			for _, r := range p {
				out = append(out, r...)
			}
			return out
		})
		seen := map[string]bool{}
		for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if !strings.Contains(line, c.rows) || strings.HasPrefix(line, "$") {
				continue
			}
			if got := ansi.StringWidth(line); got > w {
				t.Errorf("width %d: row is %d cells wide: %q", w, got, line)
			}
			// the name cell is what precedes the two-space gap before the description
			name := strings.TrimSpace(line[strings.Index(line, c.rows[:2]):])
			if i := strings.Index(name, "  "); i >= 0 {
				name = name[:i]
			}
			if seen[name] {
				t.Errorf("width %d: two rows both read %q:\n%s", w, name, ansi.Strip(m.View().Content))
			}
			seen[name] = true
		}
		if len(seen) < c.want {
			t.Errorf("width %d: found %d distinct %q rows, want %d:\n%s", w, len(seen), c.rows, c.want, ansi.Strip(m.View().Content))
		}
	}
}

func TestFitName(t *testing.T) {
	cases := []struct {
		name  string
		width int
	}{
		{"pr reviewed mark", 13}, {"pr reviewed sync", 13}, {"pr reviewed clear", 13},
		{"pr", 13}, {"projects", 5}, {"pr reviewed mark", 16}, {"a b", 2},
		{"resume hooks install", 13}, {"resume hooks uninstall", 13}, {"resume hooks status", 13},
		{"a very long first word then x", 8},
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := fitName(c.name, c.width)
		if w := ansi.StringWidth(got); w > c.width {
			t.Errorf("fitName(%q, %d) = %q, %d cells wide", c.name, c.width, got, w)
		}
		if c.width == 13 && (strings.Contains(c.name, "reviewed") || strings.HasPrefix(c.name, "resume hooks")) {
			if prev, dup := seen[got]; dup {
				t.Errorf("fitName(%q, 13) = %q, same as %q", c.name, got, prev)
			}
			seen[got] = c.name
		}
	}
	if got := fitName("pr reviewed mark", 16); got != "pr reviewed mark" {
		t.Errorf("a name that fits was cut: %q", got)
	}
}

// TestHubSearchResultKeepsCaretAndCursor pins two things a late filter result
// must not disturb, found in review of the first fix for forgectl#1102: the
// caret while the operator edits mid-query, and the cursor row after the
// filter was accepted and the operator moved down it.
//
// Mutation that turns it red: handle a filterRun in updateListMsg by calling
// m.l.SetFilterText(m.l.FilterValue()) instead of passing its message to the
// list, which moves the caret to the end and the cursor to the first row.
func TestHubSearchResultKeepsCaretAndCursor(t *testing.T) {
	press := func(m model, k tea.KeyPressMsg) (model, tea.Cmd) {
		out, cmd := m.Update(k)
		return out.(model), cmd
	}
	m := searchModel(80, 24)
	m, _ = press(m, tea.KeyPressMsg{Code: '/', Text: "/"})
	var runs []tea.Msg
	for _, r := range "pro" {
		var cmd tea.Cmd
		m, cmd = press(m, tea.KeyPressMsg{Code: r, Text: string(r)})
		runs = filterResults(cmd)
	}
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := m.l.FilterInput.Position(); got != 2 {
		t.Fatalf("setup: caret at %d after one left, want 2", got)
	}
	for _, run := range runs {
		out, _ := m.Update(run)
		m = out.(model)
	}
	if got := m.l.FilterInput.Position(); got != 2 {
		t.Errorf("a filter result moved the caret to %d, want 2", got)
	}

	// Accept the filter, move down it, then let a straggler arrive.
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyDown})
	at := m.l.Index()
	if at == 0 {
		t.Fatalf("setup: down did not move the cursor (%v)", visibleNames(m))
	}
	for _, run := range runs {
		out, _ := m.Update(run)
		m = out.(model)
	}
	if got := m.l.Index(); got != at {
		t.Errorf("a late filter result moved the cursor from row %d to %d", at, got)
	}
}

// TestHubSearchPasteIsAQueryChangeToo pins a review finding on the fix for
// forgectl#1102: a paste changes the query without a key press, so its filter
// run must be stamped like a key's. Otherwise the run for the typed "p" still
// looks current when it lands after the paste's, and the list shows "p"'s rows
// under the query "pr".
//
// Mutation that turns it red: compare the query only for tea.KeyPressMsg in
// updateList, as the first version did.
func TestHubSearchPasteIsAQueryChangeToo(t *testing.T) {
	run := func(reverse bool) []string {
		m := searchModel(80, 24)
		out, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
		m = out.(model)
		out, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
		m = out.(model)
		typed := filterResults(cmd)
		out, cmd = m.Update(tea.PasteMsg{Content: "r"})
		m = out.(model)
		pasted := filterResults(cmd)
		msgs := append(append([]tea.Msg(nil), typed...), pasted...)
		if reverse {
			slices.Reverse(msgs)
		}
		for _, msg := range msgs {
			out, _ := m.Update(msg)
			m = out.(model)
		}
		return visibleNames(m)
	}
	want, got := run(false), run(true)
	if !slices.Equal(got, want) {
		t.Errorf("a paste arriving before the typed key's result changed the list:\n in order: %v\n reversed: %v", want, got)
	}
	if len(want) == 0 || want[0] != "pr" {
		t.Errorf("setup: the query pr lists %v, want pr first", want)
	}
}
