package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// TestTmuxLsNotesUnreadableSessions is forgectl#806 at the CLI: a session
// whose name carries the 0x1F field separator cannot be listed, and `tmux ls`
// says so on stderr instead of showing one session fewer with no sign of it.
// The --json array keeps its shape.
//
// Mutation that turns it red: drop the unreadable-count note from tmux ls.
func TestTmuxLsNotesUnreadableSessions(t *testing.T) {
	row := func(id, name string) string {
		return strings.Join([]string{"123", "456", id, name, "1", "0", "1700000000", "/w"}, "\x1f")
	}
	out := row("$0", "work") + "\n" + row("$1", "hid\x1fden")
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-sessions" {
			return out, nil
		}
		return "", nil
	}}
	for _, args := range [][]string{nil, {"--json"}} {
		cmd := newTmuxLsCmd(tmux.New(fake))
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("tmux ls %v: %v", args, err)
		}
		if !strings.Contains(stderr.String(), "1 session(s) could not be read") {
			t.Errorf("tmux ls %v stderr = %q, want the unreadable-session note", args, stderr.String())
		}
		if len(args) > 0 {
			var rows []map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) != 1 {
				t.Errorf("tmux ls --json = %q (%v), want a one-row array", stdout.String(), err)
			}
		} else if !strings.Contains(stdout.String(), "work") {
			t.Errorf("tmux ls = %q, want the readable session", stdout.String())
		}
	}
}

// TestTmuxTreeNotesUnreadableRows is forgectl#815 item 2 at the CLI: `tmux
// tree` says on stderr, in both modes, that a session, a window and a pane
// (forgectl#823) could not be read, as `tmux ls` does, instead of drawing a
// smaller server.
//
// Mutation that turns it red: drop either writeUnreadableNote call in
// tmux_tree.go (that mode's stderr is empty), or leave the pane count out of
// the --json mode's note.
func TestTmuxTreeNotesUnreadableRows(t *testing.T) {
	session := func(id, name string) string {
		return strings.Join([]string{"123", "456", id, name, "1", "0", "1700000000", "/w"}, "\x1f")
	}
	window := func(id, name string) string {
		return strings.Join([]string{"123", "456", id, "$0", "work", "0", name, "1", "1"}, "\x1f")
	}
	pane := func(id, command string) string {
		return strings.Join([]string{"123", "456", id, "@0", "0", "title", command, "1"}, "\x1f")
	}
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return session("$0", "work") + "\n" + session("$1", "hid\x1fden"), nil
		case "list-windows":
			return window("@0", "ok") + "\n" + window("@1", "a\x1fb"), nil
		case "list-panes":
			return pane("%0", "zsh") + "\n" + pane("%1", "a\x1fb"), nil
		}
		return "", nil
	}}
	for _, args := range [][]string{nil, {"--json"}} {
		cmd := newTmuxTreeCmd(tmux.New(fake))
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("tmux tree %v: %v", args, err)
		}
		if !strings.Contains(stderr.String(), "1 session(s), 1 window(s) and 1 pane(s) could not be read") {
			t.Errorf("tmux tree %v stderr = %q, want the unreadable-rows note", args, stderr.String())
		}
		if !strings.Contains(stdout.String(), "work") {
			t.Errorf("tmux tree %v = %q, want the readable session", args, stdout.String())
		}
	}
}

// TestTmuxWindowsNotesUnreadableRows is forgectl#857: `tmux windows` says on
// stderr, in both modes, that a window row could not be read, as `tmux tree`
// does, instead of listing one window fewer with no sign of it. The --json
// array keeps its shape.
//
// Mutation that turns it red: call DisplayWindows (dropping the count) or
// drop the writeUnreadableNote call in tmux_window.go.
func TestTmuxWindowsNotesUnreadableRows(t *testing.T) {
	window := func(id, name string) string {
		return strings.Join([]string{"123", "456", id, "$0", "work", "0", name, "1", "1"}, "\x1f")
	}
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "list-windows" {
			return window("@0", "ok") + "\n" + window("@1", "a\x1fb"), nil
		}
		return "", nil
	}}
	for _, args := range [][]string{nil, {"--json"}} {
		cmd := newTmuxWindowsCmd(tmux.New(fake))
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("tmux windows %v: %v", args, err)
		}
		if !strings.Contains(stderr.String(), "1 window(s) could not be read") {
			t.Errorf("tmux windows %v stderr = %q, want the unreadable-window note", args, stderr.String())
		}
		if len(args) > 0 {
			var rows []map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) != 1 {
				t.Errorf("tmux windows --json = %q (%v), want a one-row array", stdout.String(), err)
			}
		} else if !strings.Contains(stdout.String(), "ok") {
			t.Errorf("tmux windows = %q, want the readable window", stdout.String())
		}
	}
}
