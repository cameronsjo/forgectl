package tui

import (
	"context"
	"image/color"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

const sep = "\x1f"

// oneSessionRow is a single list-sessions row in sessionFormat's field order:
// server pid, server start, native session id, name, windows, attached,
// created, path.
const oneSessionRow = "123" + sep + "456" + sep + "$1" + sep + "alpha" + sep +
	"1" + sep + "0" + sep + "1700000000" + sep + "/tmp"

// key builds a single-rune KEY PRESS. tea.KeyMsg is an interface in v2 that a
// release also satisfies, so tests construct the press explicitly — matching
// what the model switches on, and keeping a release from being mistaken for a
// second press.
//
// Single-rune only, enforced rather than assumed. A named or modified key
// ("esc", "ctrl+c") does not round-trip through this shape: Code would take the
// first letter while Text carried the whole name, producing a message that
// matches the model's switch for the wrong reason — or not at all — and a test
// that passes or fails on an accident. Build those with an explicit
// tea.KeyPressMsg{Code: tea.KeyEscape} instead, as TestEscFromSubscreenReturnsToMenu does.
func key(s string) tea.KeyPressMsg {
	r := []rune(s)
	if len(r) != 1 {
		panic("tui test: key() takes exactly one rune, got " + strconv.Quote(s) + "; use tea.KeyPressMsg directly for named or modified keys")
	}
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func sized(m model, w, h int) model {
	out, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return out.(model)
}

func TestMenuViewRenders(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	view := m.View().Content
	for _, want := range []string{"forgectl", "Pick", "Sessions", "Windows", "Tree", "Last", "Cheatsheet"} {
		if !strings.Contains(view, want) {
			t.Errorf("menu view missing %q\n%s", want, view)
		}
	}
}

func TestNumberKeyNavigatesAndAttaches(t *testing.T) {
	// "2" on the menu → Sessions screen (loads via the fake client); "1" there
	// → attach the first session and quit with the right Action.
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return oneSessionRow, nil
	}}
	m := sized(newModel(context.Background(), tmux.New(fake), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)

	out, _ := m.Update(key("2"))
	m = out.(model)
	if m.mode != sessionsMode {
		t.Fatalf("expected sessionsMode after '2', got %v", m.mode)
	}

	out, _ = m.Update(key("1"))
	m = out.(model)
	if m.action.Kind != ActionAttachSession {
		t.Fatalf("expected ActionAttachSession, got %+v", m.action)
	}
	// The Action carries a full identity, not a name: it has to survive Bubble
	// Tea's teardown before anything acts on it.
	if m.action.Session.ID != "$1" || m.action.Session.Name != "alpha" {
		t.Errorf("expected identity $1/alpha, got %+v", m.action.Session)
	}
	if m.action.Session.Generation.PID != "123" || m.action.Session.Generation.StartTime != "456" {
		t.Errorf("action identity is not generation-qualified: %+v", m.action.Session.Generation)
	}
}

func TestCheatFromMenu(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	out, _ := m.Update(key("6")) // Cheatsheet
	m = out.(model)
	if m.mode != cheatMode {
		t.Fatalf("expected cheatMode after '6', got %v", m.mode)
	}
	if !strings.Contains(m.View().Content, "pane") {
		t.Errorf("cheat view should explain 'pane'")
	}
}

func TestCheatsheetContent(t *testing.T) {
	cs := Cheatsheet(true, theme.Default().Styles())
	for _, want := range []string{"session", "window", "pane", "prefix |", "Ctrl+Space"} {
		if !strings.Contains(cs, want) {
			t.Errorf("cheatsheet missing %q", want)
		}
	}
}

func TestKillOthersEntersConfirm(t *testing.T) {
	// "2" → Sessions (one session via fake), then "K" → kill-others confirm form
	// with the right pending op + target. (Driving the huh form to completion is
	// out of scope; this locks the wiring.)
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return oneSessionRow, nil
	}}
	m := sized(newModel(context.Background(), tmux.New(fake), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)

	out, _ := m.Update(key("2"))
	m = out.(model)
	out, _ = m.Update(key("K"))
	m = out.(model)

	if m.mode != formMode {
		t.Fatalf("expected formMode after 'K', got %v", m.mode)
	}
	if m.pendingOp != opKillOthers {
		t.Errorf("expected opKillOthers, got %v", m.pendingOp)
	}
	// The confirmation holds the identity, not the name — the prompt the
	// operator reads and the object the kill lands on must be the same thing.
	if m.pendingSession.ID != "$1" || m.pendingSession.Name != "alpha" {
		t.Errorf("expected pending identity $1/alpha, got %+v", m.pendingSession)
	}
	if m.pendingSession.Generation.PID == "" {
		t.Error("pending identity is not generation-qualified; a restart between confirm and act would go unnoticed")
	}
	if !strings.Contains(m.View().Content, "alpha") {
		t.Error("the confirmation prompt should still render the session NAME")
	}
}

