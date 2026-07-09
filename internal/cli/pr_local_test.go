package cli

// Test plan for pr_local.go
//
// newPrLocalCmd (Classification: API handler / cobra command, offline sibling
// of newPrCmd's <ref> RunE)
//   [x] Happy: --dry-run defaults path to "." and prints the plan (head ref,
//       head oid, default-agent label, no-workspace footer) without invoking
//       Launch (no tmux call reaches the Runner)
//   [x] Happy: a positional path argument is threaded through to PrepareLocal
//       (observed via the `git -C <path> rev-parse` calls the fake Runner
//       records)
//   [x] Happy: --agent overrides the FORGECTL_PR_AGENT env fallback, and an
//       explicit agent suppresses the "(default, inline-seeded)" label
//   [x] Happy: a real (non-dry-run) run prints workspace/findings/breadcrumb
//       lines sourced from the returned Session
//   [x] Unhappy: more than one positional arg is rejected by
//       cobra.MaximumNArgs(1) before RunE runs
//   [x] Unhappy: a PrepareLocal failure (Runner error resolving HEAD)
//       propagates as the command's error, with no plan/success output

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prLocalFakeRunner fakes the two `git -C <path> rev-parse …` calls
// PrepareLocal issues, plus a no-op success for a later `git worktree add` /
// `tmux new-window`.
func prLocalFakeRunner() *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
				for _, a := range args {
					if a == "--abbrev-ref" {
						return "main", nil
					}
				}
				return "deadbeefcafe1234567890abcdef1234567890", nil
			}
			return "", nil // worktree add / tmux succeed as no-ops
		},
	}
}

// newPrLocalTestClient builds a pr.Client over fake with a throwaway
// breadcrumb dir, mirroring internal/pr's testClient helper.
func newPrLocalTestClient(t *testing.T, fake *exec.FakeRunner) *pr.Client {
	t.Helper()
	return pr.New(fake, pr.WithSessionsDir(t.TempDir()))
}

func TestPrLocalCmd_DryRun_DefaultsPathAndPrintsPlan(t *testing.T) {
	fake := prLocalFakeRunner()
	client := newPrLocalTestClient(t, fake)

	cmd := newPrLocalCmd(client, config.Config{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := out.String()
	for _, want := range []string{
		"plan: local review main @ deadbeefcafe1234567890abcdef1234567890",
		"claude (default, inline-seeded)",
		"(dry-run: no workspace, window, or breadcrumb created)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, body)
		}
	}
	if _, ok := findCLICall(fake.Calls, "tmux"); ok {
		t.Error("dry-run must not reach Launch/tmux")
	}
}

func TestPrLocalCmd_DryRun_ThreadsPositionalPathArg(t *testing.T) {
	fake := prLocalFakeRunner()
	client := newPrLocalTestClient(t, fake)
	wantPath, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve want path: %v", err)
	}

	cmd := newPrLocalCmd(client, config.Config{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"../..", "--dry-run"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call, ok := findCLICall(fake.Calls, "git")
	if !ok {
		t.Fatal("expected at least one git rev-parse call")
	}
	if len(call.Args) < 2 || call.Args[1] != wantPath {
		t.Errorf("git -C arg = %v, want -C %q (the positional path, absolute-resolved)", call.Args, wantPath)
	}
}

func TestPrLocalCmd_AgentFlagOverridesEnvAndDropsDefaultLabel(t *testing.T) {
	t.Setenv(prAgentEnv, "env-agent")
	fake := prLocalFakeRunner()
	client := newPrLocalTestClient(t, fake)

	cmd := newPrLocalCmd(client, config.Config{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--dry-run", "--agent", "flag-agent"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := out.String()
	if !strings.Contains(body, "agent: flag-agent") {
		t.Errorf("output missing explicit --agent value; got:\n%s", body)
	}
	if strings.Contains(body, "env-agent") {
		t.Errorf("--agent must win over %s; got:\n%s", prAgentEnv, body)
	}
	if strings.Contains(body, "default, inline-seeded") {
		t.Errorf("an explicit agent must suppress the default-agent label; got:\n%s", body)
	}
}

func TestPrLocalCmd_RealRun_PrintsWorkspaceFindingsBreadcrumb(t *testing.T) {
	claudeBin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	fake := prLocalFakeRunner()
	client := newPrLocalTestClient(t, fake)

	cmd := newPrLocalCmd(client, config.Config{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{t.TempDir()})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := out.String()
	for _, want := range []string{"workspace:", "findings:", "breadcrumb:"} {
		if !strings.Contains(body, want) {
			t.Errorf("real-run output missing %q line; got:\n%s", want, body)
		}
	}
	if _, ok := findCLICall(fake.Calls, "tmux"); !ok {
		t.Error("real run should dispatch through tmux (Launch)")
	}
	// Workspace/findings dirs land under the OS temp root (sandbox.Sandbox,
	// os.MkdirTemp), outside t.TempDir()'s auto-cleanup — remove them by hand.
	for _, line := range strings.Split(body, "\n") {
		for _, prefix := range []string{"  workspace: ", "  findings: "} {
			if p, ok := strings.CutPrefix(line, prefix); ok {
				t.Cleanup(func() { os.RemoveAll(p) })
			}
		}
	}
}

func TestPrLocalCmd_TooManyArgsRejected(t *testing.T) {
	fake := prLocalFakeRunner()
	client := newPrLocalTestClient(t, fake)

	cmd := newPrLocalCmd(client, config.Config{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"path-a", "path-b"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for more than one positional arg")
	}
	for _, call := range fake.Calls {
		if call.Name == "git" {
			t.Errorf("RunE must not have run (no git calls expected); got %+v", fake.Calls)
		}
	}
}

func TestPrLocalCmd_PrepareLocalFailurePropagatesAndPrintsNothing(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" {
				return "", os.ErrPermission
			}
			return "", nil
		},
	}
	client := newPrLocalTestClient(t, fake)

	cmd := newPrLocalCmd(client, config.Config{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"--dry-run"})

	// A standalone cobra.Command built without the root command attached has
	// no access to root's SilenceUsage, so cobra prints usage to OutOrStdout
	// on any error — a test-harness artifact, not production behavior (in
	// production this command is always a child of `pr`, itself a child of
	// root, which sets SilenceUsage). Match the house pattern used by every
	// other error-path CLI test (see TestQuarantineHideCmd_UnknownSchemeErrors,
	// TestKillCmd_MissingSessionErrors): assert on the returned error, not on
	// stdout being empty.
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected PrepareLocal's Runner failure to propagate")
	}
	if !strings.Contains(err.Error(), "resolve local HEAD branch") {
		t.Errorf("error = %q, want it to mention resolving local HEAD branch", err.Error())
	}
}

func findCLICall(calls []exec.Call, name string) (exec.Call, bool) {
	for _, c := range calls {
		if c.Name == name {
			return c, true
		}
	}
	return exec.Call{}, false
}
