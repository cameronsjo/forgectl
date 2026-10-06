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
		{Name: "areas · 8 commands", Heading: true},
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
		{"hub_80x20_short", 80, 20, nil},
		{"hub_100x18_short", 100, 18, nil},
		{"hub_80x16_short", 80, 16, nil},
		{"hub_80x12_paged", 80, 12, nil},
		{"hub_80x24_docs_subcommands", 80, 24, []tea.KeyPressMsg{key("1")}},
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
	out, cmd := choose(m, "1")
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
// jumper sizes the list for the jumper's own chrome, not the hub's detail
// block.
func TestHubTmuxMenuGetsTheFullHeight(t *testing.T) {
	m := frameModel(80, 24)
	out, _ := m.Update(key("4")) // tmux
	m = out.(model)
	if want := 24 - 4; m.mode != menuMode || m.l.Height() != want {
		t.Errorf("tmux menu: mode=%v list height %d, want menuMode with %d", m.mode, m.l.Height(), want)
	}
}

// TestHubShortTerminalKeepsTheAreas pins the short-height rule: when every
// top-screen row does not fit, the recent rows and their divider go, and the
// pinned rows and every area stay on screen with their keys.
func TestHubShortTerminalKeepsTheAreas(t *testing.T) {
	for _, h := range []int{16, 18, 20} {
		m := frameModel(80, h)
		got := frameText(m)
		if strings.Contains(got, "recent") || strings.Contains(got, "tasks ready") {
			t.Errorf("at 80x%d the recent rows still take room:\n%s", h, got)
		}
		for _, want := range []string{"5 sessions", "6 agents ›", "9 setup ›"} {
			if !strings.Contains(got, want) {
				t.Errorf("at 80x%d the top screen lacks %q:\n%s", h, want, got)
			}
		}
	}
	if got := frameText(frameModel(80, 24)); !strings.Contains(got, "tasks ready") {
		t.Errorf("at 80x24 the recent rows should fit:\n%s", got)
	}
}

// TestHubDeeperListDigitsOnlyMove pins that below the top screen a digit
// moves the cursor and runs nothing; enter runs the row.
func TestHubDeeperListDigitsOnlyMove(t *testing.T) {
	m := frameModel(80, 24)
	m, _ = press(m, '9') // setup area
	if m.mode != areaMode {
		t.Fatalf("9 did not open the setup area: mode=%v", m.mode)
	}
	out, cmd := m.Update(key("1"))
	got := out.(model)
	if cmd != nil || got.action.Kind != ActionNone {
		t.Fatalf("a digit in an area ran %+v", got.action)
	}
	if it, ok := got.l.SelectedItem().(hubItem); !ok || it.entry.Name != "doctor" {
		t.Fatalf("cursor after 1 = %+v, want doctor", got.l.SelectedItem())
	}
	out, cmd = got.Update(keyCode(tea.KeyEnter))
	if a := out.(model).action; cmd == nil || a.Kind != ActionRunVerb || strings.Join(a.Argv, " ") != "doctor" {
		t.Errorf("enter on doctor = %+v, want RunVerb [doctor]", a)
	}
}

// TestHubSearchSaysWhenNothingMatches pins the empty search: the line under
// the list names what was searched and how to clear it.
func TestHubSearchSaysWhenNothingMatches(t *testing.T) {
	m := frameModel(80, 24)
	m = typeInto(m, "/")
	if m.l.FilterInput.Prompt != hubSearchPrompt {
		t.Errorf("hub search prompt = %q, want %q", m.l.FilterInput.Prompt, hubSearchPrompt)
	}
	m.l.SetFilterText("zzzx")
	got := ansi.Strip(m.detailView())
	if !strings.Contains(got, `no command or subcommand is named like "zzzx"`) {
		t.Errorf("empty search detail = %q", got)
	}
	if footer := ansi.Strip(m.footerView()); strings.Contains(footer, "enter") || strings.Contains(footer, "·  ·") {
		t.Errorf("empty search footer = %q, want no enter hint and no empty slot", footer)
	}
}