func TestLastFromMenu(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	out, _ := m.Update(key("5")) // Last
	m = out.(model)
	if m.action.Kind != ActionLast {
		t.Errorf("expected ActionLast, got %+v", m.action)
	}
}

func TestEscFromSubscreenReturnsToMenu(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	out, _ := m.Update(key("3")) // Windows
	m = out.(model)
	if m.mode != windowsMode {
		t.Fatalf("expected windowsMode, got %v", m.mode)
	}
	out, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if m.mode != menuMode {
		t.Errorf("esc should return to menu, got %v", m.mode)
	}
}

// TestBackgroundColorMsgRepaintsStyles pins the Init probe's one consumer:
// receiving a BackgroundColorMsg must actually flip the rendered palette, not
// just record the bit. theme.Artificer's accent hex differs between dark
// (#dbbb6f) and light (#7a5a10) modes, so the header render below carries the
// difference directly — this would stay green even if WithDark's return value
// were silently dropped instead of reassigned onto m.theme/m.styles.
func TestBackgroundColorMsgRepaintsStyles(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	before := m.View().Content

	out, _ := m.Update(tea.BackgroundColorMsg{Color: color.White})
	m = out.(model)

	if m.theme.IsDark() {
		t.Fatal("expected WithDark(false) after a light BackgroundColorMsg")
	}
	after := m.View().Content
	if before == after {
		t.Error("BackgroundColorMsg did not change the rendered view; styles were not rebuilt")
	}
}

// hubTestModel starts a model in hubMode with two entries: a leafless
// module ("doctor") and one with a NeedsArgs leaf ("pr").
func hubTestModel() model {
	hub := []HubEntry{
		{Name: "tmux", Short: "sessions, windows, tree", Core: true, Key: 1},
		{Name: "doctor", Short: "health check", Core: false, Key: 2},
		{Name: "pr", Short: "review a PR", Core: true, Key: 3, Leaves: []HubLeaf{
			{Name: "pr", Short: "review a PR", Use: "pr <ref>", NeedsArgs: true, Self: true},
			{Name: "list", Short: "list sessions", Use: "list"},
		}},
	}
	return sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, Theme: theme.Default()}), 80, 24)
}

func TestHub_StartsInHubMode(t *testing.T) {
	m := hubTestModel()
	if m.mode != hubMode {
		t.Fatalf("expected hubMode by default, got %v", m.mode)
	}
}

func TestHub_EscQuits(t *testing.T) {
	m := hubTestModel()
	out, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if m.mode != hubMode {
		t.Errorf("esc from hubMode should stay in hubMode (quit is the cmd, not a mode change), got %v", m.mode)
	}
	if cmd == nil {
		t.Error("esc from hubMode should return tea.Quit, got nil cmd")
	}
}

