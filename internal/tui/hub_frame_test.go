package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// frameHub is a hub shaped like the real one (forgectl#1074): five pinned
// rows keyed 1-5, three recent rows, and four areas keyed 6-9. The
// descriptions are real ones, long enough to be cut at 80 columns, so the
// goldens show where each row breaks. They are copied rather than read from
// the command tree so a reworded Short does not churn these frames.
func frameHub() []HubEntry {
	return []HubEntry{
		{Name: "docs", Short: "Local markdown reader — render + serve an indexed doc set over loopback HTTP", Key: 1, Leaves: []HubLeaf{{Name: "list", Short: "List the indexed docs", Use: "list"}}},
		{Name: "pr", Short: "Clean-room review of a pull request", Use: "pr <ref>", Key: 2},
		{Name: "projects", Short: "Find and open projects across local, GitHub, and Gitea (clones on demand)", Key: 3, Leaves: []HubLeaf{{Name: "open", Short: "Open a project", Use: "open"}}},
		{Name: "tmux", Short: "Wrangle tmux sessions, windows, and panes", Key: 4},
		{Name: "sessions", Short: "Sync local session ledgers into the operational concordance and query it", Key: 5, Leaves: []HubLeaf{{Name: "sync", Short: "Sync ledgers", Use: "sync"}}},
		{Name: "recent", Heading: true},
		{Name: "desk", Short: "Operator queue: scripts Claude stages for you to approve and run", Argv: []string{"desk"}},
		{Name: "tasks ready", Short: "List tasks that are ready to start", Argv: []string{"tasks", "ready"}},
		{Name: "resume", Short: "Pick a recent Claude Code session in any repo and resume it there", Argv: []string{"resume"}},
		{Name: "all commands (8)", Heading: true},
		{Name: "agents", Short: "launch · resume · desk · tasks · surface · workflow · recipe · preflight · quarantine", Key: 6, Members: []HubEntry{
			{Name: "launch", Short: "Per-project launcher for Claude Code, Codex CLI, or Pi"},
			{Name: "quarantine", Short: "Reversibly hide AI-instruction files from a workspace"},
		}},
		{Name: "repos", Short: "status · review · branch · clean · audit · docker · k8s", Key: 7, Members: []HubEntry{
			{Name: "branch", Short: "Prune stale/orphaned git branches (dry-run by default)"},
			{Name: "clean", Short: "Reclaim dep/build directories under a project root (dry-run by default)"},
		}},
		{Name: "shell", Short: "env · y · pip · proxy · net", Key: 8, Members: []HubEntry{
			{Name: "net", Short: "Check cached reachability of the configured probe endpoint"},
		}},
		{Name: "setup", Short: "doctor · config · init · update · upgrade · bench · theme · ghostty · herdr", Key: 9, Members: []HubEntry{
			{Name: "doctor", Short: "Check the whole workbench is wired: claude, tmux, ghostty, gh, config, the local bench, and forgectl itself"},
			{Name: "config", Short: "Show the active configuration and config file path"},
		}},
	}
}

func frameModel(w, h int) model {
	return sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{
		Hub:     frameHub(),
		Header:  HubHeader{Project: "forgectl", Branch: "main", HasTmux: true, TmuxSessions: 2, HasReviews: true},
		NoIcons: true,
		Theme:   theme.Default(),
	}), w, h)
}

