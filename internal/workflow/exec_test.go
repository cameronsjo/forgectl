package workflow

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Strip-step behavior tests live with their owning module now —
// internal/quarantine/steps_test.go (ADR-0005's verb redistribution).

// TestExecutor_TrivialWorkflow_ComposedArgv runs a worktree → strip →
// teardown workflow through a FakeRunner and asserts the exact composed argv
// git receives for the worktree step (strip/teardown touch the filesystem
// directly, not the Runner, so they're covered by the quarantine/sandbox
// tests).
func TestExecutor_TrivialWorkflow_ComposedArgv(t *testing.T) {
	repoDir := t.TempDir()

	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", nil
		},
	}

	wf := Workflow{
		DSLVersion: 1,
		Name:       "trivial",
		Steps: []Step{
			{Uses: "worktree", Repo: "${repo}", Ref: "${branch}"},
			{Uses: "strip", Globs: []string{"CLAUDE.md"}},
			{Uses: "teardown"},
		},
	}

	plan, err := BuildPlan(wf, map[string]string{"repo": repoDir, "branch": "HEAD"}, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake, testRegistry(t))
	wctx := NewContext(nil)
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 Runner call (git worktree add), got %d: %+v", len(fake.Calls), fake.Calls)
	}
	call := fake.Calls[0]
	if call.Name != "git" {
		t.Errorf("call.Name = %q, want git", call.Name)
	}
	want := []string{"-C", repoDir, "worktree", "add"}
	if len(call.Args) < len(want) {
		t.Fatalf("args too short: %v", call.Args)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("arg %d: got %q want %q (full args: %v)", i, call.Args[i], w, call.Args)
		}
	}
	if call.Args[len(call.Args)-1] != "HEAD" {
		t.Errorf("expected final worktree add arg HEAD, got %q", call.Args[len(call.Args)-1])
	}
}

// TestExecutor_RunOnlyWorkflow_ComposedArgv covers the simpler run-only shape
// the deliverable also allows: a single `run` step's argv must match exactly.
func TestExecutor_RunOnlyWorkflow_ComposedArgv(t *testing.T) {
	fake := &exec.FakeRunner{}

	wf := Workflow{
		DSLVersion: 1,
		Name:       "run-only",
		Steps: []Step{
			{Uses: "run", Cmd: "echo", Args: []string{"hello", "${who}"}},
		},
	}

	plan, err := BuildPlan(wf, map[string]string{"who": "world"}, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake, testRegistry(t))
	wctx := NewContext(nil)
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 Runner call, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	call := fake.Calls[0]
	if call.Name != "echo" {
		t.Errorf("call.Name = %q, want echo", call.Name)
	}
	want := []string{"hello", "world"}
	if len(call.Args) != len(want) || call.Args[0] != want[0] || call.Args[1] != want[1] {
		t.Errorf("call.Args = %v, want %v", call.Args, want)
	}
}

// TestExecutor_DryRun_ZeroRunnerCalls is the spike's core acceptance
// criterion: --dry-run builds the Plan and returns without invoking any
// StepRunner, so the FakeRunner records zero calls.
func TestExecutor_DryRun_ZeroRunnerCalls(t *testing.T) {
	fake := &exec.FakeRunner{}

	data, err := builtinFS.ReadFile("builtins/clean-room-review.workflow.toml")
	if err != nil {
		t.Fatalf("read embedded builtin: %v", err)
	}
	wf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := BuildPlan(wf, map[string]string{"repo": "cameronsjo/forgectl"}, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake, testRegistry(t), WithDryRun(true))
	wctx := NewContext(nil)
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run (dry-run): %v", err)
	}

	if len(fake.Calls) != 0 {
		t.Fatalf("dry-run must issue zero Runner calls, got %d: %+v", len(fake.Calls), fake.Calls)
	}
}

