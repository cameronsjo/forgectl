//go:build unix

package pr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// So must `pr list`'s display read, which also reports that it saw an exited
// server so the row can say so (forgectl#805).
//
// Mutations that turn it red: have countableWindows return ListWindows'
// error unchanged; drop WindowsLiveForListing's ErrServerExited arm (the
// listing reads as unreadable and serverExited stays false).
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
	if live, ok, exited := c.WindowsLiveForListing(ctx, []Ref{ref}); !ok || !exited || live[ref] {
		t.Fatalf("WindowsLiveForListing = (%v, %v, %v), want not live, readable, server exited", live, ok, exited)
	}
}

// TestRepairRefusalOnAnExitedServerNamesTheRemedy is forgectl#805 item 4 on
// repair: rollback and forget still refuse on an exited server's leftover
// socket, but say how to clear it rather than "check tmux list-windows", which
// only prints "no server running". Nothing is removed either way.
//
// Mutations that turn it red: have windowListUnreadable return the generic
// reason for every error; put back either verb's fixed "could not be read"
// refusal.
func TestRepairRefusalOnAnExitedServerNamesTheRemedy(t *testing.T) {
	const remedy = "start any tmux session to clear the socket"
	for name, opts := range map[string]RepairOpts{
		"rollback": {Apply: true, Rollback: true, Yes: true},
		"forget":   {Apply: true, ForgetIfAbsent: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := repairClient(t, deadSocketServer(t))
			ws := fakeWorkspace(t)
			path := seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 3}, PhaseLaunching, ws)
			opts.Record = path
			_, err := c.Repair(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), remedy) {
				t.Fatalf("Repair(%s) over an exited server = %v, want a refusal naming the remedy", name, err)
			}
			if _, serr := os.Stat(ws); serr != nil {
				t.Errorf("a refusal removed the workspace: %v", serr)
			}
			if _, serr := os.Stat(path); serr != nil {
				t.Errorf("a refusal removed the record: %v", serr)
			}
		})
	}
	if got := windowListUnreadable(errors.New("boom")); strings.Contains(got, remedy) {
		t.Errorf("repair refusal on an unreadable tmux = %q; the exited-server remedy does not apply there", got)
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