// TestTmuxRowEntersMenuModeUnchanged pins the special case: selecting the
// hub's tmux row opens today's tmux jumper (menuMode) rather than a leaves
// list.
func TestTmuxRowEntersMenuModeUnchanged(t *testing.T) {
	m := hubTestModel() // tmux is index 0
	out, _ := m.Update(key("1"))
	m = out.(model)
	if m.mode != menuMode {
		t.Fatalf("selecting the tmux hub row should enter menuMode, got %v", m.mode)
	}
}

// TestMenuEscReturnsToHub pins the changed esc semantics: menuMode's esc now
// goes to the hub rather than quitting.
func TestMenuEscReturnsToHub(t *testing.T) {
	m := hubTestModel()
	out, _ := m.Update(key("1")) // tmux row -> menuMode
	m = out.(model)
	if m.mode != menuMode {
		t.Fatalf("expected menuMode, got %v", m.mode)
	}
	out, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if m.mode != hubMode {
		t.Errorf("esc from menuMode should return to hubMode, got %v", m.mode)
	}
	if cmd != nil {
		t.Error("esc from menuMode should not quit")
	}
}

// TestHub_LeaflessEntryRunsDirectly pins doctor's leafless-row behavior:
// selecting it quits with an ActionRunVerb for its own name.
func TestHub_LeaflessEntryRunsDirectly(t *testing.T) {
	m := hubTestModel() // doctor is index 1
	out, cmd := m.Update(key("2"))
	m = out.(model)
	if m.action.Kind != ActionRunVerb {
		t.Fatalf("expected ActionRunVerb, got %+v", m.action)
	}
	if len(m.action.Argv) != 1 || m.action.Argv[0] != "doctor" {
		t.Errorf("Argv = %v, want [doctor]", m.action.Argv)
	}
	if cmd == nil {
		t.Error("selecting a leafless entry should quit")
	}
}

// TestHub_NeedsArgsLeafOpensPickerWithoutRunning pins the pr <ref> contract
// (forgectl#730 item 4): selecting a leaf that needs one argument opens the
// picker in place and runs nothing until a value is chosen.
func TestHub_NeedsArgsLeafOpensPickerWithoutRunning(t *testing.T) {
	m := hubTestModel()
	out, _ := m.Update(key("3")) // pr row -> leavesMode
	m = out.(model)
	if m.mode != leavesMode {
		t.Fatalf("expected leavesMode, got %v", m.mode)
	}
	out, cmd := choose(m, "1") // pr's own NeedsArgs leaf
	m = out.(model)
	if m.picker == nil {
		t.Fatal("expected the argument picker to open")
	}
	if m.action.Kind != ActionNone || cmd != nil {
		t.Errorf("opening the picker ran something: %+v", m.action)
	}
	if got := strings.Join(m.picker.prefix, " "); got != "pr" || m.picker.placeholder != "<ref>" {
		t.Errorf("picker prefix/placeholder = %q/%q, want pr/<ref>", got, m.picker.placeholder)
	}
}

// TestHub_LeafRunsWithFullArgv pins the complete-argv case: selecting
// "list" under pr's leaves quits with ActionRunVerb{Argv: [pr list]}.
func TestHub_LeafRunsWithFullArgv(t *testing.T) {
	m := hubTestModel()
	out, _ := m.Update(key("3")) // pr row -> leavesMode
	m = out.(model)
	out, _ = choose(m, "2") // pr's "list" leaf
	m = out.(model)
	if m.action.Kind != ActionRunVerb {
		t.Fatalf("expected ActionRunVerb, got %+v", m.action)
	}
	if strings.Join(m.action.Argv, " ") != "pr list" {
		t.Errorf("Argv = %v, want [pr list]", m.action.Argv)
	}
}

