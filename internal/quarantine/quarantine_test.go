package quarantine

// Test plan for quarantine.go
//
// Hide (Classification: FS mutation with a security guard)
//   [x] Happy: PrefixUnderscore scheme renames CLAUDE.md -> _CLAUDE.md
//   [x] Happy: SuffixQuarantined scheme renames CLAUDE.md -> CLAUDE.md.quarantined
//   [x] Happy: nested target (.github/copilot-instructions.md) renames only the base name
//   [x] Happy: dry-run reports the planned Move but makes ZERO filesystem changes
//   [x] Happy: a missing target is a no-op (skipped, not an error)
//   [x] Unhappy: a target containing ".." is rejected before any rename
//   [x] Unhappy: an absolute target is rejected before any rename
//   [x] Unhappy: a target that is a symlink escaping root is rejected before any rename
//   [x] Unhappy: a pre-existing destination is refused (no clobber) and the original survives
//
// Restore (Classification: FS mutation, reversal)
//   [x] Happy: Restore reverses Hide's moves exactly (round-trip)
//   [x] Happy: Restore is idempotent — a missing quarantined path is a no-op
//
// DefaultTargets (Classification: pure data)
//   [x] Happy: exported and non-empty

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestHide_PrefixScheme_RenamesEachTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "agent instructions")

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"CLAUDE.md"}, false)
	if err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("expected 1 move, got %d: %+v", len(moves), moves)
	}
	wantTo := filepath.Join(root, "_CLAUDE.md")
	if moves[0].To != wantTo {
		t.Errorf("move.To = %q, want %q", moves[0].To, wantTo)
	}
	if _, err := os.Stat(wantTo); err != nil {
		t.Errorf("_CLAUDE.md should exist after hide, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("CLAUDE.md should be renamed away, stat err = %v", err)
	}
}

func TestHide_SuffixScheme_RenamesEachTarget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "agent instructions")

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, SuffixQuarantined, []string{"CLAUDE.md"}, false)
	if err != nil {
		t.Fatalf("Hide: %v", err)
	}
	wantTo := filepath.Join(root, "CLAUDE.md.quarantined")
	if len(moves) != 1 || moves[0].To != wantTo {
		t.Fatalf("moves = %+v, want single move To %q", moves, wantTo)
	}
	if _, err := os.Stat(wantTo); err != nil {
		t.Errorf("CLAUDE.md.quarantined should exist after hide, stat err = %v", err)
	}
}

func TestHide_NestedTarget_RenamesOnlyBaseName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".github", "copilot-instructions.md"), "x")

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{".github/copilot-instructions.md"}, false)
	if err != nil {
		t.Fatalf("Hide: %v", err)
	}
	wantTo := filepath.Join(root, ".github", "_copilot-instructions.md")
	if len(moves) != 1 || moves[0].To != wantTo {
		t.Fatalf("moves = %+v, want single move To %q", moves, wantTo)
	}
	if _, err := os.Stat(wantTo); err != nil {
		t.Errorf("renamed nested file should exist, stat err = %v", err)
	}
}

func TestHide_DryRun_ZeroFSChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "agent instructions")

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"CLAUDE.md"}, true)
	if err != nil {
		t.Fatalf("Hide (dry-run): %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("dry-run should still report the planned move, got %d", len(moves))
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Errorf("dry-run must not rename CLAUDE.md, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "_CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run must not create _CLAUDE.md, stat err = %v", err)
	}
}

func TestHide_MissingTarget_NoOp(t *testing.T) {
	root := t.TempDir()

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"CLAUDE.md", "AGENTS.md"}, false)
	if err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if len(moves) != 0 {
		t.Errorf("missing targets should be skipped as no-ops, got moves: %+v", moves)
	}
}

func TestHide_RejectsParentTraversalTarget(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(filepath.Dir(root), "quarantine-traversal-sentinel")
	writeFile(t, sentinel, "must survive")
	defer os.Remove(sentinel)

	c := New(&exec.FakeRunner{})
	_, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"../" + filepath.Base(sentinel)}, false)
	if err == nil {
		t.Fatal("expected a path-traversal error, got nil")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel outside root must survive, stat err = %v", err)
	}
}

