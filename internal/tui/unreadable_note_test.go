package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// TestScreensNoteUnreadableRows is forgectl#815 item 2 in the TUI: the
// sessions, windows and tree screens say in the footer that tmux returned
// rows they could not read, instead of showing a smaller server.
//
// Mutation that turns it red: drop the noteUnreadable call from any one of
// enterSessions, enterWindows or enterTree (that screen's footer is empty).
func TestScreensNoteUnreadableRows(t *testing.T) {
	window := func(id, name string) string {
		return strings.Join([]string{"123", "456", id, "$1", "alpha", "0", name, "1", "1"}, sep)
	}
	hidden := strings.Replace(oneSessionRow, "$1"+sep+"alpha", "$2"+sep+"hid"+sep+"den", 1)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return oneSessionRow + "\n" + hidden, nil
		case "list-windows":
			return window("@1", "ok") + "\n" + window("@2", "a"+sep+"b"), nil
		}
		return "", nil
	}}
	for menuKey, want := range map[string]string{
		"2": "1 session(s) could not be read",
		"3": "1 window(s) could not be read",
		"4": "1 session(s) and 1 window(s) could not be read",
	} {
		m := sized(newModel(context.Background(), tmux.New(fake), RunOptions{StartInTmux: true, NoIcons: true, Theme: theme.Default()}), 80, 24)
		out, _ := m.Update(key(menuKey))
		m = out.(model)
		if !strings.Contains(m.status, want) {
			t.Errorf("screen %s footer = %q, want %q", menuKey, m.status, want)
		}
	}
}

// TestNoteUnreadableKeepsTheMutationResult: a sessions reload right after a
// kill or rename appends the note, so the mutation's own result stays visible.
//
// Mutation that turns it red: assign the note to m.status instead of
// appending it.
func TestNoteUnreadableKeepsTheMutationResult(t *testing.T) {
	m := newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{Theme: theme.Default()})
	m.status = "killed alpha"
	m.noteUnreadable(tmux.UnreadableRows{Sessions: 1})
	if !strings.Contains(m.status, "killed alpha") || !strings.Contains(m.status, "could not be read") {
		t.Fatalf("status = %q, want both the mutation result and the note", m.status)
	}
	m.status = ""
	m.noteUnreadable(tmux.UnreadableRows{})
	if m.status != "" {
		t.Fatalf("status = %q, want nothing when every row was read", m.status)
	}
}