// TestLeavesEscReturnsToHub pins leavesMode's esc target: back to the hub,
// not menuMode.
func TestLeavesEscReturnsToHub(t *testing.T) {
	m := hubTestModel()
	out, _ := m.Update(key("3")) // pr row -> leavesMode
	m = out.(model)
	out, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if m.mode != hubMode {
		t.Errorf("esc from leavesMode should return to hubMode, got %v", m.mode)
	}
	if cmd != nil {
		t.Error("esc from leavesMode should not quit")
	}
}

func TestSessionItemNarrowDropsMetadata(t *testing.T) {
	// Narrow rows (iPhone/Termius) must drop the windows/path metadata column;
	// wide rows must include it.
	it := sessionItem{s: tmux.Session{Name: "alpha", Windows: 3, Path: "/Users/cam/x"}}
	wide := it.render(0, false, false, 80, 12, asciiGlyphs, theme.Default().Styles())
	narrow := it.render(0, false, true, 80, 12, asciiGlyphs, theme.Default().Styles())

	if !strings.Contains(wide, "/Users/cam/x") {
		t.Errorf("wide row should include the path: %q", wide)
	}
	if strings.Contains(narrow, "/Users/cam/x") {
		t.Errorf("narrow row must drop the path: %q", narrow)
	}
	if !strings.Contains(narrow, "alpha") {
		t.Errorf("narrow row must still show the name: %q", narrow)
	}
}

// TestMenuActivatesFilteredRow pins #496: with a filter applied, the raw list
// index is a position in the FILTERED rows, so menuMode must act on the
// selected item, not on its index. Filtering to "Last" leaves one row at
// filtered index 0 — which the raw-index switch read as Pick.
func TestMenuActivatesFilteredRow(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	m.l.SetFilterText("Last")
	if got := len(m.l.VisibleItems()); got != 1 {
		t.Fatalf("filter left %d rows, want 1", got)
	}
	out, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = out.(model)
	if m.action.Kind != ActionLast {
		t.Errorf("enter on the filtered Last row: action = %+v, mode = %v; want ActionLast", m.action, m.mode)
	}
}

// TestMenuDigitBeyondFilteredRowsIsIgnored: with a filter leaving one row, a
// digit past it must do nothing rather than select a hidden row.
func TestMenuDigitBeyondFilteredRowsIsIgnored(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
	m.l.SetFilterText("Last")
	out, _ := m.Update(key("3"))
	m = out.(model)
	if m.action.Kind != 0 || m.mode != menuMode {
		t.Errorf("digit 3 with one visible row: action = %+v, mode = %v; want no action, still in the menu", m.action, m.mode)
	}
}

// nestedHubModel is hubTestModel's pr with a nested group, reviewed, whose
// own leaves are one runnable verb and one that needs an argument (#916).
func nestedHubModel() model {
	hub := []HubEntry{
		{Name: "pr", Short: "review a PR", Core: true, Key: 1, Leaves: []HubLeaf{
			{Name: "pr", Short: "review a PR", Use: "pr <ref>", NeedsArgs: true, Self: true},
			{Name: "list", Short: "list sessions", Use: "list"},
			{Name: "reviewed", Short: "manage marks", Use: "reviewed", Leaves: []HubLeaf{
				{Name: "mark", Short: "mark a PR", Use: "mark <ref>", NeedsArgs: true},
				{Name: "sync", Short: "prune marks", Use: "sync"},
			}},
		}},
	}
	return sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, Theme: theme.Default()}), 80, 24)
}

