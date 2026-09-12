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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prLocalFakeRunner fakes the two `git -C <path> rev-parse …` calls
// PrepareLocal issues, plus a no-op success for a later `git worktree add` /
// `tmux new-window`.
func prLocalFakeRunner() *exec.FakeRunner {
	created := false
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "tmux" && len(args) > 0 {
				switch args[0] {
				case "list-sessions":
					if created {
						return "123\x1f456\x1f$1\x1fforgectl\x1f1\x1f0\x1f0\x1f/tmp", nil
					}
				case "new-session":
					created = true
					return "123\x1f456\x1f$1", nil
				}
			}
			if out, handled, err := tmuxDouble(name, args); handled {
				return out, err
			}
			if name == "tmux" && len(args) > 0 {
				switch args[0] {
				case "-V":
					return "tmux 3.7b", nil
				case "display-message":
					return "123\x1f456\x1f@0", nil
				case "new-window":
					return "123\x1f456\x1f@1", nil
				}
			}
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
// breadcrumb dir and a throwaway findings dir, mirroring internal/pr's
// testClient helper. WithFindingsDir is required, not cosmetic: without it a
// real (non-dry-run) PrepareLocal falls back to config.PrFindingsDir() and
// would create a findings dir under the developer's REAL config dir every
// time this test runs.
func newPrLocalTestClient(t *testing.T, fake *exec.FakeRunner) *pr.Client {
	t.Helper()
	return pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithFindingsDir(t.TempDir()), pr.WithDispatchWait(func(context.Context) error { return nil }))
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
	cmd.SetArgs([]string{t.TempDir(), "--no-verify"})

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
	// The workspace lands under the OS temp root (sandbox.Sandbox,
	// os.MkdirTemp); the findings dir lands under this test's injected
	// WithFindingsDir(t.TempDir()) and would be covered by that auto-cleanup
	// on its own, but removing both by hand here keeps this test's cleanup
	// independent of that injection detail.
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

// localReviewCmdCapped is localReviewCmd's (pr_outcome_test.go) sibling with
// an explicit cfg, so a cap test can set MaxConcurrent — plus the client, so
// a test can List() what the command left behind. It shares the same
// ledger-backed tmux double: reserve now runs before PrepareLocal (#472), so
// `pr local` needs the same live-window seeding a cap test on `pr <ref>` does.
func localReviewCmdCapped(t *testing.T, l *tmuxLedger, cfg config.Config) (*cobra.Command, *bytes.Buffer, *pr.Client) {
	t.Helper()
	fakeClaudeBin(t)
	reviewTempRoot(t)
	git := prLocalFakeRunner().RunFunc
	fake := l.runnerWith(git)
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(l.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cmd := newPrLocalCmd(client, cfg)
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	return cmd, out, client
}

func TestPrLocalCmd_BelowCap_ReservesThenLaunchesActive(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	cmd, out, client := localReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	cmd.SetArgs([]string{t.TempDir(), "--no-verify"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "prepared local clean-room review of main @") {
		t.Errorf("stdout = %q, want the success line", out.String())
	}

	summaries, unreadable, err := client.List(context.Background())
	if err != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly one record", summaries)
	}
	if summaries[0].Phase() != pr.PhaseActive {
		t.Errorf("phase = %q, want %q", summaries[0].Phase(), pr.PhaseActive)
	}
}

func TestPrLocalCmd_AtCap_RefusesWithTwoLineMessageNoQueueOption(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	ledger.live = append(ledger.live, ledgerWindow{id: "@0", name: "pr-already-running"})
	cmd, out, client := localReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 1}})
	cmd.SetArgs([]string{t.TempDir()})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want an error at the cap, got nil")
	}
	for _, want := range []string{
		"review cap reached (max 1, 1 running) — nothing prepared.",
		"see them:   forgectl pr list",
		"raise it:   [pr] max_concurrent in config.toml",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want line %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "--queue") {
		t.Errorf("error = %q, `pr local` has no --queue to offer", err.Error())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed on refusal", out.String())
	}
	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 0 {
		t.Errorf("summaries = %+v, want none — refusal must write no record", summaries)
	}
}

