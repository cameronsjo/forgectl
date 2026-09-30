//go:build unix

package pr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// deadSocketServer is the tmux state a server leaves after a clean exit on
// tmux 3.4 (forgectl#786). The default socket file exists, a connect to it is
// refused, and every tmux command fails exit 1 with "no server running". The
// socket is a real one in a private TMUX_TMPDIR, so the tmux client's own
// lstat and connect probe classify it; only the tmux commands are faked.
func deadSocketServer(t *testing.T) *exec.FakeRunner {
	t.Helper()
	// Not t.TempDir(): a unix socket path is capped near 104 bytes on macOS,
	// and t.TempDir() embeds the full test name.
	root, err := os.MkdirTemp("/tmp", "f786-pr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_TMPDIR", root)
	socketPath := filepath.Join(root, "tmux-"+strconv.Itoa(os.Getuid()), "default")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("bind the leftover socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" {
			return "", nil
		}
		return "", &exec.CommandError{
			Name: name, Args: append([]string(nil), args...), ExitCode: 1,
			Stderr: "no server running on " + socketPath, Err: errors.New("exit status 1"),
		}
	}}
}

// TestOccupancyCountsAnExitedServerAsZero covers #786's admission half. After
// the last review window closes, the server exits and leaves its socket. The
// live-window count must read that as zero rather than refuse every admission.
// So must `pr list`'s display read.
//
// Mutation that turns it red: have countableWindows return ListWindows'
// error unchanged.
func TestOccupancyCountsAnExitedServerAsZero(t *testing.T) {
	c := testClient(t, deadSocketServer(t))
	ctx := context.Background()

	if n, ok := c.LiveReviews(ctx); !ok || n != 0 {
		t.Fatalf("LiveReviews = (%d, %v), want (0, true)", n, ok)
	}
	if n, err := c.occupancyFrom(ctx, nil); err != nil || n != 0 {
		t.Fatalf("occupancyFrom = (%d, %v), want (0, nil)", n, err)
	}
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	if live, ok := c.WindowsLiveForListing(ctx, []Ref{ref}); !ok || live[ref] {
		t.Fatalf("WindowsLiveForListing = (%v, %v), want not live and readable", live, ok)
	}
}

// TestExitedServerIsNeverGone is the other half. Every read that gates a
// removal keeps failing closed on the same state: WindowsLive (repair's
// rollback and forget, prune, drain's retry), VerifyDispatched (reports
// reviews gone), and teardown (removes the workspace). A refused connect
// proves no server listens now, not that a crashed server's panes died with
// it (#746, #765).
//
// Mutations that turn it red: point WindowsLive at countableWindows; let
// windowConfirmedAbsent accept tmux.ErrServerExited.
func TestExitedServerIsNeverGone(t *testing.T) {
	fake := deadSocketServer(t)
	c := testClient(t, fake)
	WithDispatchWait(func(context.Context) error { return nil })(c)
	ctx := context.Background()
	ref := Ref{Owner: "o", Repo: "r", Number: 2}

	if live, ok := c.WindowsLive(ctx, []Ref{ref}); ok {
		t.Fatalf("WindowsLive = (%v, true); an exited server's socket must read as unknown", live)
	}
	gone, err := c.VerifyDispatched(ctx, []Dispatch{{Ref: ref, WindowID: "1" + tmux.FieldSep + "2" + tmux.FieldSep + "@1"}})
	if err == nil || len(gone) != 0 {
		t.Fatalf("VerifyDispatched = (%v, %v), want an error and no review reported gone", gone, err)
	}

	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhasePrepared, ws)
	err = c.Teardown(ctx, path)
	if !errors.Is(err, ErrWindowStateUnreadable) || !errors.Is(err, tmux.ErrServerExited) {
		t.Fatalf("Teardown = %v, want ErrWindowStateUnreadable wrapping tmux.ErrServerExited", err)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("the workspace must be kept when only an exited server's socket answered: %v", serr)
	}
	if bc := readRecord(t, path); bc.Phase != PhaseNeedsRepair {
		t.Errorf("record phase = %q, want needs-repair", bc.Phase)
	}
	if _, ok := findCallVerb(fake.Calls, "tmux", "kill-window"); ok {
		t.Error("nothing may be killed on an exited server's socket")
	}
}