// TestHub_NestedGroupOpensItsLeaves pins #916: selecting a group leaf opens
// its subcommands instead of running the group, and a verb inside it runs
// with the whole path as argv.
func TestHub_NestedGroupOpensItsLeaves(t *testing.T) {
	m := nestedHubModel()
	out, _ := m.Update(key("1")) // pr row -> leavesMode
	m = out.(model)
	out, cmd := choose(m, "3") // the reviewed group
	m = out.(model)
	if m.action.Kind != ActionNone || cmd != nil {
		t.Fatalf("selecting a group ran something: %+v", m.action)
	}
	if m.mode != leavesMode || strings.Join(m.leavesPath, " ") != "pr reviewed" {
		t.Fatalf("group did not open: mode=%v path=%q", m.mode, m.leavesPath)
	}
	if got := m.selectedDollar(); !strings.Contains(got, "pr reviewed mark <ref>") {
		t.Errorf("dollar line = %q, want the nested usage", got)
	}
	out, cmd = choose(m, "2") // sync
	m = out.(model)
	if m.action.Kind != ActionRunVerb || strings.Join(m.action.Argv, " ") != "pr reviewed sync" || cmd == nil {
		t.Errorf("nested verb = %+v, want ActionRunVerb [pr reviewed sync]", m.action)
	}
}

// TestHub_NestedNeedsArgsLeafOpensPicker pins the picker prefix for an
// argument-taking verb inside a nested group.
func TestHub_NestedNeedsArgsLeafOpensPicker(t *testing.T) {
	m := nestedHubModel()
	out, _ := m.Update(key("1"))
	m = out.(model)
	out, _ = choose(m, "3")
	m = out.(model)
	out, cmd := choose(m, "1") // mark <ref>
	m = out.(model)
	if m.picker == nil || cmd != nil {
		t.Fatalf("mark did not open the picker: picker=%v action=%+v", m.picker != nil, m.action)
	}
	if got := strings.Join(m.picker.prefix, " "); got != "pr reviewed mark" || m.picker.placeholder != "<ref>" {
		t.Errorf("picker prefix/placeholder = %q/%q, want pr reviewed mark/<ref>", got, m.picker.placeholder)
	}
}

// TestHub_NestedGroupEscClimbsOneLevel pins esc inside a nested group: back
// to the list it was opened from with the group still selected, then to the
// hub.
func TestHub_NestedGroupEscClimbsOneLevel(t *testing.T) {
	m := nestedHubModel()
	out, _ := m.Update(key("1"))
	m = out.(model)
	out, _ = choose(m, "3")
	m = out.(model)
	out, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if cmd != nil || m.mode != leavesMode || strings.Join(m.leavesPath, " ") != "pr" {
		t.Fatalf("esc from a nested group: mode=%v path=%q quit=%v", m.mode, m.leavesPath, cmd != nil)
	}
	if it, ok := m.l.SelectedItem().(leafItem); !ok || it.leaf.Name != "reviewed" {
		t.Errorf("cursor after esc = %+v, want the reviewed row", m.l.SelectedItem())
	}
	if got := m.selectedDollar(); !strings.Contains(got, "pr reviewed <subcommand>") {
		t.Errorf("dollar line on a group row = %q, want pr reviewed <subcommand>", got)
	}
	out, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if m.mode != hubMode {
		t.Errorf("second esc: mode=%v, want hubMode", m.mode)
	}
}