// TestHubLeafRowsShowTheirDescription pins that a subcommand needing an
// argument shows its description like any row; its usage is on the "$" line.
func TestHubLeafRowsShowTheirDescription(t *testing.T) {
	it := leafItem{leaf: HubLeaf{Name: "attach", Short: "Jump to a review window", Use: "attach <breadcrumb>", NeedsArgs: true}}
	if got := ansi.Strip(it.render(0, false, false, 80, 10, asciiGlyphs, theme.Default().Styles())); !strings.Contains(got, "Jump to a review window") || strings.Contains(got, "<breadcrumb>") {
		t.Errorf("leaf row = %q, want its description and not its usage", got)
	}
}

// TestHubEnterHintNamesWhatEnterDoes pins the footer's enter hint against
// what enter does to the selected row: "enter run" on a row that runs a
// command, "enter open" on one that opens a list or the argument picker.
func TestHubEnterHintNamesWhatEnterDoes(t *testing.T) {
	m := frameModel(80, 24)
	if got := m.enterHint(); got != "enter open" { // docs: subcommands
		t.Errorf("on docs: %q, want enter open", got)
	}
	m, _ = press(m, tea.KeyDown) // pr: argument picker
	if got := m.enterHint(); got != "enter open" {
		t.Errorf("on pr: %q, want enter open", got)
	}
	m.selectRow("desk") // a recent row runs
	if got := m.enterHint(); got != "enter run" {
		t.Errorf("on the recent desk row: %q, want enter run", got)
	}
	m, _ = press(m, '9') // setup area: doctor runs
	if got := m.enterHint(); got != "enter run" {
		t.Errorf("on doctor: %q, want enter run", got)
	}
	if !strings.Contains(ansi.Strip(m.footerView()), "enter run") {
		t.Errorf("footer on doctor = %q, want enter run", ansi.Strip(m.footerView()))
	}
}

// TestHubShortTerminalPagesByNumber pins the paged hub below 16 rows: the
// page is named in text, not by a colored dot.
func TestHubShortTerminalPagesByNumber(t *testing.T) {
	got := frameText(frameModel(80, 12))
	if !strings.Contains(got, "1/") || strings.Contains(got, "•") {
		t.Errorf("paged hub at 80x12 does not name its page:\n%s", got)
	}
}

// TestHubCursorStaysOnTheAreaRow pins row identity on short terminals: esc
// from an area, and a resize with the cursor on an area row, keep the cursor
// on that area (the drawn label carries a "›" the row's name does not).
func TestHubCursorStaysOnTheAreaRow(t *testing.T) {
	onRepos := func(m model) bool {
		it, ok := m.l.SelectedItem().(hubItem)
		return ok && it.entry.Name == "repos"
	}
	m := frameModel(80, 18)
	m, _ = press(m, '7')
	m, _ = press(m, tea.KeyEscape)
	if m.mode != hubMode || !onRepos(m) {
		t.Errorf("esc from repos at 80x18: cursor %+v, want repos", m.l.SelectedItem())
	}

	m = frameModel(80, 30)
	m.selectRow("repos")
	out, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 18})
	if m = out.(model); !onRepos(m) {
		t.Errorf("resize to 80x18 on repos: cursor %+v, want repos", m.l.SelectedItem())
	}
}

// TestHubFilterPromptBelongsToItsScreen pins that the list's prompt is set
// for each screen's own "/": a hub search leaves no "Search all:" behind on
// the tmux screens.
func TestHubFilterPromptBelongsToItsScreen(t *testing.T) {
	m := frameModel(80, 24)
	m = typeInto(m, "/")
	m, _ = press(m, tea.KeyEscape)
	m, _ = press(m, '4') // tmux
	if m.mode != menuMode {
		t.Fatalf("4 did not open the tmux screen: mode=%v", m.mode)
	}
	m = typeInto(m, "/")
	if got := m.l.FilterInput.Prompt; got != filterPrompt {
		t.Errorf("tmux screen prompt after a hub search = %q, want %q", got, filterPrompt)
	}
}