func TestHide_RejectsAbsoluteTarget(t *testing.T) {
	root := t.TempDir()
	c := New(&exec.FakeRunner{})
	_, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"/etc/passwd"}, false)
	if err == nil {
		t.Fatal("expected an absolute-path error, got nil")
	}
}

func TestHide_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	victim := filepath.Join(external, "victim.md")
	writeFile(t, victim, "must survive")

	link := filepath.Join(root, "escape.md")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	c := New(&exec.FakeRunner{})
	_, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"escape.md"}, false)
	if err == nil {
		t.Fatal("expected refusal to quarantine a symlink escaping root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("external victim.md must survive, stat err = %v", err)
	}
}

func TestHide_RejectsPreexistingDestination(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "original")
	// A checkout crafted to already contain the quarantined name would be
	// clobbered by a bare os.Rename; Hide must refuse and leave both intact.
	writeFile(t, filepath.Join(root, "_CLAUDE.md"), "pre-existing collision")

	c := New(&exec.FakeRunner{})
	_, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"CLAUDE.md"}, false)
	if err == nil {
		t.Fatal("expected a destination-collision error, got nil")
	}
	if got := readFile(t, filepath.Join(root, "CLAUDE.md")); got != "original" {
		t.Errorf("original CLAUDE.md must survive unchanged, got %q", got)
	}
	if got := readFile(t, filepath.Join(root, "_CLAUDE.md")); got != "pre-existing collision" {
		t.Errorf("pre-existing _CLAUDE.md must not be clobbered, got %q", got)
	}
}

func TestRestore_RoundTripsExactly(t *testing.T) {
	root := t.TempDir()
	claudePath := filepath.Join(root, "CLAUDE.md")
	writeFile(t, claudePath, "agent instructions")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "more instructions")

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{"CLAUDE.md", "AGENTS.md"}, false)
	if err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if len(moves) != 2 {
		t.Fatalf("expected 2 moves, got %d: %+v", len(moves), moves)
	}

	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for _, want := range []string{"CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("%s should be restored, stat err = %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "_CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("_CLAUDE.md should be gone after restore, stat err = %v", err)
	}
	content, err := os.ReadFile(claudePath)
	if err != nil || string(content) != "agent instructions" {
		t.Errorf("restored CLAUDE.md content = %q, err = %v, want original content preserved", content, err)
	}
}

func TestRestore_MissingMove_Idempotent(t *testing.T) {
	root := t.TempDir()
	moves := []Move{
		{From: filepath.Join(root, "CLAUDE.md"), To: filepath.Join(root, "_CLAUDE.md")},
	}
	c := New(&exec.FakeRunner{})
	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore of an already-restored (or never-hidden) move must not error, got: %v", err)
	}
}

func TestComputeMoves_ResolvesEachTargetWithoutTouchingFS(t *testing.T) {
	root := t.TempDir()
	moves, err := ComputeMoves(root, PrefixUnderscore, []string{"CLAUDE.md", "AGENTS.md"})
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if len(moves) != 2 {
		t.Fatalf("expected 2 moves, got %d: %+v", len(moves), moves)
	}
	if moves[0].To != filepath.Join(root, "_CLAUDE.md") {
		t.Errorf("moves[0].To = %q, want %q", moves[0].To, filepath.Join(root, "_CLAUDE.md"))
	}
	// Neither file exists on disk; ComputeMoves must not create or error on them.
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("ComputeMoves must not touch the filesystem, stat err = %v", err)
	}
}

func TestDefaultTargets_NonEmpty(t *testing.T) {
	if len(DefaultTargets) == 0 {
		t.Fatal("DefaultTargets must not be empty")
	}
}

// writeFile creates path (and its parent dirs) with content, failing the test
// on any error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}