// frameText is a view as plain text with trailing spaces trimmed per line,
// what a person reads on the screen.
func frameText(m model) string {
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestHubFrames pins the hub's layout at the widths forgectl#1074 names (80,
// 100, 160), a phone-narrow 40, and below the minimum size, plus an area and
// a cursor moved onto a long row. Regenerate with `go test ./internal/tui
// -run TestHubFrames -update` and read the diff.
//
// Beside the goldens, every frame is checked for what a golden alone would
// let through on regeneration: no line wider than the terminal and no more
// lines than it has rows. TestFitWords pins where a cut may fall.
func TestHubFrames(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		keys []tea.KeyPressMsg
	}{
		{"hub_80x24", 80, 24, nil},
		{"hub_100x30", 100, 30, nil},
		{"hub_160x40", 160, 40, nil},
		{"hub_40x20", 40, 20, nil},
		{"hub_too_small", 18, 6, nil},
		{"hub_80x24_setup_area", 80, 24, []tea.KeyPressMsg{key("9")}},
		{"hub_80x24_cursor_on_sessions", 80, 24, []tea.KeyPressMsg{keyCode(tea.KeyDown), keyCode(tea.KeyDown), keyCode(tea.KeyDown), keyCode(tea.KeyDown)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := frameModel(c.w, c.h)
			for _, k := range c.keys {
				out, _ := m.Update(k)
				m = out.(model)
			}
			got := frameText(m)
			assertFrameFits(t, got, c.w, c.h)
			assertGolden(t, c.name, got)
		})
	}
}

func assertFrameFits(t *testing.T, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	if len(lines) > h {
		t.Errorf("frame has %d lines, terminal has %d rows", len(lines), h)
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got > w {
			t.Errorf("line %d is %d cells wide, terminal is %d: %q", i+1, got, w, l)
		}
	}
}