// TestHub_ChildNamedLikeItsParent pins #948: the synthetic self leaf is told
// apart from a real subcommand by its Self flag, not by name. Under pr, the
// self leaf runs `pr <ref>` while a real child group also named "pr" opens,
// and its own verb runs as pr pr run; esc climbs back onto that group's row,
// not onto the self leaf that shares its name.
func TestHub_ChildNamedLikeItsParent(t *testing.T) {
	self := HubLeaf{Name: "pr", Use: "pr <ref>", NeedsArgs: true, Self: true}
	child := HubLeaf{Name: "pr", Use: "pr", Leaves: []HubLeaf{{Name: "run", Use: "run"}}}
	if got := strings.Join(leafArgv([]string{"pr"}, self), " "); got != "pr" {
		t.Errorf("leafArgv(self) = %q, want pr", got)
	}
	if got := strings.Join(leafArgv([]string{"pr"}, child), " "); got != "pr pr" {
		t.Errorf("leafArgv(child named pr) = %q, want pr pr", got)
	}

	hub := []HubEntry{{Name: "pr", Short: "review a PR", Core: true, Key: 1, Leaves: []HubLeaf{self, child}}}
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, Theme: theme.Default()}), 80, 24)
	out, _ := m.Update(key("1"))
	m = out.(model)
	out, _ = choose(m, "2") // the child group named pr
	m = out.(model)
	if strings.Join(m.leavesPath, " ") != "pr pr" {
		t.Fatalf("child group did not open: path=%q", m.leavesPath)
	}
	out, _ = choose(m, "1") // run
	m = out.(model)
	if m.action.Kind != ActionRunVerb || strings.Join(m.action.Argv, " ") != "pr pr run" {
		t.Errorf("nested verb = %+v, want ActionRunVerb [pr pr run]", m.action)
	}

	m = sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Hub: hub, Theme: theme.Default()}), 80, 24)
	out, _ = m.Update(key("1"))
	m = out.(model)
	out, _ = choose(m, "2")
	m = out.(model)
	out, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = out.(model)
	if it, ok := m.l.SelectedItem().(leafItem); !ok || it.leaf.Self || len(it.leaf.Leaves) == 0 {
		t.Errorf("cursor after esc = %+v, want the child group's row", m.l.SelectedItem())
	}
}

// choose picks a row in one of the hub's deeper lists the way a user does: a
// digit moves the cursor there, and enter acts on it.
func choose(m model, digit string) (tea.Model, tea.Cmd) {
	out, _ := m.Update(key(digit))
	return out.(model).Update(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// TestScopedMenuQuitsOnBack pins forgectl#1100: `forgectl tmux` opens the
// menu directly, so q and esc leave the program; they must not land in a
// hub the user never opened. From the hub's own tmux row they still back out
// to the hub.
//
// Mutation that turns it red: drop the m.scoped check in the menuMode arm of
// the q/esc handler.
func TestScopedMenuQuitsOnBack(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: tea.KeyEscape}} {
		m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
		if !strings.Contains(m.footerView(), "q/esc quit") {
			t.Errorf("scoped menu footer = %q, want q/esc quit", m.footerView())
		}
		_, cmd := m.Update(key)
		if cmd == nil {
			t.Fatalf("%v: no command, want tea.Quit", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%v: command did not quit", key)
		}
	}
	// Unscoped: the menu reached from the hub backs out to the hub.
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{NoIcons: true, Theme: theme.Default()}), 80, 24)
	m.toMenu()
	out, _ := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if got := out.(model).mode; got != hubMode {
		t.Errorf("unscoped menu q → mode %v, want hubMode", got)
	}
}

// TestKillOthersConfirmNamesTargets pins #1114: the hub's kill-others confirm
// names the sessions it will kill (as `tmux kill --others` does) and says esc
// cancels.
func TestKillOthersConfirmNamesTargets(t *testing.T) {
	rows := oneSessionRow + "\n" + "123" + sep + "456" + sep + "$2" + sep + "beta" + sep +
		"1" + sep + "0" + sep + "1700000001" + sep + "/tmp"
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return rows, nil }}
	m := sized(newModel(context.Background(), tmux.New(fake), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)

	out, _ := m.Update(key("2"))
	m = out.(model)
	// Sessions sort order is not pinned here; keep whichever is selected.
	sel := m.l.SelectedItem().(sessionItem).s.Name
	other := map[string]string{"alpha": "beta", "beta": "alpha"}[sel]
	out, _ = m.Update(key("K"))
	m = out.(model)

	view := m.View().Content
	for _, want := range []string{`Keep "` + sel + `"`, `"` + other + `"`, "esc to cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("kill-others confirm missing %q in:\n%s", want, view)
		}
	}
	if strings.Contains(view, "ALL") {
		t.Errorf("kill-others confirm still says ALL:\n%s", view)
	}
}
