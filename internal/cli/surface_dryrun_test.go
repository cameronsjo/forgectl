//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// stubHarness is an executable file a launch config can name as the codex
// binary, so a test resolves a harness without searching $PATH.
func stubHarness(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // G306: a harness stub must be executable
		t.Fatal(err)
	}
	return p
}

// dryRunDeps is a launch config that resolves to the stub harness under a
// worker-safe posture.
func dryRunDeps(t *testing.T) module.Deps {
	t.Helper()
	return module.Deps{
		Runner: exec.OSRunner{},
		Cfg: config.Config{Launch: config.LaunchConfig{Defaults: config.LaunchDefaults{
			Harness:         "codex",
			CodexBinaryPath: stubHarness(t),
			ApprovalPolicy:  "on-request",
			Sandbox:         "workspace-write",
		}}},
	}
}

// dryRunRepo makes a git repo with one commit and returns its real top. The
// global and system git config are cut off so a developer's setup cannot
// change what a test sees.
func dryRunRepo(t *testing.T) string {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	top, err := worker.RepoTop(context.Background(), exec.OSRunner{}, dir)
	if err != nil {
		t.Fatalf("RepoTop: %v", err)
	}
	return top
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.OSRunner{}.Run(context.Background(), "git", append([]string{"-C", dir}, args...)...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// repoSnapshot is everything a launch preview must leave alone in a repo.
func repoSnapshot(t *testing.T, top string) string {
	t.Helper()
	_, err := os.Lstat(filepath.Join(top, ".claude"))
	return gitIn(t, top, "worktree", "list", "--porcelain") + "|" +
		gitIn(t, top, "branch", "--list", "--all") + "|claude-exists=" + map[bool]string{true: "n", false: "y"}[errors.Is(err, os.ErrNotExist)]
}

// runPlan runs planWorkerLaunch under a command with captured streams.
func runPlan(t *testing.T, deps module.Deps, top string, led *worker.Ledger, opts surfaceLaunchOptions) (out, errOut string, err error) {
	t.Helper()
	cmd := &cobra.Command{}
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetContext(t.Context())
	self, serr := os.Executable()
	if serr != nil {
		t.Fatal(serr)
	}
	err = planWorkerLaunch(cmd, deps, opts, top, led, workerPlanInputs{self: self})
	return o.String(), e.String(), err
}

// A worker launch preview names what the launch would create and creates
// none of it: no worktree, no .claude, no branch, no ledger directory
// (forgectl#1088).
func TestPlanWorkerLaunchPreviewsAndCreatesNothing(t *testing.T) {
	top := dryRunRepo(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	led, err := worker.Open(top, "default")
	if err != nil {
		t.Fatal(err)
	}
	before := repoSnapshot(t, top)

	out, _, err := runPlan(t, dryRunDeps(t), top, led, surfaceLaunchOptions{Backend: "herdr", DisplayName: "w1", Worktree: "feat/w1"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	for _, want := range []string{
		"dry_run=true\n", "surface=herdr\n", "name=w1\n", "target=" + top + "\n", "harness=codex\n",
		"worktree=" + worker.WorktreePath(top, "w1") + "\n", "branch=feat/w1\n", "branch_from=new\n", "ledger_row=would-create\n", "brief=false\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if after := repoSnapshot(t, top); after != before {
		t.Errorf("the preview changed the repo:\nbefore %s\nafter  %s", before, after)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Errorf("the preview wrote to the state dir: %v", entries)
	}
	if rows, err := led.Rows(); err != nil || len(rows) != 0 {
		t.Errorf("the preview wrote a ledger row: %+v (err %v)", rows, err)
	}
}

func TestPlanWorkerLaunchJSONShape(t *testing.T) {
	top := dryRunRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	led, err := worker.Open(top, "default")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runPlan(t, dryRunDeps(t), top, led, surfaceLaunchOptions{Backend: "herdr", DisplayName: "w1", Worktree: "feat/w1", JSON: true})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if keys := mapKeys(got); !reflect.DeepEqual(keys, []string{"dry_run", "harness", "name", "surface", "target", "worker"}) {
		t.Errorf("top-level keys = %v", keys)
	}
	w, _ := got["worker"].(map[string]any)
	if keys := mapKeys(w); !reflect.DeepEqual(keys, []string{"branch", "branch_from", "brief", "ledger_row", "posture", "repo", "worktree"}) {
		t.Errorf("worker keys = %v", keys)
	}
	if got["dry_run"] != true || w["branch_from"] != "new" || w["ledger_row"] != "would-create" {
		t.Errorf("preview = %v", got)
	}
}

// The preview shows the worker posture the launch would use and where each
// value came from, so an operator can confirm auto took effect before
// launching: the built-in value, [launch.worker], or a stricter repo block.
func TestPlanWorkerLaunchShowsThePosture(t *testing.T) {
	top := dryRunRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	claude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // G306: a harness stub must be executable
		t.Fatal(err)
	}
	codex := stubHarness(t)
	for name, tc := range map[string]struct {
		lc   config.LaunchConfig
		want map[string]postureSetting
		text []string
	}{
		"claude built-in": {
			lc:   config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "claude", BinaryPath: claude}},
			want: map[string]postureSetting{"permission_mode": {"acceptEdits", "default"}},
			text: []string{"permission_mode=acceptEdits\n", "permission_mode_source=default\n"},
		},
		"claude auto from [launch.worker]": {
			lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "claude", BinaryPath: claude},
				Worker: config.LaunchWorker{PermissionMode: "auto"}},
			want: map[string]postureSetting{"permission_mode": {"auto", "launch.worker"}},
			text: []string{"permission_mode=auto\n", "permission_mode_source=launch.worker\n"},
		},
		"claude repo block stricter than auto": {
			lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "claude", BinaryPath: claude},
				Worker:   config.LaunchWorker{PermissionMode: "auto"},
				Projects: []config.LaunchProject{{Match: top, PermissionMode: "plan"}}},
			want: map[string]postureSetting{"permission_mode": {"plan", "repo-profile"}},
			text: []string{"permission_mode=plan\n", "permission_mode_source=repo-profile\n"},
		},
		"codex mixed sources": {
			lc: config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex", CodexBinaryPath: codex},
				Worker: config.LaunchWorker{Sandbox: "read-only"}},
			want: map[string]postureSetting{"sandbox": {"read-only", "launch.worker"}, "approval_policy": {"on-request", "default"}},
			text: []string{"sandbox=read-only\n", "sandbox_source=launch.worker\n", "approval_policy=on-request\n", "approval_policy_source=default\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			led, err := worker.Open(top, "default")
			if err != nil {
				t.Fatal(err)
			}
			deps := module.Deps{Runner: exec.OSRunner{}, Cfg: config.Config{Launch: tc.lc}}
			opts := surfaceLaunchOptions{Backend: "herdr", DisplayName: "w1", Worktree: "feat/w1", AllowPATH: true}
			opts.JSON = true
			out, _, err := runPlan(t, deps, top, led, opts)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			var got struct {
				Worker struct {
					Posture map[string]postureSetting `json:"posture"`
				} `json:"worker"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			if !reflect.DeepEqual(got.Worker.Posture, tc.want) {
				t.Errorf("posture = %+v, want %+v", got.Worker.Posture, tc.want)
			}
			opts.JSON = false
			out, _, err = runPlan(t, deps, top, led, opts)
			if err != nil {
				t.Fatalf("text preview: %v", err)
			}
			for _, line := range tc.text {
				if !strings.Contains(out, line) {
					t.Errorf("text preview lacks %q:\n%s", line, out)
				}
			}
		})
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// The preview refuses what the launch refuses, and still writes nothing: a
// name already in the ledger, a bad branch, a taken path.
func TestPlanWorkerLaunchRefusesWhatTheLaunchRefuses(t *testing.T) {
	top := dryRunRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	led, err := worker.Open(top, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Begin(worker.Row{Name: "taken", Branch: "feat/t", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worker.WorktreePath(top, "onpath"), 0o750); err != nil {
		t.Fatal(err)
	}
	deps := dryRunDeps(t)

	for _, tc := range []struct {
		desc string
		opts surfaceLaunchOptions
		want error
	}{
		{"name already in the ledger", surfaceLaunchOptions{Backend: "herdr", DisplayName: "taken", Worktree: "feat/x"}, worker.ErrNameTaken},
		{"bad branch", surfaceLaunchOptions{Backend: "herdr", DisplayName: "fresh", Worktree: "-b"}, worker.ErrInvalidBranch},
		{"worktree path in use", surfaceLaunchOptions{Backend: "herdr", DisplayName: "onpath", Worktree: "feat/y"}, nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			before := repoSnapshot(t, top)
			out, _, err := runPlan(t, deps, top, led, tc.opts)
			if err == nil {
				t.Fatalf("the preview accepted what the launch refuses:\n%s", out)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if out != "" {
				t.Errorf("a refused preview printed a plan:\n%s", out)
			}
			if after := repoSnapshot(t, top); after != before {
				t.Errorf("a refused preview changed the repo")
			}
		})
	}
}

// A harness found only on $PATH is refused by the launch unless
// --allow-path-binary is given; the preview refuses it too.
func TestPlanWorkerLaunchAppliesTheBinaryPolicy(t *testing.T) {
	top := dryRunRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	led, err := worker.Open(top, "default")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // G306: a harness stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	deps := dryRunDeps(t)
	deps.Cfg.Launch.Defaults.CodexBinaryPath = ""
	t.Setenv("FORGECTL_CODEX_BIN", "")

	opts := surfaceLaunchOptions{Backend: "herdr", DisplayName: "w1", Worktree: "feat/w1"}
	if _, _, err := runPlan(t, deps, top, led, opts); err == nil || !strings.Contains(err.Error(), "--allow-path-binary") {
		t.Fatalf("err = %v, want the PATH-provenance refusal", err)
	}
	opts.AllowPATH = true
	if _, _, err := runPlan(t, deps, top, led, opts); err != nil {
		t.Fatalf("with --allow-path-binary: %v", err)
	}
}

// A plain launch's preview needs the backend on PATH and calls nothing in it:
// a tmux that records its argv is never run.
func TestSurfaceLaunchDryRunNeverCallsTheBackend(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "tmux-ran")
	script := "#!/bin/sh\necho \"$@\" >> '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: a backend stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	run := func(args ...string) (string, error) {
		cmd := newSurfaceLaunchCmd(dryRunDeps(t))
		var o, e bytes.Buffer
		cmd.SetOut(&o)
		cmd.SetErr(&e)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return o.String(), err
	}

	out, err := run(project, "--surface", "tmux", "--name", "review", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{"dry_run=true\n", "surface=tmux\n", "name=review\n", "target=" + project + "\n", "harness=codex\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "worktree=") {
		t.Errorf("a plain launch preview names a worktree:\n%s", out)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run called tmux: %v", err)
	}

	out, err = run(project, "--surface", "tmux", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry run --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if keys := mapKeys(got); !reflect.DeepEqual(keys, []string{"dry_run", "harness", "name", "surface", "target"}) {
		t.Errorf("keys = %v (a plain launch has no worker key)", keys)
	}

	// --json prints the preview; without --dry-run there is none.
	_, err = run(project, "--surface", "tmux", "--json")
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("--json without --dry-run: err = %v (exit %d), want a usage error naming --dry-run", err, ExitCode(err))
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused call reached tmux: %v", err)
	}
}

// planClose and closeWorker decide the same way wherever the answer needs no
// herdr: every scenario below runs both and compares them.
// dirtyFacts trips every removal check at once.
func dirtyFacts() worker.WorktreeFacts {
	f := cleanFacts
	f.Stashes = 1
	f.Status = "?? untracked.txt\n"
	f.Branch = ""
	f.Ahead = 3
	return f
}

func TestPlanCloseAgreesWithCloseWorker(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1e9, 0)
	stashed := cleanFacts
	stashed.Stashes = 1
	type scenario struct {
		row  func(*testing.T) worker.Row
		fake fakeClose
		keep bool
	}
	scenarios := map[string]scenario{
		"clean worktree is removed and the row forgotten": {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts}},
		"a blocked worktree is kept":                      {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: stashed}},
		"--keep-worktree":                                 {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts}, keep: true},
		"unreadable git keeps the worktree":               {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), inspErr: errors.New("git broke")}},
		"no worktree left":                                {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed()}},
		"a launch that died before its workspace": {row: func(*testing.T) worker.Row {
			return worker.Row{Name: "w", Branch: "feat", Stage: worker.StageFailed, Worktree: "/r/.claude/worktrees/w"}
		}, fake: fakeClose{facts: cleanFacts}},
		"a row an earlier close kept": {row: func(t *testing.T) worker.Row {
			r := launchedRow(t)
			r.Stage = worker.StageClosed
			return r
		}, fake: fakeClose{facts: cleanFacts}},
		"a launch still inside its settle window": {row: func(*testing.T) worker.Row {
			return worker.Row{Name: "w", Stage: worker.StagePending, StartedAt: now.Add(-time.Minute)}
		}, fake: fakeClose{facts: cleanFacts}},
		"an undecodable ledger reference": {row: func(t *testing.T) worker.Row {
			r := launchedRow(t)
			r.Ref = []byte(`{"kind":"nope"}`)
			return r
		}, fake: fakeClose{facts: cleanFacts}},
		"a failed launch that left a recovery tag": {row: func(*testing.T) worker.Row {
			return worker.Row{Name: "w", Branch: "feat", Stage: worker.StageFailed, Recovery: "forgectl-abc"}
		}, fake: fakeClose{facts: cleanFacts}},
		"a launch past its settle window": {row: func(*testing.T) worker.Row {
			return worker.Row{Name: "w", Branch: "feat", Stage: worker.StageWorktree, StartedAt: now.Add(-launchSettleAfter - time.Second), Worktree: "/r/.claude/worktrees/w"}
		}, fake: fakeClose{facts: cleanFacts}},
		"a failed launch that holds a workspace": {row: func(t *testing.T) worker.Row {
			r := launchedRow(t)
			r.Stage = worker.StageFailed
			return r
		}, fake: fakeClose{result: backend.NewCloseAlreadyGone(), facts: cleanFacts}},
		"every removal blocker keeps the worktree": {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: dirtyFacts()}},
		"--keep-worktree on an unreadable git":     {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), inspErr: errors.New("git broke")}, keep: true},
		"--keep-worktree on a missing worktree":    {row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed()}, keep: true},
	}
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			row := sc.row(t)
			planFake := sc.fake
			plan := planClose(ctx, row, sc.keep, now, func(ctx context.Context) (worker.WorktreeFacts, error) {
				planFake.calls = append(planFake.calls, "inspect")
				return planFake.facts, planFake.inspErr
			})
			realFake := sc.fake
			closed := closeWorker(ctx, row, sc.keep, now, realFake.steps())

			if plan.Refused != !closed.Closed {
				t.Fatalf("plan refused = %v, close closed = %v", plan.Refused, closed.Closed)
			}
			if !plan.DryRun {
				t.Error("a plan is not marked dry_run")
			}
			if plan.WouldForget != closed.Forgotten {
				t.Errorf("would_forget = %v, close forgot = %v", plan.WouldForget, closed.Forgotten)
			}
			if plan.Reason != closed.Reason {
				t.Errorf("reason = %q, close said %q", plan.Reason, closed.Reason)
			}
			if plan.Note != closed.Note {
				t.Errorf("note = %q, close said %q", plan.Note, closed.Note)
			}
			if sc.keep && len(planFake.calls) != 0 {
				t.Errorf("--keep-worktree inspected the worktree: %v", planFake.calls)
			}
			if !reflect.DeepEqual(plan.KeptBecause, closed.KeptBecause) {
				t.Errorf("kept_because = %v, close kept %v", plan.KeptBecause, closed.KeptBecause)
			}
			wantWorkspace := map[string]string{
				closeWorkspaceClosed:  closeWorkspaceWouldClose,
				closeWorkspaceGone:    closeWorkspaceWouldClose,
				closeWorkspaceEarlier: closeWorkspaceEarlier,
				closeWorkspaceNone:    closeWorkspaceNone,
				closeWorkspaceRefused: closeWorkspaceRefused,
			}[closed.Workspace]
			if plan.Workspace != wantWorkspace {
				t.Errorf("workspace = %q, want %q (close: %q)", plan.Workspace, wantWorkspace, closed.Workspace)
			}
			wantWorktree := map[string]string{
				closeWorktreeRemoved:   closeWorktreeWouldRemove,
				closeWorktreeKept:      closeWorktreeWouldKeep,
				closeWorktreeGone:      closeWorktreeGone,
				closeWorktreeUntouched: closeWorktreeUntouched,
			}[closed.Worktree]
			if plan.Worktree != wantWorktree {
				t.Errorf("worktree = %q, want %q (close: %q)", plan.Worktree, wantWorktree, closed.Worktree)
			}
			for _, call := range planFake.calls {
				if call != "inspect" {
					t.Errorf("the plan made a write call: %q", call)
				}
			}
		})
	}
}

// The preview never asks herdr: not even a workspace herdr would refuse to
// close is reported as refused, because only close can know.
func TestPlanCloseDoesNotAskHerdr(t *testing.T) {
	unreadable := backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureUnavailable, errors.New("protocol_mismatch")))
	f := fakeClose{result: unreadable, facts: cleanFacts}
	plan := planClose(context.Background(), launchedRow(t), false, time.Unix(1e9, 0), func(context.Context) (worker.WorktreeFacts, error) {
		return f.facts, nil
	})
	if plan.Refused || plan.Workspace != closeWorkspaceWouldClose || plan.Worktree != closeWorktreeWouldRemove || !plan.WouldForget {
		t.Fatalf("plan = %+v", plan)
	}
	if len(f.calls) != 0 {
		t.Fatalf("the plan called the steps: %v", f.calls)
	}
}

func TestRenderClosePlan(t *testing.T) {
	ok := closePlan{Name: "w", Branch: "feat", DryRun: true, Workspace: closeWorkspaceWouldClose, Worktree: closeWorktreeWouldKeep, KeptBecause: []string{"--keep-worktree"}}
	refused := closePlan{Name: "w", DryRun: true, Refused: true, Workspace: closeWorkspaceRefused, Worktree: closeWorktreeUntouched, Reason: "its launch may still be running"}

	var out bytes.Buffer
	if err := renderClosePlan(&out, ok, false); err != nil {
		t.Fatalf("text: %v", err)
	}
	for _, want := range []string{"dry_run=true\n", "workspace=would-close\n", "worktree=would-keep\n", "would_forget=false\n", "kept: --keep-worktree\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := renderClosePlan(&out, ok, true); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if keys := mapKeys(got); !reflect.DeepEqual(keys, []string{"branch", "dry_run", "kept_because", "name", "refused", "workspace", "worktree", "would_forget"}) {
		t.Errorf("keys = %v", keys)
	}

	// A would-be refusal exits 1, as close does, so close --dry-run && close stops.
	out.Reset()
	err := renderClosePlan(&out, refused, false)
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "would refuse") {
		t.Errorf("text refusal: err = %v (exit %d)", err, ExitCode(err))
	}
	// Text mode prints the preview lines before the error, as JSON mode prints
	// its object, so a caller reading stdout sees the same facts either way.
	for _, want := range []string{"dry_run=true\n", "refused=true\n", "workspace=refused\n", "worktree=untouched\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("a refused text preview lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	err = renderClosePlan(&out, refused, true)
	if ExitCode(err) != 1 || !strings.Contains(out.String(), `"refused": true`) {
		t.Errorf("json refusal: err = %v (exit %d), out %s", err, ExitCode(err), out.String())
	}
}

// `surface close --dry-run` end to end: the row is read, the worktree is
// inspected, herdr is never run, and the ledger row and the worktree stay.
func TestSurfaceCloseDryRunTouchesNothing(t *testing.T) {
	top := dryRunRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HERDR_SESSION", "dryrun")

	// A fake herdr that records every call: any call at all fails the test.
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "herdr-ran")
	script := "#!/bin/sh\necho \"$@\" >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: a backend stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A real worktree with no unsaved work, so a real close would remove it.
	gitIn(t, top, "branch", "feat/w1")
	wtPath := worker.WorktreePath(top, "w1")
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, top, "worktree", "add", "-q", wtPath, "feat/w1")
	head := strings.TrimSpace(gitIn(t, top, "rev-parse", "HEAD"))

	led, err := worker.Open(top, "dryrun")
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Begin(worker.Row{Name: "w1", Branch: "feat/w1", StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	encoded, err := testHerdrRef(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Update("w1", func(r *worker.Row) {
		r.Stage, r.Worktree, r.Base, r.Ref = worker.StageLaunched, wtPath, head, encoded
	}); err != nil {
		t.Fatal(err)
	}
	before := repoSnapshot(t, top)

	run := func(args ...string) (string, error) {
		cmd := newSurfaceCloseCmd(dryRunDeps(t))
		var o, e bytes.Buffer
		cmd.SetOut(&o)
		cmd.SetErr(&e)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return o.String(), err
	}

	out, err := run("w1", "--repo", top, "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	for _, want := range []string{"dry_run=true\n", "name=w1\n", "workspace=would-close\n", "worktree=would-remove\n", "would_forget=true\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run ran herdr: %v", err)
	}
	if after := repoSnapshot(t, top); after != before {
		t.Errorf("the dry run changed the repo:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("the dry run removed the worktree: %v", err)
	}
	if rows, err := led.Rows(); err != nil || len(rows) != 1 || rows[0].Stage != worker.StageLaunched {
		t.Errorf("the dry run changed the ledger: %+v (err %v)", rows, err)
	}

	// A name with no row is the usage error close gives, preview or not.
	_, err = run("nosuch", "--repo", top, "--dry-run")
	if ExitCode(err) != 2 {
		t.Errorf("unknown worker: err = %v (exit %d), want exit 2", err, ExitCode(err))
	}
}

// A worker launch's --dry-run, end to end through the command: herdr is on
// PATH (a recording stub that fails every call) and is never run; the repo and
// the state dir are unchanged.
func TestSurfaceWorkerLaunchDryRunCreatesNothing(t *testing.T) {
	top := dryRunRepo(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HERDR_SESSION", "dryrun")
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "herdr-ran")
	script := "#!/bin/sh\necho \"$@\" >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: a backend stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	before := repoSnapshot(t, top)

	cmd := newSurfaceLaunchCmd(dryRunDeps(t))
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetArgs([]string{top, "--surface", "herdr", "--worktree", "feat/w1", "--name", "w1", "--dry-run", "--json"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("dry run: %v\nstderr: %s", err, e.String())
	}
	var got launchPlan
	if err := json.Unmarshal(o.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, o.String())
	}
	if !got.DryRun || got.Worker == nil || got.Worker.Worktree != worker.WorktreePath(top, "w1") || got.Worker.BranchFrom != "new" {
		t.Errorf("preview = %+v", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run ran herdr: %v", err)
	}
	if after := repoSnapshot(t, top); after != before {
		t.Errorf("the dry run changed the repo:\nbefore %s\nafter  %s", before, after)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Errorf("the dry run wrote to the state dir: %v", entries)
	}
}

// TestSurfaceLaunchDryRunFromASubfolder is cadence-ecosystem#608 in the
// preview: a claude launch aimed at a repository subfolder whose root carries
// the .claude settings previews the root as run_directory, with the same move
// notice on stderr as the launch, and --here previews the target itself.
func TestSurfaceLaunchDryRunFromASubfolder(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: a backend stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, sub := settingsRootRepo(t)

	deps := dryRunDeps(t)
	deps.Cfg.Launch.Defaults = config.LaunchDefaults{Harness: "claude", BinaryPath: stubHarness(t)}

	run := func(args ...string) (string, string, error) {
		cmd := newSurfaceLaunchCmd(deps)
		var o, e bytes.Buffer
		cmd.SetOut(&o)
		cmd.SetErr(&e)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return o.String(), e.String(), err
	}

	out, stderr, err := run(sub, "--surface", "tmux", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got["target"] != sub || got["run_directory"] != root {
		t.Errorf("target/run_directory = %v/%v, want %s/%s", got["target"], got["run_directory"], sub, root)
	}
	if !strings.Contains(stderr, "forgectl: starting claude in") {
		t.Errorf("stderr = %q, want the move notice", stderr)
	}

	out, stderr, err = run(sub, "--surface", "tmux", "--dry-run", "--here")
	if err != nil {
		t.Fatalf("dry run --here: %v", err)
	}
	if !strings.Contains(out, "run_directory="+sub+"\n") {
		t.Errorf("--here preview does not start in the target:\n%s", out)
	}
	if stderr != "" {
		t.Errorf("--here: stderr = %q, want nothing", stderr)
	}
}