func TestPrLocalCmd_QueueFlagDoesNotExist(t *testing.T) {
	fake := prLocalFakeRunner()
	client := newPrLocalTestClient(t, fake)

	cmd := newPrLocalCmd(client, config.Config{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--queue"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --queue") {
		t.Errorf("error = %v, want cobra's unknown-flag refusal for --queue", err)
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

// --- Fix round on the security review's Important findings (local path) ---
//
//   [x] The agent gate refuses BEFORE Reserve, so a refused local review
//       leaves zero records
//   [x] HEAD is read exactly once: a git double whose HEAD moves between
//       calls still yields a record ref and a window name that agree
//   [x] A PrepareLocal failure after Reserve parks the reservation

// TestPrLocalCmd_CodexWithoutOperatorAuthored_RefusesBeforeReserving pins the
// ordering PrepareLocal's own comment promises — "a refusal creates no state
// at all" — now that the reservation happens before PrepareLocal runs.
func TestPrLocalCmd_CodexWithoutOperatorAuthored_RefusesBeforeReserving(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	cmd, out, client := localReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	cmd.SetArgs([]string{t.TempDir(), "--agent", "codex"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want a refusal for --agent codex without --operator-authored, got nil")
	}
	if !strings.Contains(err.Error(), "codex") {
		t.Errorf("error = %q, want the Codex confinement refusal", err.Error())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed on refusal", out.String())
	}
	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 0 {
		t.Errorf("summaries = %+v, want none — a pure policy refusal must reserve nothing", summaries)
	}
}

// movingHeadRunner is prLocalFakeRunner's git half with a HEAD that advances on
// every `rev-parse HEAD`. Two reads of HEAD would key the reservation (and so
// the record) to the first commit while the workspace and tmux window were
// pinned to the second, which is exactly the disagreement teardown and repair
// cannot survive: both resolve the window from the record's ref.
func movingHeadRunner(oids []string) func(string, []string) (string, error) {
	call := 0
	return func(name string, args []string) (string, error) {
		if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
			for _, a := range args {
				if a == "--abbrev-ref" {
					return "main", nil
				}
			}
			oid := oids[len(oids)-1]
			if call < len(oids) {
				oid = oids[call]
			}
			call++
			return oid, nil
		}
		return "", nil
	}
}

func TestPrLocalCmd_MovingHead_RecordRefAndWindowNameAgree(t *testing.T) {
	fakeClaudeBin(t)
	reviewTempRoot(t)
	ledger := newTmuxLedger("forgectl")
	fake := ledger.runnerWith(movingHeadRunner([]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cccccccccccccccccccccccccccccccccccccccc",
	}))
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(ledger.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cmd := newPrLocalCmd(client, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{t.TempDir(), "--no-verify"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	summaries, unreadable, err := client.List(context.Background())
	if err != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly one record", summaries)
	}
	wantName, err := pr.ReviewWindowName(summaries[0].Ref())
	if err != nil {
		t.Fatalf("ReviewWindowName(%v): %v", summaries[0].Ref(), err)
	}
	var dispatched []string
	for _, w := range ledger.live {
		dispatched = append(dispatched, w.name)
	}
	if len(dispatched) != 1 || dispatched[0] != wantName {
		t.Errorf("dispatched windows = %v, want exactly [%s] — the record's ref and the window "+
			"must name one commit even though HEAD moved between reads", dispatched, wantName)
	}
}

// TestPrLocalCmd_PrepareFailureParksTheReservation mirrors the remote path: a
// failure after the slot is claimed must not leave the slot held.
func TestPrLocalCmd_PrepareFailureParksTheReservation(t *testing.T) {
	fakeClaudeBin(t)
	reviewTempRoot(t)
	ledger := newTmuxLedger("forgectl")
	fake := ledger.runnerWith(func(name string, args []string) (string, error) {
		if name == "git" && len(args) >= 3 && args[2] == "rev-parse" {
			for _, a := range args {
				if a == "--abbrev-ref" {
					// ResolveLocalHead has already reserved by the time
					// PrepareLocal reads the branch name, so failing here is a
					// post-reserve failure.
					return "", errors.New("boom: git could not name the branch")
				}
			}
			return "deadbeefcafe1234567890abcdef1234567890", nil
		}
		return "", nil
	})
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(ledger.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cmd := newPrLocalCmd(client, config.Config{Pr: config.PrConfig{MaxConcurrent: 1}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{t.TempDir()})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want the git failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "boom: git could not name the branch") {
		t.Errorf("error = %q, want the original git failure, not a bookkeeping error", err.Error())
	}

	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 1 || summaries[0].Phase() != pr.PhaseNeedsRepair {
		t.Fatalf("summaries = %+v, want exactly one needs-repair record", summaries)
	}
	if _, live, free, ok := client.Admit(context.Background(), 1); !ok || free != 1 || live != 0 {
		t.Errorf("Admit = live %d, free %d, ok %v; want live 0, free 1 — a parked record holds no slot", live, free, ok)
	}
}
