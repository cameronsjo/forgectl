package tmux

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// fakeFileInfo is an lstat answer with a chosen file type.
type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "default" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// deadSocketClient is an environmental client whose default socket lstat
// answers info and whose connect probe answers dialErr.
func deadSocketClient(run internalexec.Runner, info os.FileInfo, dialErr error) (*Client, *int) {
	c := New(run)
	identityEnv(c, "", "/tmp")
	c.getuid = func() int { return 501 }
	c.lstat = func(string) (os.FileInfo, error) { return info, nil }
	dials := 0
	c.dialSocket = func(context.Context, string) error {
		dials++
		return dialErr
	}
	return c, &dials
}

// TestClassifyDeadSocket pins what counts as proof that a leftover socket's
// server is gone (forgectl#786). Only a file that IS a socket AND refuses the
// connect qualifies. Every other answer stays the fail-closed stale verdict.
//
// Mutations that turn it red: drop the ModeSocket check (the regular-file row
// becomes dead); accept any dial error (the permission row becomes dead);
// remove the serverDeadSocket arm (the first row stays stale).
func TestClassifyDeadSocket(t *testing.T) {
	args := []string{"list-sessions", "-F", sessionFormat}
	socket := fakeFileInfo{mode: fs.ModeSocket | 0o600}
	tests := []struct {
		name     string
		info     os.FileInfo
		dialErr  error
		want     serverFailureKind
		wantDial bool
	}{
		{"socket refusing connect", socket, syscall.ECONNREFUSED, serverDeadSocket, true},
		{"socket accepting connect", socket, nil, serverStaleSocket, true},
		{"socket denying permission", socket, syscall.EACCES, serverStaleSocket, true},
		{"regular file refusing connect", fakeFileInfo{mode: 0o600}, syscall.ECONNREFUSED, serverStaleSocket, false},
		{"no file info", nil, syscall.ECONNREFUSED, serverStaleSocket, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dials := deadSocketClient(&internalexec.FakeRunner{}, tt.info, tt.dialErr)
			got := c.classifyServerFailure(context.Background(), args, commandFailure("tmux", args, "no server running"))
			if got.Kind != tt.want {
				t.Fatalf("kind = %v, want %v", got.Kind, tt.want)
			}
			if (*dials > 0) != tt.wantDial {
				t.Fatalf("dialed %d times, want dial=%v", *dials, tt.wantDial)
			}
		})
	}
}

// TestClassifyDeadSocketNeedsTwoRefusals is the forgectl#806 macOS hardening:
// on Darwin a live server with a full listen backlog also refuses a connect,
// so one refusal is not enough to call the socket dead. Only a second refusal
// after the pause is.
//
// Mutation that turns it red: return true after the first refusal (the
// "refused, then accepted" row reads dead, and the dead row dials once).
func TestClassifyDeadSocketNeedsTwoRefusals(t *testing.T) {
	args := []string{"list-sessions", "-F", sessionFormat}
	socket := fakeFileInfo{mode: fs.ModeSocket | 0o600}
	for name, tc := range map[string]struct {
		answers []error
		want    serverFailureKind
	}{
		"refused twice":               {[]error{syscall.ECONNREFUSED, syscall.ECONNREFUSED}, serverDeadSocket},
		"refused, then accepted":      {[]error{syscall.ECONNREFUSED, nil}, serverStaleSocket},
		"refused, then another error": {[]error{syscall.ECONNREFUSED, syscall.EAGAIN}, serverStaleSocket},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := deadSocketClient(&internalexec.FakeRunner{}, socket, nil)
			var dials int
			c.dialSocket = func(context.Context, string) error {
				answer := tc.answers[dials]
				dials++
				return answer
			}
			got := c.classifyServerFailure(context.Background(), args, commandFailure("tmux", args, "no server running"))
			if got.Kind != tc.want {
				t.Fatalf("kind = %v, want %v", got.Kind, tc.want)
			}
			if dials != 2 {
				t.Fatalf("dialed %d times, want 2", dials)
			}
		})
	}

	// A canceled context during the pause is not a second refusal.
	c, _ := deadSocketClient(&internalexec.FakeRunner{}, socket, nil)
	ctx, cancel := context.WithCancel(context.Background())
	c.dialSocket = func(context.Context, string) error {
		cancel()
		return syscall.ECONNREFUSED
	}
	if c.socketRefusesConnect(ctx, "/tmp/tmux-501/default", socket) {
		t.Fatal("a probe canceled during the pause read the socket as dead")
	}
}

