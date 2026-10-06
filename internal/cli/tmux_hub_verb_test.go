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
	stubInteractiveTTY(t, true)
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

// deferringRoot is a root whose `pick` command defers `ok` through
// deferHubVerb, the way `forgectl tmux` and `status --tui` hand on a verb
// chosen in their TUI. ran counts the runs of `ok`.
func deferringRoot(ran *int) *cobra.Command {
	root := &cobra.Command{Use: "forgectl", SilenceUsage: true}
	root.AddCommand(&cobra.Command{Use: "pick", RunE: func(cmd *cobra.Command, _ []string) error {
		return deferHubVerb(cmd, theme.Theme{}, []string{"ok"})
	}})
	root.AddCommand(&cobra.Command{Use: "ok", RunE: func(*cobra.Command, []string) error {
		*ran++
		return nil
	}})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root
}

// TestExecDispatch_ASecondDispatchOfTheSameCommandStillDefers: cobra gives a
// subcommand the root's context only while its own is nil, so a second run of
// the same subcommand in one process sees the first dispatch's context. The
// slot must still be the second dispatch's, or the deferred verb is dropped
// silently (forgectl#1003 item 1). No production path dispatches the same
// subcommand twice in one process today; this holds the slot to the dispatch
// before one does.
//
// Mutation that turns it red: have deferHubVerb read the slot from
// cmd.Context() instead of the root's.
func TestExecDispatch_ASecondDispatchOfTheSameCommandStillDefers(t *testing.T) {
	ran := 0
	root := deferringRoot(&ran)
	for i := 1; i <= 2; i++ {
		if err := execDispatch(context.Background(), module.Deps{}, root, []string{"pick"}, theme.Theme{}); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		if ran != i {
			t.Fatalf("after dispatch %d the deferred verb ran %d times, want %d", i, ran, i)
		}
	}
}

// TestExecDispatch_ACancelledDispatchNeverRunsTheDeferredVerb: the command
// defers a verb and its dispatch's context ends before fang returns. The verb
// must not start (forgectl#1003 item 2).
//
// Mutation that turns it red: drop the ctx.Err() check in execDispatch.
func TestExecDispatch_ACancelledDispatchNeverRunsTheDeferredVerb(t *testing.T) {
	ran := 0
	root := deferringRoot(&ran)
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root.AddCommand(&cobra.Command{Use: "pick-then-cancel", RunE: func(cmd *cobra.Command, _ []string) error {
		err := deferHubVerb(cmd, theme.Theme{}, []string{"ok"})
		cancel()
		return err
	}})

	err := execDispatch(ctx, module.Deps{}, root, []string{"pick-then-cancel"}, theme.Theme{})
	if ran != 0 {
		t.Errorf("the deferred verb ran %d times after its dispatch was cancelled, want 0", ran)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(stderr.String(), "not running ok") {
		t.Errorf("stderr = %q, want a line saying the verb did not run", stderr.String())
	}
}

func stubInteractiveTTY(t *testing.T, interactive bool) {
	t.Helper()
	prev := isInteractiveTTY
	isInteractiveTTY = func() bool { return interactive }
	t.Cleanup(func() { isInteractiveTTY = prev })
}

// TestTmux_BareOffTerminalRefusesWithoutOpeningTheHub pins forgectl#1100:
// off a terminal `forgectl tmux` must not start the TUI (which writes
// alt-screen sequences into a pipe) and must name the plain forms.
//
// Mutation that turns it red: delete the isInteractiveTTY check in
// newTmuxCmdWith's RunE.
func TestTmux_BareOffTerminalRefusesWithoutOpeningTheHub(t *testing.T) {
	stubInteractiveTTY(t, false)
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	opened := false
	hub := func(context.Context, *tmux.Client, tui.RunOptions) (tui.Action, error) {
		opened = true
		return tui.Action{}, nil
	}
	cmd := newTmuxCmdWith(deps, tmux.New(deps.Runner), hub)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if opened {
		t.Fatal("the hub opened off a terminal")
	}
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") || !strings.Contains(err.Error(), "tmux ls") {
		t.Fatalf("err = %v, want a terminal-required message naming the plain forms", err)
	}
}
