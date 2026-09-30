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