// TestDeadSocketIsEmptyOnlyForOptedInCallers is the #786 split. A dead socket
// lists as empty for display, and EnsureSession creates over it. A kill-time
// revalidation still refuses: it must not turn a dead socket into
// ErrObjectGone, because a refused connect proves no server listens NOW, not
// that a crashed server's panes died with it (#765).
//
// Mutations that turn it red: read serverDeadSocket as absent in absentServer
// (the revalidation becomes ErrObjectGone); drop the ErrServerExited arm from
// any Display*Listing (that display listing errors); drop EnsureSession's
// ErrServerExited arm (the create never runs).
func TestDeadSocketIsEmptyOnlyForOptedInCallers(t *testing.T) {
	socket := fakeFileInfo{mode: fs.ModeSocket | 0o600}
	created := false
	fake := &internalexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) > 0 && args[0] == "new-session" {
			created = true
			return strings.Join([]string{"77", "88", "$0"}, FieldSep), nil
		}
		return "", commandFailure("tmux", args, "no server running on /tmp/tmux-501/default")
	}}
	c, _ := deadSocketClient(fake, socket, syscall.ECONNREFUSED)
	ctx := context.Background()

	if sessions, err := c.DisplaySessions(ctx); err != nil || len(sessions) != 0 {
		t.Fatalf("DisplaySessions = (%v, %v), want empty, nil", sessions, err)
	}
	if windows, err := c.DisplayWindows(ctx); err != nil || len(windows) != 0 {
		t.Fatalf("DisplayWindows = (%v, %v), want empty, nil", windows, err)
	}
	if panes, unreadable, err := c.DisplayPaneListing(ctx); err != nil || len(panes) != 0 || unreadable != 0 {
		t.Fatalf("DisplayPaneListing = (%v, %d, %v), want empty, 0, nil", panes, unreadable, err)
	}

	_, err := c.ListSessions(ctx)
	if !errors.Is(err, ErrServerUnreadable) || !errors.Is(err, ErrServerExited) {
		t.Fatalf("ListSessions = %v, want ErrServerUnreadable wrapping ErrServerExited", err)
	}
	if errors.Is(err, ErrNoServer) {
		t.Fatalf("ListSessions = %v; a dead socket must not read as the create-permitting ErrNoServer", err)
	}
	// forgectl#805: every refusal that wraps this carries the remedy, since the
	// strict callers (repair, teardown, prune) otherwise print no next step.
	// Mutation that turns it red: drop the remedy from ErrServerExited's text.
	if !strings.Contains(err.Error(), "start any tmux session to clear the socket, then retry") {
		t.Errorf("ListSessions = %q, want the exited-server remedy in the message", err)
	}

	gen := ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"}
	_, err = c.RevalidateSession(ctx, SessionIdentity{Generation: gen, ID: "$1", Name: "alpha"})
	if errors.Is(err, ErrObjectGone) || !errors.Is(err, ErrServerUnreadable) {
		t.Fatalf("RevalidateSession = %v, want ErrServerUnreadable and never ErrObjectGone", err)
	}
	_, err = c.RevalidateWindow(ctx, WindowIdentity{Generation: gen, ID: "@1", SessionID: "$1", Name: "w"})
	if errors.Is(err, ErrObjectGone) || !errors.Is(err, ErrServerUnreadable) {
		t.Fatalf("RevalidateWindow = %v, want ErrServerUnreadable and never ErrObjectGone", err)
	}

	identity, err := c.EnsureSession(ctx, "fresh", "")
	if err != nil || !created || identity.ID != "$0" {
		t.Fatalf("EnsureSession over a dead socket = (%+v, %v), created=%v; want a new $0", identity, err, created)
	}
}