// TestExecutor_Teardown_Idempotent verifies teardown is safe to run twice
// (ADR-0003's reentrancy requirement): the second call on an already-removed
// workspace must not error.
func TestExecutor_Teardown_Idempotent(t *testing.T) {
	workspace, err := os.MkdirTemp("", "forgectl-teardown-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	exe := NewExecutor(&exec.FakeRunner{}, testRegistry(t))
	wctx := NewContext(nil)
	wctx.Set("workspace", workspace)

	plan := Plan{Name: "teardown-twice", Steps: []PlanStep{{Uses: "teardown"}}}
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace should be gone after teardown, stat err = %v", err)
	}
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("second (idempotent) teardown must not error, got: %v", err)
	}
}

// TestExecutor_NotYetWiredSteps confirms launch (module-contributed) and
// collect (builtin) are registered but return the sentinel error rather than
// panicking or silently no-opping.
func TestExecutor_NotYetWiredSteps(t *testing.T) {
	exe := NewExecutor(&exec.FakeRunner{}, testRegistry(t))
	wctx := NewContext(nil)

	for _, uses := range []string{"launch", "collect"} {
		plan := Plan{Name: "not-wired", Steps: []PlanStep{{Uses: uses}}}
		err := exe.Run(context.Background(), plan, wctx)
		if !errors.Is(err, ErrNotYetWired) {
			t.Errorf("%s step: expected ErrNotYetWired, got %v", uses, err)
		}
	}
}

// TestExecutor_UnknownVerbWithoutContribution pins the eviction story's
// failure mode: an executor built over the bare builtins (no module
// contributions) must fail loudly on a module-owned verb like strip.
func TestExecutor_UnknownVerbWithoutContribution(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	exe := NewExecutor(&exec.FakeRunner{}, reg)
	plan := Plan{Name: "no-strip", Steps: []PlanStep{{Uses: "strip"}}}
	if err := exe.Run(context.Background(), plan, NewContext(nil)); err == nil {
		t.Fatal("expected an unknown-verb error for strip without quarantine's contribution")
	}
}

// TestExecutor_CloneVerb_ClonesAndExportsWorkspace covers the "clone" uses
// value on a remote-looking repo: it must git-clone and export ${workspace}
// exactly like worktree does.
func TestExecutor_CloneVerb_ClonesAndExportsWorkspace(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", nil
		},
	}

	wf := Workflow{
		DSLVersion: 1,
		Name:       "clone-only",
		Steps: []Step{
			{Uses: "clone", Repo: "cameronsjo/forgectl", Ref: "main"},
		},
	}

	plan, err := BuildPlan(wf, nil, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake, testRegistry(t))
	wctx := NewContext(nil)
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 Runner call (git clone), got %d: %+v", len(fake.Calls), fake.Calls)
	}
	call := fake.Calls[0]
	if call.Name != "git" {
		t.Errorf("call.Name = %q, want git", call.Name)
	}
	if len(call.Args) == 0 || call.Args[0] != "clone" {
		t.Errorf("expected a git clone invocation, got args %v", call.Args)
	}

	workspace, ok := wctx.Get("workspace")
	if !ok || workspace == "" {
		t.Fatal("clone step must export ${workspace}, same as worktree")
	}
	// The sandbox step created the dir via os.MkdirTemp; clean it up.
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
}

// TestExecutor_CloneVerb_ClonesLocalRepo locks the worktree/clone verb split:
// an explicit `clone` on a LOCAL repo path must git-clone (full isolation on
// request — no shared object store, no .git back-pointer to the source
// checkout), never silently downgrade to a worktree.
func TestExecutor_CloneVerb_ClonesLocalRepo(t *testing.T) {
	repoDir := t.TempDir()
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", nil
		},
	}

	plan := Plan{Name: "clone-local", Steps: []PlanStep{{Uses: "clone", Repo: repoDir}}}
	wctx := NewContext(nil)
	exe := NewExecutor(fake, testRegistry(t))
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 Runner call (git clone), got %d: %+v", len(fake.Calls), fake.Calls)
	}
	call := fake.Calls[0]
	if call.Name != "git" || len(call.Args) == 0 || call.Args[0] != "clone" {
		t.Errorf("explicit clone on a local repo must git-clone, got %s %v", call.Name, call.Args)
	}
	if workspace, ok := wctx.Get("workspace"); ok && workspace != "" {
		t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	}
}

