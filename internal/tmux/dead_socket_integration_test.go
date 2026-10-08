//go:build unix

package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
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
//
// The server exits after kill-session returns, not before, so the test waits
// for it to stop listening (waitForServerExit) before it reads anything.
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
	waitForServerExit(t, socketPath)
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

// waitForServerExit blocks until the tmux server listening on socketPath has
// stopped listening: a connect is refused, or the socket is gone. kill-session
// returns once the server has destroyed the session, but the server leaves
// its event loop and closes its listener only afterwards, and under host load
// that can take a while. A command sent in that window reaches a live server
// with no sessions: Tree's list-windows -a fails with "no current target"
// over a socket that still accepts, which the classifier rightly reads as
// unreadable rather than exited. Under twelve CPU burners that failed 7 of
// 900 runs, and none with this wait (forgectl#919). The deadline only
// backstops a server that never exits.
func waitForServerExit(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := dialUnixSocket(context.Background(), socketPath)
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tmux server on %s still accepts connections 30s after its last session was killed (last dial: %v)", socketPath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
