package cli

// The CLI half of forgectl#232: which routes DECLARE authorship, and how.
//
// Provenance is only ever created by an operator saying so, so the CLI is the
// entire trusted surface. Every other route declares third-party by
// construction — and the tests below assert that from the outside, through the
// cobra commands, rather than by inspecting the opts struct.

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
)

// findCLICallVerb finds the first call to name whose first arg is verb — for
// separating a tmux MUTATION (new-window) from the read-only capability probes
// that legitimately precede a refusal.
func findCLICallVerb(calls []exec.Call, name, verb string) (exec.Call, bool) {
	for _, c := range calls {
		if c.Name == name && len(c.Args) > 0 && c.Args[0] == verb {
			return c, true
		}
	}
	return exec.Call{}, false
}

// detachedPrLocalFakeRunner is prLocalFakeRunner on a DETACHED HEAD — the state
// `gh pr checkout` leaves behind, and the one forgectl#232 was filed about.
func detachedPrLocalFakeRunner() *exec.FakeRunner {
	fake := prLocalFakeRunner()
	inner := fake.RunFunc
	fake.RunFunc = func(name string, args []string) (string, error) {
		if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
			for _, a := range args {
				if a == "--abbrev-ref" {
					return "HEAD", nil // detached
				}
			}
		}
		return inner(name, args)
	}
	return fake
}

// TestPrLocalCmd_CodexRefusedWithoutFlag is the user-visible statement of the
// fix: the ordinary `gh pr checkout` review can no longer reach the unconfined
// reviewer just by being on a local path.
func TestPrLocalCmd_CodexRefusedWithoutFlag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner func() *exec.FakeRunner
	}{
		{"attached", prLocalFakeRunner},
		{"detached", detachedPrLocalFakeRunner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.runner()
			cmd := newPrLocalCmd(newPrLocalTestClient(t, fake), config.Config{})
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{"--agent", "codex"})

			err := cmd.ExecuteContext(context.Background())
			if err == nil {
				t.Fatal("`pr local --agent codex` must refuse without --operator-authored")
			}
			if !strings.Contains(err.Error(), "--operator-authored") {
				t.Errorf("refusal should name the assertion for an unknown-provenance tree: %v", err)
			}
			// Assert on the error only, never on stdout: a standalone cobra
			// command resolves cmd.Root() to itself, so the real root's
			// SilenceUsage never applies and usage text lands here. That is a
			// harness artifact, not production behavior.
			if strings.Contains(out.String(), "prepared local clean-room review") {
				t.Errorf("a refused run printed success output: %q", out.String())
			}
			// tmux MUTATIONS specifically. `tmux -V` and the session lookup are
			// #242's read-only dispatch-capability preflight, which the CLI runs
			// before PrepareLocal and this change deliberately leaves in place.
			// What must not happen is a window being created.
			for _, verb := range []string{"new-window", "new-session"} {
				if _, ok := findCLICallVerb(fake.Calls, "tmux", verb); ok {
					t.Errorf("a refused run issued `tmux %s`", verb)
				}
			}
		})
	}
}

// TestPrLocalCmd_OperatorAuthoredFlagPermitsCodex is the other direction, on a
// detached HEAD specifically: the assertion is what opens the path, and
// detachedness neither grants nor withholds it.
// It asserts the success output, so it cannot pass under a refusal: a refused
// run returns an error and never reaches PrepareLocal's log line.
func TestPrLocalCmd_OperatorAuthoredFlagPermitsCodex(t *testing.T) {
	fakeCodexBin(t)
	fake := detachedPrLocalFakeRunner()
	cmd := newPrLocalCmd(newPrLocalTestClient(t, fake), config.Config{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--agent", "codex", "--operator-authored", "--no-verify"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("an asserted local Codex review must run: %v", err)
	}
	if !strings.Contains(out.String(), "prepared local clean-room review") {
		t.Errorf("expected success output, got:\n%s", out.String())
	}
}

// TestPrLocalCmd_OperatorAuthoredHelpStatesTheStakes keeps the flag from reading
// like a routine toggle. It grants an unconfined reviewer arbitrary shell with
// host-wide read, so its help has to say what it permits and what it asserts —
// an operator reaching for it after a refusal must be able to tell that lying
// here has consequences.
func TestPrLocalCmd_OperatorAuthoredHelpStatesTheStakes(t *testing.T) {
	cmd := newPrLocalCmd(newPrLocalTestClient(t, prLocalFakeRunner()), config.Config{})
	flag := cmd.Flags().Lookup("operator-authored")
	if flag == nil {
		t.Fatal("`pr local` has no --operator-authored flag")
	}
	usage := strings.ToLower(flag.Usage)
	for _, want := range []string{"you wrote", "codex"} {
		if !strings.Contains(usage, want) {
			t.Errorf("flag help does not mention %q: %q", want, flag.Usage)
		}
	}
	// It must not read as a generic trust/skip switch.
	for _, forbidden := range []string{"skip", "bypass", "disable"} {
		if strings.Contains(usage, forbidden) {
			t.Errorf("flag help reads as a safety bypass (%q): %q", forbidden, flag.Usage)
		}
	}
}

// TestPrLocalCmd_ClaudeUnaffectedByFlag is the control: the default agent is
// confined by its own allowlist, so the assertion changes nothing for it.
func TestPrLocalCmd_ClaudeUnaffectedByFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--dry-run"},
		{"--dry-run", "--operator-authored"},
	} {
		cmd := newPrLocalCmd(newPrLocalTestClient(t, detachedPrLocalFakeRunner()), config.Config{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("args %v: the default agent must be unaffected: %v", args, err)
		}
		if !strings.Contains(out.String(), "plan: local review") {
			t.Errorf("args %v: expected a plan, got:\n%s", args, out.String())
		}
	}
}

// TestPrRefCmd_CodexAlwaysRefused proves the remote route declares third-party
// by construction — there is no flag to reach for, and the refusal must not
// advertise one.
func TestPrRefCmd_CodexAlwaysRefused(t *testing.T) {
	fake := prLocalFakeRunner()
	cmd := newPrCmdForClient(config.Config{}, newPrLocalTestClient(t, fake),
		netpkg.New(fake), filepath.Join(t.TempDir(), "reviewed.json"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"cameronsjo/forgectl#42", "--agent", "codex"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("`pr <ref> --agent codex` must be refused")
	}
	if strings.Contains(err.Error(), "--operator-authored") {
		t.Errorf("the remote refusal advertises a flag that cannot honestly apply: %v", err)
	}
	if _, ok := findCLICall(fake.Calls, "gh"); ok {
		t.Error("refusal must precede the gh round-trip")
	}
}
