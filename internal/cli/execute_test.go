package cli

import (
	"context"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

func TestDispatchAction(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name            string
		action          tui.Action
		wantCallCount   int
		wantCmd         string
		wantArgs        []string
		wantInteractive bool
	}{
		{
			name:   "ActionNone makes no calls",
			action: tui.Action{},
		},
		{
			name:          "ActionAttach inside tmux issues switch-client",
			action:        tui.Action{Kind: tui.ActionAttach, Target: "main"},
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-t", "main"},
		},
		{
			name:          "ActionLast inside tmux issues switch-client -l",
			action:        tui.Action{Kind: tui.ActionLast},
			wantCallCount: 1,
			wantCmd:       "tmux",
			wantArgs:      []string{"switch-client", "-l"},
		},
		{
			name:            "ActionPick issues interactive sesh connect",
			action:          tui.Action{Kind: tui.ActionPick, Target: "dev"},
			wantCallCount:   1,
			wantCmd:         "sesh",
			wantInteractive: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			client := tmux.New(fake, tmux.WithInsideTmux(func() bool { return true }))

			if err := dispatchAction(ctx, client, tc.action); err != nil {
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
	fake := &exec.FakeRunner{
		InteractiveErr: &exec.CommandError{Name: "sesh", Err: &mockExitErr{}},
	}
	client := tmux.New(fake, tmux.WithInsideTmux(func() bool { return false }))

	act := tui.Action{Kind: tui.ActionAttach, Target: "main"}
	err := dispatchAction(ctx, client, act)
	if err == nil {
		t.Fatal("expected error from failed attach, got nil")
	}
}

// mockExitErr satisfies the error interface for testing error propagation.
type mockExitErr struct{}

func (e *mockExitErr) Error() string { return "exit status 1" }