// TestTopEntries pins the order rows give way on a short terminal: recent
// rows and their divider first, then the other dividers, never a keyed row.
func TestTopEntries(t *testing.T) {
	names := func(es []HubEntry) string {
		var out []string
		for _, e := range es {
			out = append(out, e.Name)
		}
		return strings.Join(out, ",")
	}
	all := frameHub()
	cases := []struct {
		rows int
		want string
	}{
		{0, names(all)},
		{len(all), names(all)},
		{10, "docs,pr,projects,tmux,sessions,areas · 8 commands,agents,repos,shell,setup"},
		{9, "docs,pr,projects,tmux,sessions,agents,repos,shell,setup"},
		{4, "docs,pr,projects,tmux,sessions,agents,repos,shell,setup"},
	}
	for _, c := range cases {
		if got := names(topEntries(all, c.rows)); got != c.want {
			t.Errorf("topEntries(rows=%d) = %s, want %s", c.rows, got, c.want)
		}
	}
}

// TestHubFilterFramesFit pins the frame with a filter open, where the list
// adds its filter bar and a longer prompt: no line wider than the terminal
// and no more lines than it has rows, on the top screen and in an area.
func TestHubFilterFramesFit(t *testing.T) {
	for _, size := range [][2]int{{20, 8}, {40, 12}, {80, 12}, {80, 16}, {80, 24}, {104, 20}} {
		for _, area := range []bool{false, true} {
			m := frameModel(size[0], size[1])
			if area {
				m, _ = press(m, '9')
			}
			m = typeInto(m, "/")
			name := fmt.Sprintf("%dx%d area=%v", size[0], size[1], area)
			t.Run(name, func(t *testing.T) { assertFrameFits(t, frameText(m), size[0], size[1]) })
			m.l.SetFilterText("zzzx")
			t.Run(name+" applied", func(t *testing.T) { assertFrameFits(t, frameText(m), size[0], size[1]) })
		}
	}
	// A typed query, not just an open filter: the prompt must leave the
	// input room at the narrow end, on the top screen and in a subcommand
	// list whose title is long.
	for _, w := range []int{20, 24, 30, 40} {
		for _, deep := range []bool{false, true} {
			m := frameModel(w, 12)
			if deep {
				m, _ = press(m, '6') // agents
				m, _ = press(m, tea.KeyEnter)
			}
			m = typeInto(m, "/a-much-longer-query-than-fits")
			name := fmt.Sprintf("%dx12 deep=%v typed", w, deep)
			t.Run(name, func(t *testing.T) { assertFrameFits(t, frameText(m), w, 12) })
		}
	}
}

// TestHubPickerCancelKeepsTheRecentRow pins that cancelling the argument
// picker opened from a recent row leaves the cursor on that row, even where
// the top screen is short enough to drop recent rows.
func TestHubPickerCancelKeepsTheRecentRow(t *testing.T) {
	hub := frameHub()
	for i := range hub {
		if hub[i].Name == "tasks ready" {
			hub[i] = HubEntry{Name: "pr reviewed", Short: "mark a PR reviewed", Use: "reviewed <ref>", Argv: []string{"pr", "reviewed"}, NeedsArgs: true}
		}
	}
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, NoIcons: true, Theme: theme.Default()}), 80, 24)
	m.selectRow("pr reviewed")
	m, _ = press(m, tea.KeyEnter)
	if m.picker == nil {
		t.Fatal("enter on pr reviewed did not open the picker")
	}
	m, _ = press(m, tea.KeyEscape)
	if got := m.selectedName(); m.picker != nil || got != "pr reviewed" {
		t.Errorf("after esc: picker=%v cursor=%q, want the pr reviewed row", m.picker != nil, got)
	}
}

