//go:build unix

package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestDeadSocketAfterCleanExitIsolated reproduces forgectl#786 on tmux 3.4.
// A server that exits because its last session was killed leaves its socket
// file behind. The display listing must read that as empty, and EnsureSession
// must create over it. The kill-time revalidation must still refuse to call
// anything gone.
//
// Mutation that turns it red: remove the serverDeadSocket arm from
// classifyServerFailure. DisplaySessionListing then fails with ErrServerUnreadable,
// and EnsureSession refuses to create.
func TestDeadSocketAfterCleanExitIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "last", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	last, err := c.ResolveSessionExact(ctx, "last")
	if err != nil {
		t.Fatalf("ResolveSessionExact: %v", err)
	}
	if err := c.KillSession(ctx, last); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	socketPath := filepath.Join(os.Getenv("TMUX_TMPDIR"), "tmux-"+strconv.Itoa(os.Getuid()), "default")
	if info, err := os.Lstat(filepath.Clean(socketPath)); err != nil || info.Mode().Type() != os.ModeSocket {
		t.Skipf("this tmux unlinked its socket on exit (lstat %v); the #786 state does not arise", err)
	}

	if sessions, unreadable, err := c.DisplaySessionListing(ctx); err != nil || len(sessions) != 0 || unreadable != 0 {
		t.Fatalf("DisplaySessionListing = (%v, %d, %v), want empty, 0, nil", sessions, unreadable, err)
	}
	if tree, err := c.Tree(ctx, false); err != nil {
		t.Fatalf("Tree over an exited server: %v (%q)", err, tree)
	}
	if _, err := c.ListSessions(ctx); !errors.Is(err, ErrServerExited) || !errors.Is(err, ErrServerUnreadable) {
		t.Fatalf("ListSessions = %v, want ErrServerUnreadable wrapping ErrServerExited", err)
	}
	if _, err := c.RevalidateSession(ctx, last); errors.Is(err, ErrObjectGone) || err == nil {
		t.Fatalf("RevalidateSession = %v; an exited server must not prove the session gone", err)
	}

	created, err := c.EnsureSession(ctx, "fresh", "")
	if err != nil {
		t.Fatalf("EnsureSession over the leftover socket: %v", err)
	}
	if _, err := c.RevalidateSession(ctx, created); err != nil {
		t.Fatalf("the created session does not revalidate: %v", err)
	}
}