// TestExecutor_Run_ResolvesExportsAtExecuteTime locks the design contract that
// a deferred ${export} reference resolves during execute: a `run` step
// consuming ${workspace} must receive the actual sandbox path as its argv,
// never the literal string "${workspace}".
func TestExecutor_Run_ResolvesExportsAtExecuteTime(t *testing.T) {
	repoDir := t.TempDir()
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", nil
		},
	}

	wf := Workflow{
		DSLVersion: 1,
		Name:       "export-consumer",
		Steps: []Step{
			{Uses: "worktree", Repo: repoDir},
			{Uses: "run", Cmd: "echo", Args: []string{"${workspace}"}},
			{Uses: "teardown"},
		},
	}
	plan, err := BuildPlan(wf, nil, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// At plan time the export is deferred, so the arg is still the literal.
	if got := plan.Steps[1].Args[0]; got != "${workspace}" {
		t.Fatalf("plan-time arg = %q, want the deferred literal ${workspace}", got)
	}

	exe := NewExecutor(fake, testRegistry(t))
	wctx := NewContext(nil)
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	workspace, ok := wctx.Get("workspace")
	if !ok || workspace == "" {
		t.Fatal("worktree step must export ${workspace}")
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("expected 2 Runner calls (worktree add, echo), got %d: %+v", len(fake.Calls), fake.Calls)
	}
	echoCall := fake.Calls[1]
	if echoCall.Name != "echo" {
		t.Errorf("call.Name = %q, want echo", echoCall.Name)
	}
	if len(echoCall.Args) != 1 || echoCall.Args[0] != workspace {
		t.Errorf("echo args = %v, want the resolved workspace %q — a literal ${workspace} means execute-time interpolation didn't run", echoCall.Args, workspace)
	}
}

// TestExecutor_Run_ForwardReferenceFails covers the other side of execute-time
// resolution: consuming an export BEFORE the step that produces it has run
// must be a hard error — never a command executed with a literal "${...}".
func TestExecutor_Run_ForwardReferenceFails(t *testing.T) {
	repoDir := t.TempDir()
	fake := &exec.FakeRunner{}

	wf := Workflow{
		DSLVersion: 1,
		Name:       "forward-ref",
		Steps: []Step{
			{Uses: "run", Cmd: "echo", Args: []string{"${workspace}"}},
			{Uses: "worktree", Repo: repoDir},
		},
	}
	plan, err := BuildPlan(wf, nil, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err) // plan-time deferral keeps this buildable
	}

	exe := NewExecutor(fake, testRegistry(t))
	if err := exe.Run(context.Background(), plan, NewContext(nil)); err == nil {
		t.Fatal("expected a hard error for a forward reference, got nil")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no command may run with an unresolved ${workspace}, got calls: %+v", fake.Calls)
	}
}

// TestExecutor_Worktree_RejectsOptionLikeRepoRef locks the git-argument-
// injection defense: a repo or ref beginning with '-' (which git would parse as
// an option, e.g. --upload-pack=<cmd>) is refused before any git invocation.
func TestExecutor_Worktree_RejectsOptionLikeRepoRef(t *testing.T) {
	repoDir := t.TempDir()
	cases := []struct {
		name string
		step PlanStep
	}{
		{"option-like repo", PlanStep{Uses: "worktree", Repo: "--upload-pack=touch /tmp/pwned"}},
		{"option-like ref", PlanStep{Uses: "worktree", Repo: repoDir, Ref: "--upload-pack=x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			exe := NewExecutor(fake, testRegistry(t))
			plan := Plan{Name: "inject", Steps: []PlanStep{tc.step}}
			if err := exe.Run(context.Background(), plan, NewContext(nil)); err == nil {
				t.Fatal("expected rejection of an option-like value, got nil")
			}
			if len(fake.Calls) != 0 {
				t.Errorf("git must not run for a rejected value, got calls: %+v", fake.Calls)
			}
		})
	}
}