// TestHubOptionalArgumentRowsRunBare pins the three things enter does to a
// row whose usage names placeholders: open the picker when it can take the
// argument, run the bare command when every placeholder is optional, or
// print the usage when a required one is left. The footer hint says which.
func TestHubOptionalArgumentRowsRunBare(t *testing.T) {
	hub := []HubEntry{{Name: "docs", Key: 1, Leaves: []HubLeaf{
		{Name: "check", Short: "check links", Use: "check [dir|file ...]", NeedsArgs: true},
		{Name: "open", Short: "open one", Use: "open [path]", NeedsArgs: true},
		{Name: "copy", Short: "copy two", Use: "copy <from> <to>", NeedsArgs: true},
	}}}
	start := func(row int) model {
		m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, NoIcons: true, Theme: theme.Default()}), 80, 24)
		m, _ = press(m, '1')
		out, _ := m.Update(key(fmt.Sprint(row)))
		return out.(model)
	}
	cases := []struct {
		row  int
		hint string
		kind ActionKind
	}{
		{1, "enter run", ActionRunVerb},
		{2, "enter open", ActionNone},
		{3, "enter print command", ActionShowInvocation},
	}
	for _, c := range cases {
		m := start(c.row)
		if got := m.enterHint(); got != c.hint {
			t.Errorf("row %d hint = %q, want %q", c.row, got, c.hint)
		}
		out, _ := m.Update(keyCode(tea.KeyEnter))
		got := out.(model)
		if got.action.Kind != c.kind {
			t.Errorf("row %d enter = %+v, want kind %v", c.row, got.action, c.kind)
		}
		if c.row == 1 && strings.Join(got.action.Argv, " ") != "docs check" {
			t.Errorf("row 1 argv = %v, want [docs check]", got.action.Argv)
		}
		if c.row == 2 && got.picker == nil {
			t.Error("row 2 did not open the picker")
		}
	}
	// The "$" line names exactly what enter runs: the bare command for a row
	// that runs without its optional placeholders.
	if got := ansi.Strip(start(1).selectedDollar()); got != "$ forgectl docs check" {
		t.Errorf("$ line on the bare-run row = %q, want $ forgectl docs check", got)
	}
}

// TestHubSearchFindsSubcommands pins that "/" covers subcommands under their
// full path, and that enter on one acts on that subcommand.
func TestHubSearchFindsSubcommands(t *testing.T) {
	m := frameModel(80, 24)
	m = typeInto(m, "/")
	m.l.SetFilterText("sessions sync")
	if got := m.selectedName(); got != "sessions sync" {
		t.Fatalf("search for sessions sync selected %q", got)
	}
	out, cmd := m.Update(keyCode(tea.KeyEnter))
	if a := out.(model).action; cmd == nil || a.Kind != ActionRunVerb || strings.Join(a.Argv, " ") != "sessions sync" {
		t.Errorf("enter on the found subcommand = %+v, want RunVerb [sessions sync]", a)
	}
}

// TestHubEscKeepsTheRow pins two esc paths that return to a list: clearing
// a filter keeps the cursor on the row it was on, and leaving the tmux
// screen lands on the tmux row.
func TestHubEscKeepsTheRow(t *testing.T) {
	m := frameModel(80, 24)
	m, _ = press(m, '7') // repos
	m = typeInto(m, "/")
	m.l.SetFilterText("clean")
	m, _ = press(m, tea.KeyEscape)
	if got := m.selectedName(); m.mode != areaMode || got != "clean" {
		t.Errorf("esc from a filter in repos: mode=%v cursor=%q, want clean", m.mode, got)
	}

	m = frameModel(80, 24)
	m.selectRow("agents")
	m = typeInto(m, "/")
	m, _ = press(m, tea.KeyEscape)
	if got := m.selectedName(); m.mode != hubMode || got != "agents" {
		t.Errorf("cancelled search from agents: mode=%v cursor=%q, want agents", m.mode, got)
	}

	m = frameModel(80, 24)
	m, _ = press(m, '4')
	m, _ = press(m, tea.KeyEscape)
	if got := m.selectedName(); m.mode != hubMode || got != "tmux" {
		t.Errorf("esc from the tmux screen: mode=%v cursor=%q, want tmux", m.mode, got)
	}
}