func TestFitWords(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"Find and open projects across local, GitHub, and Gitea", 40, "Find and open projects across local…"},
		{"Local markdown reader — render + serve", 24, "Local markdown reader…"},
		{"abcdefgh ij klmno", 12, "abcdefgh ij…"},
		{"supercalifragilistic word", 10, "supercali…"},
		{"a b", 1, "…"},
		{"anything", 0, ""},
		{"日本語のテキストです", 9, "日本語の…"},
	}
	for _, c := range cases {
		got := fitWords(c.in, c.width)
		if got != c.want {
			t.Errorf("fitWords(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
		if w := ansi.StringWidth(got); w > c.width {
			t.Errorf("fitWords(%q, %d) is %d cells wide", c.in, c.width, w)
		}
	}
}

func TestFitHints(t *testing.T) {
	hints := []string{"↑↓ move", "1-9 jump", "enter open", "q quit"}
	prio := []int{2, 3, 0, 1}
	cases := []struct {
		width int
		want  string
	}{
		{80, "↑↓ move · 1-9 jump · enter open · q quit"},
		{30, "↑↓ move · enter open · q quit"},
		{20, "enter open · q quit"},
		{5, "enter open"},
	}
	for _, c := range cases {
		if got := fitHints(c.width, hints, prio); got != c.want {
			t.Errorf("fitHints(%d) = %q, want %q", c.width, got, c.want)
		}
	}
}

// TestHubFooterFitsEveryScreen checks every screen's footer at the widths
// the frames use: none may overflow, so none ends mid-word.
func TestHubFooterFitsEveryScreen(t *testing.T) {
	for _, w := range []int{40, 80, 100, 160} {
		for _, md := range []mode{hubMode, areaMode, leavesMode, menuMode, sessionsMode, pickMode, windowsMode, treeMode, cheatMode, formMode} {
			m := frameModel(w, 24)
			m.mode = md
			footer := ansi.Strip(m.footerView())
			if got := ansi.StringWidth(footer); got > w {
				t.Errorf("mode %d at %d columns: footer is %d cells: %q", md, w, got, footer)
			}
		}
	}
}

// TestHubHeaderFits checks the header line is cut to the width rather than
// wrapped, however long the branch name.
func TestHubHeaderFits(t *testing.T) {
	for _, w := range []int{30, 40, 80} {
		m := frameModel(w, 24)
		m.header.Branch = strings.Repeat("feature-", 10)
		header := ansi.Strip(m.headerView())
		if strings.Contains(header, "\n") || ansi.StringWidth(header) > w {
			t.Errorf("header at %d columns = %q (%d cells)", w, header, ansi.StringWidth(header))
		}
	}
}

// TestHubDetailShowsCutDescription pins the detail line: when the selected
// row cut its description, the full text appears under the list, and when it
// fit, nothing repeats it.
func TestHubDetailShowsCutDescription(t *testing.T) {
	m := frameModel(80, 24) // cursor on docs, whose description is cut at 80
	detail := ansi.Strip(m.detailView())
	if !strings.Contains(detail, "over loopback HTTP") {
		t.Errorf("detail for a cut row lacks the full description:\n%s", detail)
	}
	m, _ = press(m, tea.KeyDown) // pr: fits
	detail = ansi.Strip(m.detailView())
	if strings.Contains(detail, "Clean-room") {
		t.Errorf("detail repeats a description the row showed whole:\n%s", detail)
	}
	if !strings.Contains(detail, "$ forgectl pr <ref>") {
		t.Errorf("detail lacks the command line:\n%s", detail)
	}
}

func TestHubTooSmallSaysSo(t *testing.T) {
	for _, size := range [][2]int{{hubMinWidth - 1, 24}, {80, hubMinHeight - 1}} {
		m := frameModel(size[0], size[1])
		got := frameText(m)
		if !strings.Contains(strings.Join(strings.Fields(got), " "), fmt.Sprintf("this terminal is %d×%d", size[0], size[1])) {
			t.Errorf("at %dx%d the view does not say the terminal is too small:\n%s", size[0], size[1], got)
		}
	}
}

// TestHubTooSmallTakesNoKeys pins that a screen the user cannot see does not
// act: below the minimum size every key but q and esc is dropped, and those
// quit, whatever screen is underneath.
func TestHubTooSmallTakesNoKeys(t *testing.T) {
	m := frameModel(hubMinWidth-1, 24)
	for _, k := range []tea.KeyPressMsg{key("1"), key("9"), keyCode(tea.KeyEnter), key("/")} {
		out, cmd := m.Update(k)
		got := out.(model)
		if cmd != nil || got.action.Kind != ActionNone || got.mode != hubMode || got.hubFlat {
			t.Errorf("key %q acted on a hidden screen: mode=%v action=%+v flat=%v", k.String(), got.mode, got.action, got.hubFlat)
		}
	}
	m.mode = leavesMode
	if _, cmd := m.Update(key("q")); cmd == nil {
		t.Error("q below the minimum size does not quit")
	}
}

// TestHubSearchNumbersByPosition pins the search-all list's numbering: its
// rows carry no fixed key, so 1-9 number the visible rows, and a digit runs
// the row drawn with it.
func TestHubSearchNumbersByPosition(t *testing.T) {
	m := frameModel(80, 24)
	m = typeInto(m, "/")
	m.l.SetFilterText("quarantine")
	if got := ansi.Strip(m.l.View()); !strings.Contains(got, "1 quarantine") {
		t.Fatalf("the one search result is not numbered 1:\n%s", got)
	}
	out, cmd := m.Update(key("1"))
	if a := out.(model).action; cmd == nil || a.Kind != ActionRunVerb || strings.Join(a.Argv, " ") != "quarantine" {
		t.Errorf("1 on the search result = %+v, want RunVerb [quarantine]", a)
	}
}

// TestHubRowsDrawBeforeASize pins that a frame drawn before the first
// WindowSizeMsg still names its rows: width 0 means no size yet, not no room.
func TestHubRowsDrawBeforeASize(t *testing.T) {
	m := newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: frameHub(), NoIcons: true, Theme: theme.Default()})
	m.l.SetSize(0, 30)
	got := ansi.Strip(m.l.View())
	for _, want := range []string{"docs", "recent", "agents ›"} {
		if !strings.Contains(got, want) {
			t.Errorf("unsized frame lacks %q:\n%s", want, got)
		}
	}
}

// TestHubTmuxMenuGetsTheFullHeight pins that leaving the hub for the tmux
// jumper gives back the rows the hub's detail block took.
func TestHubTmuxMenuGetsTheFullHeight(t *testing.T) {
	m := frameModel(80, 24)
	hubHeight := m.l.Height()
	out, _ := m.Update(key("4")) // tmux
	m = out.(model)
	if m.mode != menuMode || m.l.Height() != hubHeight+hubDetailLines {
		t.Errorf("tmux menu: mode=%v list height %d, want menuMode with %d", m.mode, m.l.Height(), hubHeight+hubDetailLines)
	}
}
