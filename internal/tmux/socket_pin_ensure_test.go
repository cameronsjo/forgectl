package tmux

import (
	"context"
	"slices"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestPinnedListSessionsReturnsEmptyNotErrorWhenServerAbsent exercises the
// integration the mutation sweep's direct serverStateError test (see
// TestPinnedClientMayCreateItsFirstServer) does not: the actual
// ListSessions -> absentServer -> (nil, nil) short-circuit, driven by a
// pinned client's own list-sessions failure rather than by calling
// serverStateError directly. A pinned client whose socket is absent must
// read "no sessions" the same way an unpinned one reads "no default server" —
// not surface ErrNoServer up through ListSessions.
func TestPinnedListSessionsReturnsEmptyNotErrorWhenServerAbsent(t *testing.T) {
	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			return "", commandFailure("tmux", args, "no server running")
		},
	}
	c := pinnedClient(t, run)

	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions = %v, want nil error for an absent pinned server", err)
	}
	if sessions != nil {
		t.Errorf("sessions = %v, want nil", sessions)
	}
	if len(run.Calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(run.Calls))
	}
	if run.Calls[0].Args[0] != "-S" || run.Calls[0].Args[1] != testSocket {
		t.Errorf("argv %v was not pinned", run.Calls[0].Args)
	}
}

// TestEnsureSessionCreatesFirstServerWhenPinned drives EnsureSession's full
// resolve-then-create sequence under a pin, which
// TestPinnedClientMayCreateItsFirstServer does not: that test asserts the
// classification directly against serverStateError, never through
// EnsureSession's actual list -> not-found -> create chain. This is the path
// forgectl#332 exists to fix — a pinned client bringing up its OWN first
// server rather than being told to go look at the default one.
func TestEnsureSessionCreatesFirstServerWhenPinned(t *testing.T) {
	const pid, start = "9", "1"
	created := false
	run := &internalexec.FakeRunner{
		RunFunc: func(_ string, args []string) (string, error) {
			switch {
			case slices.Contains(args, "list-sessions"):
				return "", commandFailure("tmux", args, "no server running")
			case slices.Contains(args, "new-session"):
				created = true
				return strings.Join([]string{pid, start, "$1"}, FieldSep), nil
			}
			return "", nil
		},
	}
	c := pinnedClient(t, run)

	identity, err := c.EnsureSession(context.Background(), "forge", "/tmp")
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if !created {
		t.Fatal("EnsureSession never reached CreateSession — the absent-server verdict was not treated as create-permitting")
	}
	if identity.ID != "$1" {
		t.Errorf("identity.ID = %q, want $1", identity.ID)
	}
	want := ServerSelector{Socket: testSocket}
	if identity.Generation.Selector != want {
		t.Errorf("minted selector = %+v, want %+v", identity.Generation.Selector, want)
	}

	var createArgs []string
	for _, call := range run.Calls {
		if slices.Contains(call.Args, "new-session") {
			createArgs = call.Args
		}
	}
	if createArgs == nil {
		t.Fatal("new-session never reached the runner")
	}
	if createArgs[0] != "-S" || createArgs[1] != testSocket {
		t.Errorf("new-session argv = %v, want it pinned to %s", createArgs, testSocket)
	}
}
