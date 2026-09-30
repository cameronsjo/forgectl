//go:build unix

package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestStartDirectoryIsNotFormatExpandedIsolated is forgectl#839 on a real
// tmux. tmux format-expands a -c start directory, so a directory whose path
// holds `#(cmd)` ran cmd in the tmux server, and one holding `#{...}` or `##`
// was rewritten into a path that does not exist, which tmux answers by
// starting the session in $HOME. Every hostile directory must now start the
// session and the window in exactly that directory, and the `#(...)` one must
// start no job.
//
// The root is resolved through EvalSymlinks first, because macOS's /tmp is a
// link to /private/tmp and a pane reports its resolved cwd.
//
// Mutations that turn it red: pass dir without escapeDirOperand at
// CreateSession's -c (the session path and pane cwd differ, and the marker
// appears), or at NewWindowWithEnv's -c (the window's pane cwd differs, and
// the marker appears).
func TestStartDirectoryIsNotFormatExpandedIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()

	tmp, err := os.MkdirTemp("/tmp", "f839-dir-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ran")

	// The `#(...)` case spans path components (the marker path has slashes),
	// which is fine: it is one directory path, created with MkdirAll.
	hostile := []string{
		"a#(touch " + marker + ")",
		"b#{session_name}",
		"c##d",
		"e#[x",
		"f#",
		"g#;",
	}
	for i, leaf := range hostile {
		dir := filepath.Join(root, leaf)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("s%d", i)
		session, err := c.CreateSession(ctx, name, dir)
		if err != nil {
			t.Fatalf("CreateSession(-c %q): %v", dir, err)
		}
		sessions, err := c.ListSessions(ctx)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		found := false
		for _, s := range sessions {
			if s.Name == name {
				found = true
				if s.Path != dir {
					t.Errorf("session %s path = %q, want exactly %q", name, s.Path, dir)
				}
			}
		}
		if !found {
			t.Fatalf("session %s is not listed", name)
		}
		if got := paneCwd(t, runner, tmuxBin, session.ID); got != dir {
			t.Errorf("session %s's pane started in %q, want exactly %q", name, got, dir)
		}

		window, err := c.NewWindow(ctx, session, "w", dir)
		if err != nil {
			t.Fatalf("NewWindow(-c %q): %v", dir, err)
		}
		if got := paneCwd(t, runner, tmuxBin, window.ID); got != dir {
			t.Errorf("window in %s started in %q, want exactly %q", name, got, dir)
		}
	}

	// A `#(...)` job runs asynchronously in the server, so absence is only
	// meaningful after it has had time to run. Unescaped, the marker appears
	// well inside this window on tmux 3.4.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a -c directory holding #(touch ...) ran the command in the tmux server")
	}
}

// paneCwd reads the current path of the active pane in target, polling
// briefly because a just-spawned shell may not have reached its cwd yet on
// every platform.
func paneCwd(t *testing.T, runner internalexec.Runner, tmuxBin, target string) string {
	t.Helper()
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := runner.Run(context.Background(), tmuxBin, "display-message", "-p", "-t", target, "#{pane_current_path}")
		if err != nil {
			t.Fatalf("display-message -t %s: %v", target, err)
		}
		got = strings.TrimRight(out, "\n")
		if got != "" || time.Now().After(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}
