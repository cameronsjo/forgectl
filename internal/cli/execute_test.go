package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

func TestInteractiveTTY_RequiresBothDescriptors(t *testing.T) {
	for _, tt := range []struct {
		name            string
		stdinTTY        bool
		stdoutTTY       bool
		wantInteractive bool
	}{
		{name: "neither", stdinTTY: false, stdoutTTY: false, wantInteractive: false},
		{name: "stdout only", stdinTTY: false, stdoutTTY: true, wantInteractive: false},
		{name: "stdin only", stdinTTY: true, stdoutTTY: false, wantInteractive: false},
		{name: "both", stdinTTY: true, stdoutTTY: true, wantInteractive: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := interactiveTTY(tt.stdinTTY, tt.stdoutTTY); got != tt.wantInteractive {
				t.Errorf("interactiveTTY(%t, %t) = %t, want %t", tt.stdinTTY, tt.stdoutTTY, got, tt.wantInteractive)
			}
		})
	}
}

// liveServer answers listings describing one session ($1 "main") holding one
// window (@3), so an Action's identity survives the revalidation every dispatch
// now performs. Building fixtures this way rather than stubbing the client is
// the point: the argv a test asserts is the argv that came out the far side of
// a real identity check.
func liveServer() *exec.FakeRunner {
	const sep = "\x1f"
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return strings.Join([]string{"123", "456", "$1", "main", "1", "1", "1700000000", "/w"}, sep), nil
		case "list-windows":
			return strings.Join([]string{"123", "456", "@3", "$1", "main", "0", "editor", "1", "1"}, sep), nil
		}
		return "", nil
	}}
}

func TestDispatchAction(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name            string
		action          func(*tmux.Client) tui.Action
		wantCallCount   int
		wantCmd         string
		wantArgs        []string
		wantInteractive bool
	}{
		{
			name:   "ActionNone makes no calls",
			action: func(*tmux.Client) tui.Action { return tui.Action{} },
		},
		{
			name: "ActionAttachSession inside tmux switches by native id",
			action: func(c *tmux.Client) tui.Action {
				return tui.Action{
					Kind:    tui.ActionAttachSession,
					Session: c.SessionIdentity(tmux.Session{ServerPID: "123", ServerStart: "456", ID: "$1", Name: "main"}),
				}
			},
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-t", "$1"},
		},
		{
			name: "ActionAttachWindow selects the window then switches to its parent",
			action: func(c *tmux.Client) tui.Action {
				return tui.Action{
					Kind:   tui.ActionAttachWindow,
					Window: c.WindowIdentity(tmux.Window{ServerPID: "123", ServerStart: "456", ID: "@3", SessionID: "$1", Name: "editor"}),
				}
			},
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-t", "$1"},
		},
		{
			name:          "ActionLast inside tmux issues switch-client -l",
			action:        func(*tmux.Client) tui.Action { return tui.Action{Kind: tui.ActionLast} },
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-l"},
		},
		{
			name:            "ActionPick issues interactive sesh connect",
			action:          func(*tmux.Client) tui.Action { return tui.Action{Kind: tui.ActionPick, Pick: "dev"} },
			wantCallCount:   1,
			wantCmd:         "sesh",
			wantInteractive: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := liveServer()
			// Stub the sesh PATH check so the ActionPick case exercises the
			// dispatch, not a real sesh binary (CI runners have none).
			client := tmux.New(fake,
				tmux.WithInsideTmux(func() bool { return true }),
				tmux.WithLookPath(func(string) (string, error) { return "sesh", nil }),
			)

			if err := dispatchAction(ctx, client, tc.action(client)); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantCallCount == 0 {
				if len(fake.Calls) != 0 {
					t.Errorf("expected no calls, got %d: %v", len(fake.Calls), fake.Calls)
				}
				return
			}

			if len(fake.Calls) == 0 {
				t.Fatal("expected calls but got none")
			}
			last := fake.Last()
			if last.Name != tc.wantCmd {
				t.Errorf("cmd = %q, want %q", last.Name, tc.wantCmd)
			}
			if tc.wantInteractive && !last.Interactive {
				t.Errorf("expected interactive call, got non-interactive")
			}
			for i, want := range tc.wantArgs {
				if i >= len(last.Args) {
					t.Errorf("args[%d] missing, want %q", i, want)
					continue
				}
				if last.Args[i] != want {
					t.Errorf("args[%d] = %q, want %q", i, last.Args[i], want)
				}
			}
		})
	}
}

func TestDispatchAction_ErrorPropagates(t *testing.T) {
	ctx := context.Background()
	fake := liveServer()
	fake.InteractiveErr = &exec.CommandError{Name: "tmux", Err: &mockExitErr{}}
	client := tmux.New(fake, tmux.WithInsideTmux(func() bool { return false }))

	act := tui.Action{
		Kind:    tui.ActionAttachSession,
		Session: client.SessionIdentity(tmux.Session{ServerPID: "123", ServerStart: "456", ID: "$1", Name: "main"}),
	}
	if err := dispatchAction(ctx, client, act); err == nil {
		t.Fatal("expected error from failed attach, got nil")
	}
}

// TestDispatchAction_StaleIdentityIsRefused closes the gap the TUI's deferred
// action opens: Bubble Tea's teardown sits between choosing a session and
// attaching to it, and the server can restart in that window. The captured id
// would still resolve — to a different session.
func TestDispatchAction_StaleIdentityIsRefused(t *testing.T) {
	fake := liveServer()
	client := tmux.New(fake, tmux.WithInsideTmux(func() bool { return true }))
	act := tui.Action{
		Kind: tui.ActionAttachSession,
		// Same $1, a server generation that no longer exists.
		Session: client.SessionIdentity(tmux.Session{ServerPID: "999", ServerStart: "999", ID: "$1", Name: "main"}),
	}
	if err := dispatchAction(context.Background(), client, act); err == nil {
		t.Fatal("attached to a session id minted by a dead server generation")
	}
	for _, call := range fake.Calls {
		if len(call.Args) > 0 && call.Args[0] != "list-sessions" {
			t.Fatalf("ran %v on a stale identity, want only the listing", call.Args)
		}
	}
}

// mockExitErr satisfies the error interface for testing error propagation.
type mockExitErr struct{}

func (e *mockExitErr) Error() string { return "exit status 1" }
