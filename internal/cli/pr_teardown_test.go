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

// TestPrTeardown_StillWired is the control: teardown must still resolve by
// its real name and still enforce its 1-arg contract after the alias drop.
func TestPrTeardown_StillWired(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	prCmd := newPrCmd(deps)

	teardown := findChild(prCmd, "teardown")
	if teardown == nil {
		t.Fatal("findChild(prCmd, \"teardown\") = nil, want the teardown command")
	}

	teardown.SetArgs(nil)
	teardown.SetOut(new(strings.Builder))
	teardown.SetErr(new(strings.Builder))
	err := teardown.Execute()
	if err == nil || !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("teardown with 0 args: err = %v, want an \"accepts 1 arg(s)\" error", err)
	}
}

// TestPrTeardown_CloseAliasRemoved is the regression: findChild is the real
// dispatcher (execute.go) and resolves by alias, so a cobra-only assertion
// on Aliases would miss a lingering argv-level resolution.
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
