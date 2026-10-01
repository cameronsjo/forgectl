package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// TestTmux_AFailingHubVerbIsRenderedOnce drives `forgectl tmux` through
// execDispatch and fang, with a hub that hands back a verb that fails. The
// verb must run after the tmux command's own fang frame has returned: run
// from inside tmux's RunE, the inner fang renders the error and the outer
// renders it again (forgectl#1000).
//
// Mutation that turns it red: in newTmuxCmdWith's RunE, hand the chosen
// argv to runHubVerb directly instead of deferHubVerb.
func TestTmux_AFailingHubVerbIsRenderedOnce(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	hub := func(context.Context, *tmux.Client, tui.RunOptions) (tui.Action, error) {
		return tui.Action{Kind: tui.ActionRunVerb, Argv: []string{"boom"}}, nil
	}
	root := &cobra.Command{Use: "forgectl", SilenceUsage: true}
	root.AddCommand(newTmuxCmdWith(deps, tmux.New(deps.Runner), hub))
	ran := 0
	root.AddCommand(&cobra.Command{Use: "boom", RunE: func(*cobra.Command, []string) error {
		ran++
		return WithExitCode(errors.New("boom failed"), 3)
	}})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := execDispatch(context.Background(), deps, root, []string{"tmux"}, theme.Theme{})
	if ran != 1 {
		t.Fatalf("the chosen verb ran %d times, want 1", ran)
	}
	if got := ExitCode(err); got != 3 {
		t.Errorf("exit code = %d (err %v), want the verb's own 3", got, err)
	}
	if n := strings.Count(strings.ToLower(stderr.String()), "boom failed"); n != 1 {
		t.Errorf("the verb's error was rendered %d times, want once:\n%s", n, stderr.String())
	}
}
