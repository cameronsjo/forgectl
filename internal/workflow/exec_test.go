package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestExecutor_TrivialWorkflow_ComposedArgv runs a worktree → strip →
// teardown workflow through a FakeRunner and asserts the exact composed argv
// git receives for the worktree step (strip/teardown touch the filesystem
// directly, not the Runner, so they're covered by the sandbox test below).
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

	plan, err := BuildPlan(wf, map[string]string{"repo": repoDir, "branch": "HEAD"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake)
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

	plan, err := BuildPlan(wf, map[string]string{"who": "world"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake)
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
	plan, err := BuildPlan(wf, map[string]string{"repo": "cameronsjo/forgectl"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake, WithDryRun(true))
	wctx := NewContext(nil)
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run (dry-run): %v", err)
	}

	if len(fake.Calls) != 0 {
		t.Fatalf("dry-run must issue zero Runner calls, got %d: %+v", len(fake.Calls), fake.Calls)
	}
}

// TestExecutor_Strip_DeletesOnlyConfiguredGlobsInsideWorkspace plants files in
// a real os.MkdirTemp sandbox (not FakeRunner — strip/teardown touch the
// filesystem directly) and verifies strip removes only the configured globs,
// leaving everything else untouched.
func TestExecutor_Strip_DeletesOnlyConfiguredGlobsInsideWorkspace(t *testing.T) {
	workspace, err := os.MkdirTemp("", "forgectl-strip-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(workspace)

	plant := func(rel, content string) {
		full := filepath.Join(workspace, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", rel, err)
		}
	}
	plant("CLAUDE.md", "agent instructions")
	plant(".claude/settings.json", "{}")
	plant("README.md", "keep me")
	plant("src/main.go", "package main")

	fake := &exec.FakeRunner{}
	exe := NewExecutor(fake)
	wctx := NewContext(nil)
	wctx.Set("workspace", workspace)

	plan := Plan{
		Name: "strip-only",
		Steps: []PlanStep{
			{Uses: "strip", Globs: []string{"CLAUDE.md", ".claude/"}},
		},
	}
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("CLAUDE.md should have been stripped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude/ should have been stripped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "README.md")); err != nil {
		t.Errorf("README.md should survive strip, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "src", "main.go")); err != nil {
		t.Errorf("src/main.go should survive strip, stat err = %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("strip must not shell out, got %d Runner calls: %+v", len(fake.Calls), fake.Calls)
	}
}

// TestExecutor_Strip_RejectsPathEscape is the ADR-0003 security requirement:
// a glob attempting to escape ${workspace} via ".." or an absolute path must
// be refused, never deleted.
func TestExecutor_Strip_RejectsPathEscape(t *testing.T) {
	workspace, err := os.MkdirTemp("", "forgectl-strip-escape-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(workspace)

	sentinel := filepath.Join(filepath.Dir(workspace), "forgectl-strip-escape-sentinel")
	if err := os.WriteFile(sentinel, []byte("must survive"), 0o644); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}
	defer os.Remove(sentinel)

	exe := NewExecutor(&exec.FakeRunner{})
	wctx := NewContext(nil)
	wctx.Set("workspace", workspace)

	cases := []PlanStep{
		{Uses: "strip", Globs: []string{"../" + filepath.Base(sentinel)}},
		{Uses: "strip", Globs: []string{sentinel}}, // absolute path
	}
	for _, step := range cases {
		plan := Plan{Name: "escape-attempt", Steps: []PlanStep{step}}
		if err := exe.Run(context.Background(), plan, wctx); err == nil {
			t.Errorf("expected a path-escape error for globs %v, got nil", step.Globs)
		}
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel outside workspace must survive, stat err = %v", err)
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

	exe := NewExecutor(&exec.FakeRunner{})
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

// TestExecutor_NotYetWiredSteps confirms launch/collect are registered but
// return the sentinel error rather than panicking or silently no-opping.
func TestExecutor_NotYetWiredSteps(t *testing.T) {
	exe := NewExecutor(&exec.FakeRunner{})
	wctx := NewContext(nil)

	for _, uses := range []string{"launch", "collect"} {
		plan := Plan{Name: "not-wired", Steps: []PlanStep{{Uses: uses}}}
		err := exe.Run(context.Background(), plan, wctx)
		if !errors.Is(err, ErrNotYetWired) {
			t.Errorf("%s step: expected ErrNotYetWired, got %v", uses, err)
		}
	}
}

// TestExecutor_CloneVerb_RoutesToWorktreeStepAndExportsWorkspace covers the
// newly-registered "clone" uses value: the registry maps it to worktreeStep
// (same runner as "worktree") and it must export ${workspace} exactly like
// worktree does. A remote-looking repo string (not a local path) exercises
// the git-clone branch of worktreeStep rather than "worktree add".
func TestExecutor_CloneVerb_RoutesToWorktreeStepAndExportsWorkspace(t *testing.T) {
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

	plan, err := BuildPlan(wf, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	exe := NewExecutor(fake)
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
	// worktreeStep created the dir via os.MkdirTemp; clean it up.
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
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
			exe := NewExecutor(fake)
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

// TestExecutor_Strip_ExpandsGlobPattern verifies the strip-list is a real glob:
// a "*.md" pattern removes every match, not only a file literally named "*.md".
func TestExecutor_Strip_ExpandsGlobPattern(t *testing.T) {
	workspace, err := os.MkdirTemp("", "forgectl-strip-glob-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(workspace)

	for _, f := range []string{"a.md", "b.md", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", f, err)
		}
	}

	exe := NewExecutor(&exec.FakeRunner{})
	wctx := NewContext(nil)
	wctx.Set("workspace", workspace)
	plan := Plan{Name: "glob-strip", Steps: []PlanStep{{Uses: "strip", Globs: []string{"*.md"}}}}
	if err := exe.Run(context.Background(), plan, wctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, gone := range []string{"a.md", "b.md"} {
		if _, err := os.Stat(filepath.Join(workspace, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be stripped by *.md, stat err = %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "keep.txt")); err != nil {
		t.Errorf("keep.txt should survive a *.md strip, stat err = %v", err)
	}
}

// TestExecutor_Strip_RefusesSymlinkEscape covers the glob-via-symlink vector: a
// pattern with no ".." can still match through a symlink pointing outside
// ${workspace}. withinWorkspace must refuse to delete through it.
func TestExecutor_Strip_RefusesSymlinkEscape(t *testing.T) {
	workspace, err := os.MkdirTemp("", "forgectl-strip-symlink-ws-*")
	if err != nil {
		t.Fatalf("MkdirTemp workspace: %v", err)
	}
	defer os.RemoveAll(workspace)

	external, err := os.MkdirTemp("", "forgectl-strip-symlink-ext-*")
	if err != nil {
		t.Fatalf("MkdirTemp external: %v", err)
	}
	defer os.RemoveAll(external)

	victim := filepath.Join(external, "victim.md")
	if err := os.WriteFile(victim, []byte("must survive"), 0o644); err != nil {
		t.Fatalf("WriteFile victim: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(workspace, "sub")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	exe := NewExecutor(&exec.FakeRunner{})
	wctx := NewContext(nil)
	wctx.Set("workspace", workspace)
	plan := Plan{Name: "symlink-escape", Steps: []PlanStep{{Uses: "strip", Globs: []string{"sub/*.md"}}}}
	if err := exe.Run(context.Background(), plan, wctx); err == nil {
		t.Error("expected refusal to strip through a workspace symlink escaping the sandbox")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("external victim.md must survive, stat err = %v", err)
	}
}
