package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

const sep = "\x1f"

func key(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func sized(m model, w, h int) model {
	out, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return out.(model)
}

func TestMenuViewRenders(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), true), 80, 24)
	view := m.View()
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
		return "alpha" + sep + "1" + sep + "0" + sep + "1700000000" + sep + "/tmp", nil
	}}
	m := sized(newModel(context.Background(), tmux.New(fake), true), 80, 24)

	out, _ := m.Update(key("2"))
	m = out.(model)
	if m.mode != sessionsMode {
		t.Fatalf("expected sessionsMode after '2', got %v", m.mode)
	}

	out, _ = m.Update(key("1"))
	m = out.(model)
	if m.action.Kind != ActionAttach || m.action.Target != "alpha" {
		t.Errorf("expected attach alpha, got %+v", m.action)
	}
}

func TestCheatFromMenu(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), true), 80, 24)
	out, _ := m.Update(key("6")) // Cheatsheet
	m = out.(model)
	if m.mode != cheatMode {
		t.Fatalf("expected cheatMode after '6', got %v", m.mode)
	}
	if !strings.Contains(m.View(), "pane") {
		t.Errorf("cheat view should explain 'pane'")
	}
}

func TestCheatsheetContent(t *testing.T) {
	cs := Cheatsheet(true)
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
		return "alpha" + sep + "1" + sep + "0" + sep + "1700000000" + sep + "/tmp", nil
	}}
	m := sized(newModel(context.Background(), tmux.New(fake), true), 80, 24)

	out, _ := m.Update(key("2"))
	m = out.(model)
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")})
	m = out.(model)

	if m.mode != formMode {
		t.Fatalf("expected formMode after 'K', got %v", m.mode)
	}
	if m.pendingOp != opKillOthers {
		t.Errorf("expected opKillOthers, got %v", m.pendingOp)
	}
	if m.pendingTarget != "alpha" {
		t.Errorf("expected target alpha, got %q", m.pendingTarget)
	}
}

func TestLastFromMenu(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), true), 80, 24)
	out, _ := m.Update(key("5")) // Last
	m = out.(model)
	if m.action.Kind != ActionLast {
		t.Errorf("expected ActionLast, got %+v", m.action)
	}
}

func TestEscFromSubscreenReturnsToMenu(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), true), 80, 24)
	out, _ := m.Update(key("3")) // Windows
	m = out.(model)
	if m.mode != windowsMode {
		t.Fatalf("expected windowsMode, got %v", m.mode)
	}
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = out.(model)
	if m.mode != menuMode {
		t.Errorf("esc should return to menu, got %v", m.mode)
	}
}

func TestSessionItemNarrowDropsMetadata(t *testing.T) {
	// Narrow rows (iPhone/Termius) must drop the windows/path metadata column;
	// wide rows must include it.
	it := sessionItem{s: tmux.Session{Name: "alpha", Windows: 3, Path: "/Users/cam/x"}}
	wide := it.render(0, false, false, asciiGlyphs)
	narrow := it.render(0, false, true, asciiGlyphs)

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
