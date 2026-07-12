package quarantine

// Strip-step behavior tests, relocated from internal/workflow/exec_test.go
// with assertions unchanged when the strip verb moved to this module
// (ADR-0005's verb redistribution). They drive the step.Runner directly —
// the same function the merged workflow registry dispatches.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/step"
)

// runStrip drives the contributed strip runner over a workspace-seeded
// context, mirroring how the workflow Executor dispatches it.
func runStrip(t *testing.T, fake *exec.FakeRunner, workspace string, globs []string) error {
	t.Helper()
	def, ok := Steps(nil)["strip"]
	if !ok {
		t.Fatal("Steps(nil) must contribute the strip verb")
	}
	wctx := step.NewContext(nil)
	wctx.Set("workspace", workspace)
	return def.Runner(context.Background(), fake, wctx, step.PlanStep{Uses: "strip", Globs: globs})
}

// TestStrip_DeletesOnlyConfiguredGlobsInsideWorkspace plants files in a real
// os.MkdirTemp sandbox (not FakeRunner — strip touches the filesystem
// directly) and verifies strip removes only the configured globs, leaving
// everything else untouched.
func TestStrip_DeletesOnlyConfiguredGlobsInsideWorkspace(t *testing.T) {
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
	if err := runStrip(t, fake, workspace, []string{"CLAUDE.md", ".claude/"}); err != nil {
		t.Fatalf("strip: %v", err)
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

// TestStrip_RejectsPathEscape is the ADR-0003 security requirement: a glob
// attempting to escape ${workspace} via ".." or an absolute path must be
// refused, never deleted.
func TestStrip_RejectsPathEscape(t *testing.T) {
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

	cases := [][]string{
		{"../" + filepath.Base(sentinel)},
		{sentinel}, // absolute path
	}
	for _, globs := range cases {
		if err := runStrip(t, &exec.FakeRunner{}, workspace, globs); err == nil {
			t.Errorf("expected a path-escape error for globs %v, got nil", globs)
		}
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel outside workspace must survive, stat err = %v", err)
	}
}

// TestStrip_ExpandsGlobPattern verifies the strip-list is a real glob: a
// "*.md" pattern removes every match, not only a file literally named "*.md".
func TestStrip_ExpandsGlobPattern(t *testing.T) {
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

	if err := runStrip(t, &exec.FakeRunner{}, workspace, []string{"*.md"}); err != nil {
		t.Fatalf("strip: %v", err)
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

// TestStrip_RefusesSymlinkEscape covers the glob-via-symlink vector: a
// pattern with no ".." can still match through a symlink pointing outside
// ${workspace}. WithinWorkspace must refuse to delete through it.
func TestStrip_RefusesSymlinkEscape(t *testing.T) {
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

	if err := runStrip(t, &exec.FakeRunner{}, workspace, []string{"sub/*.md"}); err == nil {
		t.Error("expected refusal to strip through a workspace symlink escaping the sandbox")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("external victim.md must survive, stat err = %v", err)
	}
}

// TestStrip_MissingWorkspaceErrors pins the precondition: strip without a
// ${workspace} export (no prior worktree/clone step) is a hard error.
func TestStrip_MissingWorkspaceErrors(t *testing.T) {
	def := Steps(nil)["strip"]
	err := def.Runner(context.Background(), &exec.FakeRunner{}, step.NewContext(nil), step.PlanStep{Uses: "strip"})
	if err == nil {
		t.Fatal("expected an error for strip without ${workspace}")
	}
}

// TestSteps_NonEmptyDefaultGlobsOverrideDefaultTargets pins the other side
// of the config seam: a configured [workflow] strip_globs list REPLACES
// DefaultTargets as the fallback. If the len==0 guard or the assignment
// direction in Steps ever inverted, a user's override would silently stop
// taking effect (always DefaultTargets) with the suite green — this is the
// test that goes red instead.
func TestSteps_NonEmptyDefaultGlobsOverrideDefaultTargets(t *testing.T) {
	workspace := t.TempDir()
	for _, f := range []string{"CLAUDE.md", "custom.md"} {
		if err := os.WriteFile(filepath.Join(workspace, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", f, err)
		}
	}

	def := Steps([]string{"custom.md"})["strip"]
	wctx := step.NewContext(nil)
	wctx.Set("workspace", workspace)
	// No step-level globs → the configured override applies, NOT DefaultTargets.
	if err := def.Runner(context.Background(), &exec.FakeRunner{}, wctx, step.PlanStep{Uses: "strip"}); err != nil {
		t.Fatalf("strip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "custom.md")); !os.IsNotExist(err) {
		t.Errorf("custom.md (the configured override) should have been stripped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md must SURVIVE when a configured override replaces DefaultTargets, stat err = %v", err)
	}
}

// TestSteps_DefaultGlobsFallBackToDefaultTargets pins the config seam: an
// empty default list falls back to the canonical DefaultTargets, so the
// destructive strip and the reversible Hide can never drift.
func TestSteps_DefaultGlobsFallBackToDefaultTargets(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "CLAUDE.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	def := Steps(nil)["strip"]
	wctx := step.NewContext(nil)
	wctx.Set("workspace", workspace)
	// No step globs AND no configured default → DefaultTargets applies.
	if err := def.Runner(context.Background(), &exec.FakeRunner{}, wctx, step.PlanStep{Uses: "strip"}); err != nil {
		t.Fatalf("strip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("CLAUDE.md (a DefaultTargets entry) should have been stripped, stat err = %v", err)
	}
}
