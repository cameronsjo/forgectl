package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
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
func filterResults(cmd tea.Cmd) []list.FilterMatchesMsg {
	if cmd == nil {
		return nil
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	select {
	case msg := <-got:
		switch t := msg.(type) {
		case tea.BatchMsg:
			var out []list.FilterMatchesMsg
			for _, c := range t {
				out = append(out, filterResults(c)...)
			}
			return out
		case list.FilterMatchesMsg:
			return []list.FilterMatchesMsg{t}
		}
	case <-time.After(100 * time.Millisecond):
	}
	return nil
}

// typeSearch opens the search and types q, then delivers every key's filter
// result in the order deliver picks: the order the runs finish in is the
// scheduler's, so a test has to try both.
func typeSearch(t *testing.T, m model, q string, deliver func([][]list.FilterMatchesMsg) []list.FilterMatchesMsg) model {
	t.Helper()
	out, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = out.(model)
	var perKey [][]list.FilterMatchesMsg
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
// Mutation that turns it red: in updateList, hand a FilterMatchesMsg to
// m.l.Update instead of calling m.refilter().
func TestHubSearchOrderDoesNotDependOnWhichRunFinishesLast(t *testing.T) {
	inOrder := func(perKey [][]list.FilterMatchesMsg) []list.FilterMatchesMsg {
		var out []list.FilterMatchesMsg
		for _, r := range perKey {
			out = append(out, r...)
		}
		return out
	}
	reversed := func(perKey [][]list.FilterMatchesMsg) []list.FilterMatchesMsg {
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
		got := visibleNames(typeSearch(t, searchModel(w, 24), "pr", func(p [][]list.FilterMatchesMsg) []list.FilterMatchesMsg {
			var out []list.FilterMatchesMsg
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
	for _, w := range []int{40, 48} {
		m := typeSearch(t, searchModel(w, 24), "reviewed", func(p [][]list.FilterMatchesMsg) []list.FilterMatchesMsg {
			var out []list.FilterMatchesMsg
			for _, r := range p {
				out = append(out, r...)
			}
			return out
		})
		seen := map[string]bool{}
		for _, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
			if !strings.Contains(line, "pr re") || strings.HasPrefix(line, "$") {
				continue
			}
			if got := ansi.StringWidth(line); got > w {
				t.Errorf("width %d: row is %d cells wide: %q", w, got, line)
			}
			// the name cell is what precedes the two-space gap before the description
			name := strings.TrimSpace(line[strings.Index(line, "pr"):])
			if i := strings.Index(name, "  "); i >= 0 {
				name = name[:i]
			}
			if seen[name] {
				t.Errorf("width %d: two rows both read %q:\n%s", w, name, ansi.Strip(m.View().Content))
			}
			seen[name] = true
		}
		if len(seen) < 3 {
			t.Errorf("width %d: found %d distinct pr reviewed rows, want 3:\n%s", w, len(seen), ansi.Strip(m.View().Content))
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
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := fitName(c.name, c.width)
		if w := ansi.StringWidth(got); w > c.width {
			t.Errorf("fitName(%q, %d) = %q, %d cells wide", c.name, c.width, got, w)
		}
		if c.width == 13 && strings.Contains(c.name, "reviewed") {
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
