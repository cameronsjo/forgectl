package cli

// Pins forgectl#190: the "close" alias on `pr teardown` collided with the
// unrelated Bash(gh pr close:*) allowlist pattern closely enough to read as
// the same verb, so it was removed. teardown itself must still resolve and
// dispatch correctly — this is a removal, not a rename.

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// TestPrTeardown_StillWired is the control: dispatching "pr teardown" through
// the real command tree (SetArgs + Execute, the same path a real invocation
// takes) must still resolve to teardown and enforce its 1-arg contract after
// the alias drop. Exercising `teardown.Execute()` directly is a trap: cobra's
// ExecuteC redirects any command with a parent to `c.Root().ExecuteC()`, so
// `teardown.SetArgs(...)` is silently ignored and the run falls back to
// prCmd's own unset args (which happen to carry the identical "accepts 1
// arg(s)" message from prCmd's own ExactArgs(1), not teardown's) — a
// same-shaped failure for the wrong reason. Driving it through prCmd instead
// avoids the redirect: prCmd has no parent in this test, so its own
// Execute() resolves "teardown" via cobra's real Find and validates
// teardown's Args, not prCmd's.
func TestPrTeardown_StillWired(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	prCmd := newPrCmd(deps)

	prCmd.SetArgs([]string{"teardown"})
	prCmd.SetOut(new(strings.Builder))
	prCmd.SetErr(new(strings.Builder))
	err := prCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("pr teardown with 0 args: err = %v, want an \"accepts 1 arg(s)\" error", err)
	}
}

// TestPrTeardown_CloseAliasRemoved is the regression: findChild is the same
// dispatcher cobra's own Execute/Find reads (both walk Command.Aliases), so
// asserting through it is equivalent to asserting through a live
// invocation, not a second, privileged alias table.
func TestPrTeardown_CloseAliasRemoved(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	prCmd := newPrCmd(deps)

	if got := findChild(prCmd, "close"); got != nil {
		t.Errorf("findChild(prCmd, \"close\") = %v, want nil (alias removed)", got.Name())
	}
}

// TestPrCmd_NoCloseAlias sweeps every subcommand under pr for a lingering
// "close" alias anywhere in the tree, not just on teardown.
func TestPrCmd_NoCloseAlias(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	prCmd := newPrCmd(deps)

	for _, sub := range prCmd.Commands() {
		for _, a := range sub.Aliases {
			if a == "close" {
				t.Errorf("command %q carries alias \"close\"; forgectl#190 removed it from teardown and it must not resurface", sub.Name())
			}
		}
	}
}

// TestPrCmd_HelpOmitsCloseAlias pins the advertised help text: the Long
// description no longer mentions "alias: close" for teardown.
func TestPrCmd_HelpOmitsCloseAlias(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	prCmd := newPrCmd(deps)

	if strings.Contains(prCmd.Long, "alias: close") {
		t.Errorf("pr Long help still advertises \"alias: close\": %q", prCmd.Long)
	}
}
