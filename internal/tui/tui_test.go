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
		{Name: "tmux", Short: "sessions, windows, tree", Core: true},
		{Name: "doctor", Short: "health check", Core: false},
		{Name: "pr", Short: "review a PR", Core: true, Leaves: []HubLeaf{
			{Name: "pr", Short: "review a PR", Use: "pr <ref>", NeedsArgs: true},
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

// TestHub_NeedsArgsLeafShowsInvocationWithoutRunning pins the pr <ref>
// contract: selecting it quits with ActionShowInvocation and the
// placeholder still in the Argv, never ActionRunVerb.
func TestHub_NeedsArgsLeafShowsInvocationWithoutRunning(t *testing.T) {
	m := hubTestModel()
	out, _ := m.Update(key("3")) // pr row -> leavesMode
	m = out.(model)
	if m.mode != leavesMode {
		t.Fatalf("expected leavesMode, got %v", m.mode)
	}
	out, cmd := m.Update(key("1")) // pr's own NeedsArgs leaf
	m = out.(model)
	if m.action.Kind != ActionShowInvocation {
		t.Fatalf("expected ActionShowInvocation, got %+v", m.action)
	}
	if got := strings.Join(m.action.Argv, " "); got != "pr <ref>" {
		t.Errorf("Argv joined = %q, want %q", got, "pr <ref>")
	}
	if cmd == nil {
		t.Error("a NeedsArgs leaf should still quit (back to the shell)")
	}
}

// TestHub_LeafRunsWithFullArgv pins the complete-argv case: selecting
// "list" under pr's leaves quits with ActionRunVerb{Argv: [pr list]}.
func TestHub_LeafRunsWithFullArgv(t *testing.T) {
	m := hubTestModel()
	out, _ := m.Update(key("3")) // pr row -> leavesMode
	m = out.(model)
	out, _ = m.Update(key("2")) // pr's "list" leaf
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
	wide := it.render(0, false, false, asciiGlyphs, theme.Default().Styles())
	narrow := it.render(0, false, true, asciiGlyphs, theme.Default().Styles())

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
