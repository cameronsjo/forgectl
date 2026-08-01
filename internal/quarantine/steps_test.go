package quarantine

// Strip-step behavior tests, relocated from internal/workflow/exec_test.go
// with assertions unchanged when the strip verb moved to this module
// (ADR-0005's verb redistribution). They drive the step.Runner directly —
// the same function the merged workflow registry dispatches.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestSteps_ConfiguredGlobsWidenDefaultTargets pins the other side of the
// config seam, and it INVERTS its predecessor deliberately. That test
// (…NonEmptyDefaultGlobsOverrideDefaultTargets) asserted CLAUDE.md must
// SURVIVE a configured strip_globs, pinning replace semantics.
//
// Replace is the wrong rule for this key. The built-in clean-room workflow
// inherits DefaultTargets precisely by omitting `globs`, so under replace an
// operator adding one glob for their own workflow silently swapped out the
// clean room's entire carrier set — .mcp.json included — from a config key
// that reads like an addition. The clean room is forgectl's own control, not
// the operator's to shrink by accident; see stripFallback.
//
// So: both halves must go. The configured entry still takes effect (the
// direction the old test protected, and the len==0 / assignment-direction
// inversion it guarded against still fails here), and DefaultTargets still
// applies alongside it. Narrowing remains available, explicitly, by setting
// `globs` on the [[step]].
func TestSteps_ConfiguredGlobsWidenDefaultTargets(t *testing.T) {
	workspace := t.TempDir()
	for _, f := range []string{"CLAUDE.md", ".mcp.json", "custom.md", "README.md"} {
		if err := os.WriteFile(filepath.Join(workspace, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", f, err)
		}
	}

	def := Steps([]string{"custom.md"})["strip"]
	wctx := step.NewContext(nil)
	wctx.Set("workspace", workspace)
	// No step-level globs → the configured list is ADDED to DefaultTargets.
	if err := def.Runner(context.Background(), &exec.FakeRunner{}, wctx, step.PlanStep{Uses: "strip"}); err != nil {
		t.Fatalf("strip: %v", err)
	}
	for _, gone := range []string{"custom.md", "CLAUDE.md", ".mcp.json"} {
		if _, err := os.Stat(filepath.Join(workspace, gone)); !os.IsNotExist(err) {
			t.Errorf("%q should have been stripped (configured globs widen DefaultTargets, they do not replace it), stat err = %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "README.md")); err != nil {
		t.Errorf("README.md must stay readable to the reviewer, stat err = %v", err)
	}
}

// TestSteps_StepGlobsNarrowExplicitly is the escape hatch stripFallback keeps
// open: a [[step]] that SETS `globs` replaces the list outright. That is a
// workflow author choosing a narrower set in the open, on a guarded field —
// as opposed to an operator shrinking the built-in clean room from an
// unrelated config key without being shown the trade.
func TestSteps_StepGlobsNarrowExplicitly(t *testing.T) {
	workspace := t.TempDir()
	for _, f := range []string{"CLAUDE.md", "custom.md"} {
		if err := os.WriteFile(filepath.Join(workspace, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", f, err)
		}
	}

	def := Steps(nil)["strip"]
	wctx := step.NewContext(nil)
	wctx.Set("workspace", workspace)
	if err := def.Runner(context.Background(), &exec.FakeRunner{}, wctx, step.PlanStep{Uses: "strip", Globs: []string{"custom.md"}}); err != nil {
		t.Fatalf("strip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "custom.md")); !os.IsNotExist(err) {
		t.Errorf("custom.md (the step's own glob) should have been stripped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md must survive when a [[step]] sets its own globs, stat err = %v", err)
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

// TestSteps_DefaultTargetsPatternsReachStrip is the destructive half of the
// carrier work, and the riskier half: `strip` os.RemoveAll's what it matches,
// where Hide only renames. The MCP pattern rule therefore has to be verified
// on THIS path too: strip shares globFold with ExpandTargets, so the resolver
// is no longer what differs, but strip receives the RAW target list — patterns
// included, no ExpandTargets pre-processing pass — and it deletes rather than
// renames what that list resolves to.
//
// It pins both directions at once: an MCP carrier in an unenumerated
// dot-directory is destroyed, and the reviewable tree — CI workflows above
// all — survives. Over-stripping is the failure mode with no undo.
func TestSteps_DefaultTargetsPatternsReachStrip(t *testing.T) {
	workspace := t.TempDir()
	write := func(rel string) string {
		path := filepath.Join(workspace, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	stripped := []string{
		".mcp.json",
		filepath.Join(".aurora", "mcp.json"),
		filepath.Join(".zed", ".mcp.json"),
		// The attacker writes the filename, and filepath.Match is case-sensitive
		// where APFS is not — an exact-matching strip leaves these readable under
		// a name the reviewer's open() resolves to. See globFold.
		filepath.Join(".gemini", "MCP.json"),
		filepath.Join(".windsurf", ".MCP.JSON"),
	}
	survives := []string{
		filepath.Join(".github", "workflows", "ci.yml"),
		"README.md", "go.mod", ".gitignore",
		filepath.Join("src", "main.go"),
		filepath.Join("sub", ".mcp.json"), // non-dot dir: outside the bounded rule
	}
	for _, rel := range append(append([]string{}, stripped...), survives...) {
		write(rel)
	}

	def := Steps(nil)["strip"]
	wctx := step.NewContext(nil)
	wctx.Set("workspace", workspace)
	if err := def.Runner(context.Background(), &exec.FakeRunner{}, wctx, step.PlanStep{Uses: "strip"}); err != nil {
		t.Fatalf("strip: %v", err)
	}

	for _, rel := range stripped {
		if _, err := os.Stat(filepath.Join(workspace, rel)); !os.IsNotExist(err) {
			t.Errorf("MCP carrier %q survived strip, stat err = %v", rel, err)
		}
	}
	for _, rel := range survives {
		if _, err := os.Stat(filepath.Join(workspace, rel)); err != nil {
			t.Errorf("strip destroyed reviewable content %q: %v", rel, err)
		}
	}
}

// TestValidateStripGlob_RejectsWorkspaceRoot covers the shape with no undo.
// The escape guards are all about leaving ${workspace}; this one is about
// naming ${workspace} itself. `filepath.Clean(".")` is ".", which the resolver
// would hand back as the workspace root, and strip os.RemoveAll's what it is
// handed — so `strip_globs = ["."]` deletes the whole clean room mid-run while
// passing every other check (not empty, not absolute, no ".." segment).
//
// Every spelling that CLEANS to the root is listed, not just the literal ".".
func TestValidateStripGlob_RejectsWorkspaceRoot(t *testing.T) {
	// Spellings that must be named as the workspace root specifically.
	for _, g := range []string{".", "./", ".//", "./.", "\\", ".\\"} {
		err := validateStripGlob(g)
		if err == nil {
			t.Errorf("validateStripGlob(%q) = nil, must reject the workspace root", g)
			continue
		}
		if !strings.Contains(err.Error(), "workspace root") {
			t.Errorf("validateStripGlob(%q) = %v, want a workspace-root rejection", g, err)
		}
	}
	// "/" is rejected as absolute on Unix and as the root on Windows; either
	// verdict is correct, only "accepted" is not.
	if err := validateStripGlob("/"); err == nil {
		t.Error(`validateStripGlob("/") = nil, must be rejected`)
	}
	// Control: an ordinary glob still passes, so the guard above is not
	// rejecting everything.
	for _, g := range []string{".mcp.json", ".*/mcp.json", "*.md", "docs/."} {
		if err := validateStripGlob(g); err != nil {
			t.Errorf("validateStripGlob(%q) = %v, want nil", g, err)
		}
	}
}

// TestSteps_StripRefusesWorkspaceRootGlob is the same guard proven where it
// matters — on the runner that calls os.RemoveAll — through BOTH entry points
// an operator has: [workflow] strip_globs in config, and `globs` on a [[step]].
//
// The fixture is a t.TempDir() this test creates itself and nothing else, for
// the obvious reason: the failure mode under test is a recursive delete of the
// directory named by the glob. A regression here destroys only the sandbox.
func TestSteps_StripRefusesWorkspaceRootGlob(t *testing.T) {
	// seed builds a throwaway workspace with a sentinel file and a sentinel
	// subtree, and returns a check that both survived.
	seed := func(t *testing.T) (string, func(t *testing.T)) {
		t.Helper()
		workspace := t.TempDir()
		nested := filepath.Join(workspace, "src", "main.go")
		if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		for _, p := range []string{filepath.Join(workspace, "keep.txt"), nested} {
			if err := os.WriteFile(p, []byte("reviewable"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		}
		return workspace, func(t *testing.T) {
			t.Helper()
			for _, p := range []string{filepath.Join(workspace, "keep.txt"), nested} {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("strip destroyed %q through a whole-root glob: %v", p, err)
				}
			}
		}
	}

	t.Run("step globs", func(t *testing.T) {
		workspace, survived := seed(t)
		err := runStrip(t, &exec.FakeRunner{}, workspace, []string{"."})
		if err == nil {
			t.Error(`strip accepted globs = ["."], which resolves to the workspace root`)
		} else if !strings.Contains(err.Error(), "workspace root") {
			t.Errorf("strip rejected the root glob with the wrong reason: %v", err)
		}
		survived(t)
	})

	t.Run("configured strip_globs", func(t *testing.T) {
		workspace, survived := seed(t)
		def, ok := Steps([]string{"."})["strip"]
		if !ok {
			t.Fatal(`Steps must contribute the strip verb`)
		}
		wctx := step.NewContext(nil)
		wctx.Set("workspace", workspace)
		// No step globs: the configured default set applies, root glob included.
		err := def.Runner(context.Background(), &exec.FakeRunner{}, wctx, step.PlanStep{Uses: "strip"})
		if err == nil {
			t.Error(`strip accepted strip_globs = ["."], which resolves to the workspace root`)
		} else if !strings.Contains(err.Error(), "workspace root") {
			t.Errorf("strip rejected the root glob with the wrong reason: %v", err)
		}
		survived(t)
	})
}
