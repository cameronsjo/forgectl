package quarantine

// Test plan for quarantine.go
//
// Hide (Classification: FS mutation with a security guard)
//   [x] Happy: PrefixUnderscore scheme renames CLAUDE.md -> _CLAUDE.md
//   [x] Happy: SuffixQuarantined scheme renames CLAUDE.md -> CLAUDE.md.quarantined
//   [x] Happy: nested target (.github/copilot-instructions.md) renames only the base name
//   [x] Happy: a DIRECTORY target (.claude/) is renamed as a unit (_.claude),
//       and a file inside it rides along with its content intact — issue
//       #20's verification pass found no test exercising this path directly
//       (only the separate destructive strip step's test touched it)
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
//   [x] Happy: a quarantined DIRECTORY restores as a unit, its file's content
//       round-tripping exactly (see TestHide_DirectoryTarget)
//
// DefaultTargets (Classification: pure data)
//   [x] Happy: exported and non-empty

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestHide_DirectoryTarget covers issue #20's verification-pass gap: no
// test exercised a DIRECTORY quarantine target (.claude/, the default-list
// entry every other DefaultTargets member is a plain file) end-to-end
// through Hide AND Restore — only workflow's separate destructive strip
// step's test touched a directory-shaped target. os.Rename on a directory
// moves it (and everything inside) as a single filesystem unit; this
// asserts that holds through both directions of quarantine.
func TestHide_DirectoryTarget(t *testing.T) {
	root := t.TempDir()
	const settingsContent = `{"theme":"dark"}`
	writeFile(t, filepath.Join(root, ".claude", "settings.json"), settingsContent)

	c := New(&exec.FakeRunner{})
	moves, err := c.Hide(context.Background(), root, PrefixUnderscore, []string{".claude/"}, false)
	if err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("expected 1 move, got %d: %+v", len(moves), moves)
	}
	wantTo := filepath.Join(root, "_.claude")
	if moves[0].To != wantTo {
		t.Errorf("move.To = %q, want %q", moves[0].To, wantTo)
	}

	info, err := os.Stat(wantTo)
	if err != nil {
		t.Fatalf("_.claude should exist after hide, stat err = %v", err)
	}
	if !info.IsDir() {
		t.Errorf("_.claude should still be a directory, got mode %v", info.Mode())
	}
	if got := readFile(t, filepath.Join(wantTo, "settings.json")); got != settingsContent {
		t.Errorf("settings.json content did not ride along with the directory rename, got %q, want %q", got, settingsContent)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude should be renamed away, stat err = %v", err)
	}

	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restoredPath := filepath.Join(root, ".claude")
	info, err = os.Stat(restoredPath)
	if err != nil {
		t.Fatalf(".claude should be restored, stat err = %v", err)
	}
	if !info.IsDir() {
		t.Errorf("restored .claude should still be a directory, got mode %v", info.Mode())
	}
	if got := readFile(t, filepath.Join(restoredPath, "settings.json")); got != settingsContent {
		t.Errorf("restored settings.json content = %q, want original content preserved (%q)", got, settingsContent)
	}
	if _, err := os.Stat(wantTo); !os.IsNotExist(err) {
		t.Errorf("_.claude should be gone after restore, stat err = %v", err)
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

func TestComputeMoves_ResolvesEachTargetWithoutMutatingFS(t *testing.T) {
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
	// Neither file exists on disk; ComputeMoves may read path identities but
	// must not create or error on them.
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

// --- ExpandTargets: nested instruction files (recursive quarantine) ---
//
// Test plan
//   [x] A nested CLAUDE.md / AGENTS.md is discovered; the root literal survives
//   [x] Non-nestable entries (.claude/, .cursor, …) are never expanded
//   [x] An explicit path target (one with a directory component) is literal
//   [x] Discovery is direction-agnostic: the ALREADY-QUARANTINED tree yields
//       the identical target list, which is what makes teardown reversible
//   [x] .git is not walked
//   [x] Full round-trip: Hide(expanded) then Restore(ComputeMoves(expanded))
//       recomputed from scratch restores every nested file byte-for-byte

func TestExpandTargets_FindsNestedInstructionFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "root")
	writeFile(t, filepath.Join(root, "src", "AGENTS.md"), "nested")
	writeFile(t, filepath.Join(root, "packages", "api", "CLAUDE.md"), "deep")
	writeFile(t, filepath.Join(root, "docs", "README.md"), "not an instruction file")

	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}

	for _, want := range []string{
		"AGENTS.md",                       // root literal preserved
		"CLAUDE.md",                       // absent at root, still preserved
		filepath.Join("src", "AGENTS.md"), // nested
		filepath.Join("packages", "api", "CLAUDE.md"), // deeply nested
		".claude", ".cursor", ".github/copilot-instructions.md",
	} {
		if !containsStr(got, want) {
			t.Errorf("ExpandTargets missing %q; got %v", want, got)
		}
	}
	if containsStr(got, filepath.Join("docs", "README.md")) {
		t.Errorf("ExpandTargets expanded a non-instruction file: %v", got)
	}
}

// TestExpandTargets_OnlyExpandsNestableBasenames guards the blast radius: a
// recursive sweep for `.cursor` or an explicitly-pathed target would
// quarantine files the caller never asked about.
func TestExpandTargets_OnlyExpandsNestableBasenames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub", ".github", "copilot-instructions.md"), "nested copilot")
	writeFile(t, filepath.Join(root, "other", "src", "AGENTS.md"), "nested but explicitly pathed target")

	// Non-nestable entry: never expanded, even though a nested match exists.
	got, err := ExpandTargets(root, SuffixQuarantined, []string{".github/copilot-instructions.md"})
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if len(got) != 1 || got[0] != ".github/copilot-instructions.md" {
		t.Errorf("non-nestable target was expanded: %v", got)
	}

	// An entry carrying a directory component is an explicit path — literal.
	got, err = ExpandTargets(root, SuffixQuarantined, []string{filepath.Join("other", "src", "AGENTS.md")})
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("explicitly-pathed target was expanded: %v", got)
	}
}

// TestExpandTargets_SameListBeforeAndAfterHide is the property teardown
// depends on. ComputeMoves recomputes from ExpandTargets against a workspace
// whose files are already RENAMED; if discovery only matched original names it
// would find nothing and silently leave nested files quarantined forever.
func TestExpandTargets_SameListBeforeAndAfterHide(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "AGENTS.md"), "nested")
	writeFile(t, filepath.Join(root, "packages", "api", "CLAUDE.md"), "deep")

	before, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (before): %v", err)
	}
	if _, err := New(&exec.FakeRunner{}).Hide(context.Background(), root, SuffixQuarantined, before, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	after, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (after): %v", err)
	}
	if !equalStrings(before, after) {
		t.Errorf("target list changed across Hide:\n before = %v\n after  = %v", before, after)
	}
}

func TestExpandTargets_SkipsGitDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "objects", "AGENTS.md"), "must not be quarantined")

	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if containsStr(got, filepath.Join(".git", "objects", "AGENTS.md")) {
		t.Errorf("ExpandTargets walked into .git: %v", got)
	}
}

// TestExpandTargets_NestedRoundTrip is the end-to-end reversibility proof:
// hide with an expanded list, then restore from a target list recomputed from
// scratch (exactly what teardown does — it holds no persisted Move list).
func TestExpandTargets_NestedRoundTrip(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		filepath.Join(root, "AGENTS.md"):                    "root agents",
		filepath.Join(root, "src", "AGENTS.md"):             "nested agents",
		filepath.Join(root, "packages", "api", "CLAUDE.md"): "deep claude",
		filepath.Join(root, "docs", "guide.md"):             "untouched",
	}
	for path, content := range files {
		writeFile(t, path, content)
	}

	c := New(&exec.FakeRunner{})
	hideTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (hide): %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, hideTargets, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	// Every instruction file is gone from its original path.
	for path := range files {
		if filepath.Base(path) == "guide.md" {
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("still present after Hide: %s", path)
		}
	}

	// Teardown's exact sequence: recompute targets, recompute moves, restore.
	restoreTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (restore): %v", err)
	}
	moves, err := ComputeMoves(root, SuffixQuarantined, restoreTargets)
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for path, want := range files {
		if got := readFile(t, path); got != want {
			t.Errorf("round-trip corrupted %s: got %q, want %q", path, got, want)
		}
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExpandTargets_MatchesNestedCaseInsensitively closes an injection vector
// through the very hole the recursive walk was added to close. APFS is
// case-insensitive by default and forgectl is macOS-first, so a PR head
// carrying `src/agents.md` is NOT matched by an exact-string walk — yet the
// reviewing agent's open("src/AGENTS.md") resolves to it and the instructions
// are read.
//
// The root level is covered by accident (Hide's os.Lstat matches
// case-insensitively); only the nested walk missed, so this test is nested.
func TestExpandTargets_MatchesNestedCaseInsensitively(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "agents.md"), "lowercase nested")
	writeFile(t, filepath.Join(root, "pkg", "Claude.md"), "mixed-case nested")

	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	for _, want := range []string{
		filepath.Join("src", "agents.md"),
		filepath.Join("pkg", "Claude.md"),
	} {
		if !containsStr(got, want) {
			t.Errorf("case-variant nested instruction file %q not quarantined; got %v", want, got)
		}
	}
}

// TestExpandTargets_CaseVariantRoundTrip proves the case-variant path is
// reversible too — the rename must restore the ORIGINAL on-disk spelling, not
// a normalized one.
func TestExpandTargets_CaseVariantRoundTrip(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "src", "agents.md")
	writeFile(t, nested, "lowercase nested")

	c := New(&exec.FakeRunner{})
	hideTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (hide): %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, hideTargets, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "src", "agents.md.quarantined")); err != nil {
		t.Errorf("case-variant file was not quarantined to its own spelling: %v", err)
	}

	restoreTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (restore): %v", err)
	}
	moves, err := ComputeMoves(root, SuffixQuarantined, restoreTargets)
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := readFile(t, nested); got != "lowercase nested" {
		t.Errorf("round-trip corrupted the case-variant file: %q", got)
	}
}

// TestRestore_OccupiedDestinationIsNotFatal pins the asymmetry with Hide.
// discard returns on a Restore error BEFORE sandbox.Teardown, the tmux kill,
// and the breadcrumb delete — so a fatal Restore would permanently strand the
// clean room: workspace on disk holding fetched content, `pr list` still
// showing it, every `pr teardown` retry failing identically.
//
// Restore must therefore skip an occupied destination and keep going. It must
// still not clobber.
func TestRestore_OccupiedDestinationIsNotFatal(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "CLAUDE.md")
	quarantined := filepath.Join(root, "CLAUDE.md.quarantined")
	writeFile(t, original, "the survivor")
	writeFile(t, quarantined, "planted by the review")

	// A second, unoccupied move must still be restored — the skip is per-move,
	// not an abort.
	otherOriginal := filepath.Join(root, "AGENTS.md")
	otherQuarantined := filepath.Join(root, "AGENTS.md.quarantined")
	writeFile(t, otherQuarantined, "legitimately quarantined")

	c := New(&exec.FakeRunner{})
	err := c.Restore(context.Background(), []Move{
		{From: original, To: quarantined},
		{From: otherOriginal, To: otherQuarantined},
	})
	if err != nil {
		t.Fatalf("an occupied destination must not fail teardown: %v", err)
	}
	if got := readFile(t, original); got != "the survivor" {
		t.Errorf("Restore clobbered the original: %q", got)
	}
	if got := readFile(t, otherOriginal); got != "legitimately quarantined" {
		t.Errorf("the unoccupied move was not restored: %q", got)
	}
}

// TestDefaultTargets_CoversClaudeLocal pins CLAUDE.local.md, which Claude Code
// reads at the project root and which nests the same way CLAUDE.md does.
func TestDefaultTargets_CoversClaudeLocal(t *testing.T) {
	if !containsStr(DefaultTargets, "CLAUDE.local.md") {
		t.Errorf("DefaultTargets omits CLAUDE.local.md: %v", DefaultTargets)
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub", "CLAUDE.local.md"), "nested local instructions")

	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if !containsStr(got, filepath.Join("sub", "CLAUDE.local.md")) {
		t.Errorf("nested CLAUDE.local.md not quarantined; got %v", got)
	}
}

// TestUndecorate_CaseInsensitive guards the reversibility property. The walk
// matches case-insensitively, so `src/AGENTS.md.QUARANTINED` reaches
// undecorate; an exact TrimSuffix left it unchanged, Hide renamed it to
// `…QUARANTINED.quarantined`, and the teardown walk no longer matched that.
// --- AI-config carrier coverage (#195) ---
//
// Test plan
//   [x] .mcp.json is covered, with the measurement that justifies it
//   [x] The enumeration audits ITSELF: DefaultTargets is exactly the tier
//       groups plus the pattern rule, both directions
//   [x] Every DefaultTargets entry actually round-trips through Hide/Restore —
//       derived from the list, never a second copy of it
//   [x] Over-quarantining control: reviewable content stays readable
//   [x] The MCP pattern rule reaches an UNENUMERATED dot-directory
//   [x] The pattern rule is bounded: no nested walk, no non-dot directories
//   [x] Pattern entries never survive expansion into Hide
//   [x] A pre-existing .mcp.json.quarantined fails loud rather than clobbering

// TestDefaultTargets_CoversMCPCarrier pins the one entry whose absence was a
// live remote-code-execution path, and records WHY — naming the measurement is
// what stops a future reader deleting this as redundant with
// --strict-mcp-config.
//
// Measured on Claude Code 2.1.220, reproducing the review posture exactly
// (`claude -p`, --permission-mode plan, the deny-by-default workspace
// allowlist in force): a root .mcp.json whose server declared
// `/bin/sh -c "touch <sentinel>"` left the sentinel on disk. The session
// answered normally and the agent never invoked an MCP tool — registration
// alone spawned the process. Renaming the same file to .mcp.json.quarantined
// suppressed the spawn, which is what makes quarantine a sufficient control
// for this carrier.
func TestDefaultTargets_CoversMCPCarrier(t *testing.T) {
	if !containsStr(DefaultTargets, ".mcp.json") {
		t.Fatalf("DefaultTargets omits .mcp.json — a PR author's `command` runs on the reviewer's host: %v", DefaultTargets)
	}
	if !containsStr(tier1Carriers, ".mcp.json") {
		t.Errorf(".mcp.json must sit in tier 1 (read by the dispatched harness), not tier 2: %v", tier1Carriers)
	}

	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"probe":{"command":"/bin/sh"}}}`)

	targets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	c := New(&exec.FakeRunner{})
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, targets, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".mcp.json")); !os.IsNotExist(err) {
		t.Errorf(".mcp.json still readable under its own name after Hide, stat err = %v", err)
	}
}

// TestDefaultTargets_TierSumIsComplete is the self-audit. The defect in #195
// was a hand-maintained literal drifting from reality, so asserting a
// hard-coded expected list here would relocate that defect into the test file
// rather than fix it. Instead it asserts the STRUCTURE both ways:
//
//   - every tier/pattern element reaches DefaultTargets — a new group declared
//     and left unwired fails loudly;
//   - every DefaultTargets element comes from exactly one group — an entry
//     appended outside the tiers fails loudly;
//   - the lengths agree — a duplicate or a dropped entry fails loudly.
func TestDefaultTargets_TierSumIsComplete(t *testing.T) {
	groups := map[string][]string{
		"tier1Carriers":     tier1Carriers,
		"tier2Carriers":     tier2Carriers,
		"mcpConfigPatterns": mcpConfigPatterns,
	}

	sum := 0
	origin := make(map[string]string)
	for name, group := range groups {
		sum += len(group)
		for _, entry := range group {
			if prev, dup := origin[entry]; dup {
				t.Errorf("%q appears in both %s and %s", entry, prev, name)
			}
			origin[entry] = name
			if !containsStr(DefaultTargets, entry) {
				t.Errorf("%s entry %q never reaches DefaultTargets — is the group wired into concatTargets?", name, entry)
			}
		}
	}

	if len(DefaultTargets) != sum {
		t.Errorf("len(DefaultTargets) = %d, want %d (the tier groups summed): %v", len(DefaultTargets), sum, DefaultTargets)
	}
	for _, entry := range DefaultTargets {
		if _, ok := origin[entry]; !ok {
			t.Errorf("DefaultTargets carries %q, which belongs to no tier group — add it to a tier, not to the list", entry)
		}
	}
}

// TestCarrierGroups_AllReachDefaultTargets closes the hole
// TestDefaultTargets_TierSumIsComplete cannot: that test names its groups, so
// a NEW group declared and never wired into concatTargets stays invisible to
// it — the same hand-maintained enumeration this issue was about, relocated
// into the test file.
//
// So read the source instead. Every package-level var whose name ends in
// `Carriers` or `Patterns` is a carrier group, and every one of its entries
// must reach DefaultTargets. Declaring `.zed/settings.json` in a new tier and
// forgetting to concatenate it fails HERE, at the moment the carrier is added,
// rather than silently leaving it unquarantined.
//
// Three properties this test's own shape has to hold, each of them a measured
// evasion of an earlier version:
//
//   - It parses the whole PACKAGE, not quarantine.go. Splitting a growing
//     carrier list into carriers.go is the natural next refactor, and a
//     one-file parse would greet it with a green suite.
//   - A group that is not a literal slice is an ERROR, not a skip.
//     `var tier3Carriers = buildTier3()` is unreadable to an AST audit, so the
//     audit must say so rather than fall silent — silence here reads exactly
//     like coverage.
//   - It keys on the NAMING CONVENTION rather than on "any []string". Under
//     the old rule any package-level []string that was not a carrier group
//     failed: an allowlist like `alwaysReviewable = []string{".github/workflows"}`
//     — precisely the over-quarantine guard this change's own discussion
//     invites — failed the test, and the naive fix (add it to DefaultTargets)
//     hides the CI workflows. A guard that pushes you toward the security
//     regression it exists to prevent is worse than no guard.
//
// The convention is the contract: a group named outside it evades this test.
// TestDefaultTargets_TierSumIsComplete is the second net (it catches the
// init()-append this one cannot), and neither substitutes for naming a new
// group `…Carriers`.
func TestCarrierGroups_AllReachDefaultTargets(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	fset := token.NewFileSet()
	groups, parsed := 0, 0
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		parsed++
		file, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !isCarrierGroupName(name.Name) {
						continue
					}
					groups++
					if i >= len(vs.Values) {
						t.Errorf("carrier group %s (%s) declares no value — this audit reads literal slices, so an appended-to group is invisible to it; build it as a []string literal", name.Name, src)
						continue
					}
					entries, ok := stringSliceLiteral(vs.Values[i])
					if !ok {
						t.Errorf("carrier group %s (%s) is not a []string literal — this audit cannot read a function-built group, and a silent skip would look like coverage; keep carrier groups literal", name.Name, src)
						continue
					}
					for _, entry := range entries {
						if !containsStr(DefaultTargets, entry) {
							t.Errorf("carrier group %s declares %q, which never reaches DefaultTargets — wire the group into concatTargets", name.Name, entry)
						}
					}
				}
			}
		}
	}
	if parsed == 0 {
		t.Fatal("found no non-test sources in the package — this test has gone vacuous")
	}
	if groups == 0 {
		t.Fatal("found no carrier groups in the package — this test has gone vacuous")
	}
}

// isCarrierGroupName reports whether a package-level var name declares a
// carrier group, by the `…Carriers` / `…Patterns` convention the audit above
// keys on. Naming, rather than type, so that an ordinary package-level
// []string is not conscripted into the carrier set by accident.
func isCarrierGroupName(name string) bool {
	return strings.HasSuffix(name, "Carriers") || strings.HasSuffix(name, "Patterns")
}

// stringSliceLiteral reports the elements of a `[]string{…}` composite literal,
// and whether the expression was one at all.
func stringSliceLiteral(expr ast.Expr) ([]string, bool) {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	arr, ok := lit.Type.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return nil, false
	}
	if ident, ok := arr.Elt.(*ast.Ident); !ok || ident.Name != "string" {
		return nil, false
	}
	out := make([]string, 0, len(lit.Elts))
	for _, elt := range lit.Elts {
		bl, ok := elt.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return nil, false
		}
		unquoted, err := strconv.Unquote(bl.Value)
		if err != nil {
			return nil, false
		}
		out = append(out, unquoted)
	}
	return out, true
}

// TestDefaultTargets_EveryEntryRoundTrips is the behavioral half of the
// self-audit: it plants one file per DefaultTargets entry — derived from the
// list, so a new carrier is exercised the day it is added — and proves the
// whole set is hidden by Hide and returned intact by Restore. A carrier that
// is listed but unreachable by the machinery (a pattern the expander does not
// understand, say) fails here rather than in production.
func TestDefaultTargets_EveryEntryRoundTrips(t *testing.T) {
	root := t.TempDir()
	planted := make(map[string]string, len(DefaultTargets))
	for i, entry := range DefaultTargets {
		rel := plantablePath(entry, i)
		content := "carrier for " + entry
		writeFile(t, filepath.Join(root, rel), content)
		planted[rel] = content
	}

	c := New(&exec.FakeRunner{})
	hideTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (hide): %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, hideTargets, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	for rel := range planted {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("carrier %q survived Hide under its own name, stat err = %v", rel, err)
		}
	}

	restoreTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (restore): %v", err)
	}
	if !equalStrings(hideTargets, restoreTargets) {
		t.Fatalf("target list changed across Hide:\n before %v\n  after %v", hideTargets, restoreTargets)
	}
	moves, err := ComputeMoves(root, SuffixQuarantined, restoreTargets)
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, want := range planted {
		if got := readFile(t, filepath.Join(root, rel)); got != want {
			t.Errorf("round-trip corrupted %q: got %q, want %q", rel, got, want)
		}
	}
}

// plantablePath turns a DefaultTargets entry into one concrete relative path
// that entry must cover. A pattern is instantiated against a dot-directory
// name that appears nowhere in the source, which is the point of a pattern
// rule; a literal is planted as a plain file (directory-shaped targets have
// their own coverage in TestHide_DirectoryTarget).
func plantablePath(entry string, i int) string {
	if !isPattern(entry) {
		return filepath.Clean(entry)
	}
	dir, base := filepath.Split(filepath.Clean(entry))
	if dir == "" {
		return base
	}
	return filepath.Join(fmt.Sprintf(".unlisted-vendor-%d", i), base)
}

// TestDefaultTargets_DoesNotHideReviewableContent is the over-quarantine
// control. Quarantine operates on a repository whose entire PURPOSE is to be
// read by the reviewer, so widening the list has a real failure mode in the
// opposite direction. CI workflows are the sharpest case: `.github/` must
// never be quarantined wholesale, because a hostile workflow change is exactly
// what a reviewer must be able to see — a security regression wearing a
// security fix's clothes.
func TestDefaultTargets_DoesNotHideReviewableContent(t *testing.T) {
	for _, forbidden := range []string{
		".github", ".github/", ".github/workflows", ".github/workflows/",
		"README.md", "go.mod", "go.sum", ".gitignore", "package.json", "src", ".",
	} {
		if containsStr(DefaultTargets, forbidden) {
			t.Errorf("DefaultTargets hides reviewable content %q: %v", forbidden, DefaultTargets)
		}
	}

	root := t.TempDir()
	readable := []string{
		filepath.Join(".github", "workflows", "ci.yml"),
		filepath.Join(".github", "dependabot.yml"),
		"README.md", "go.mod", ".gitignore",
		filepath.Join("src", "main.go"),
		filepath.Join("docs", "design.md"),
	}
	for _, rel := range readable {
		writeFile(t, filepath.Join(root, rel), "reviewable")
	}

	c := New(&exec.FakeRunner{})
	targets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, targets, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	for _, rel := range readable {
		if got := readFile(t, filepath.Join(root, rel)); got != "reviewable" {
			t.Errorf("%q must stay readable to the reviewer, got %q", rel, got)
		}
	}
}

// TestExpandTargets_MCPPatternCoversUnenumeratedDotDir is the durability
// claim, stated as a test. `.aurora` is a vendor that exists nowhere in this
// codebase: the pattern rule must reach its MCP config anyway, because MCP
// configuration shares a shape (machine-read JSON carrying an execution
// primitive, at the root or one level down inside a dot-directory) where the
// prose carriers share nothing.
// `.gemini/MCP.json` is in the fixture because the ATTACKER writes the
// filename. filepath.Match is case-sensitive and APFS is not, so a pattern
// rule that matched exactly would leave that file readable under a name the
// reviewer's open(".gemini/mcp.json") resolves to — see globFold.
func TestExpandTargets_MCPPatternCoversUnenumeratedDotDir(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		filepath.Join(".aurora", "mcp.json"),
		filepath.Join(".zed", ".mcp.json"),
		filepath.Join(".gemini", "MCP.json"),
	} {
		writeFile(t, filepath.Join(root, rel), `{"mcpServers":{}}`)
	}

	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	for _, want := range []string{
		filepath.Join(".aurora", "mcp.json"),
		filepath.Join(".zed", ".mcp.json"),
		filepath.Join(".gemini", "MCP.json"),
	} {
		if !containsStr(got, want) {
			t.Errorf("pattern rule missed %q in an unenumerated dot-directory; got %v", want, got)
		}
	}

	// A dot-directory already quarantined WHOLESALE must not also be listed
	// nested — see coveredRootEntries: the nested entry would vanish from the
	// post-Hide list and break the reversibility property teardown depends on.
	writeFile(t, filepath.Join(root, ".cursor", "mcp.json"), `{"mcpServers":{}}`)
	got, err = ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if containsStr(got, filepath.Join(".cursor", "mcp.json")) {
		t.Errorf(".cursor is hidden as a unit; a nested entry under it breaks reversibility: %v", got)
	}
	if !containsStr(got, ".cursor") {
		t.Errorf(".cursor itself must still be a target: %v", got)
	}
}

// TestExpandTargets_MCPPatternIsCaseFolded is the reversible half of the
// case-fold, end to end. The pattern rule is the one place in the target list
// where an ATTACKER picks the filename, and filepath.Glob matches basenames
// case-sensitively while APFS does not — so `.gemini/MCP.json` used to survive
// Hide untouched while the reviewer's open(".gemini/mcp.json") resolved
// straight to it.
//
// It also pins the half that makes the fold safe: the target carries the
// ACTUAL on-disk spelling, so Restore returns `MCP.json` under its own name
// rather than silently case-normalizing it across the round trip (and rather
// than missing it entirely on a case-sensitive filesystem).
func TestExpandTargets_MCPPatternIsCaseFolded(t *testing.T) {
	root := t.TempDir()
	planted := map[string]string{
		filepath.Join(".gemini", "MCP.json"): `{"mcpServers":{"a":{}}}`,
		filepath.Join(".zed", ".MCP.JSON"):   `{"mcpServers":{"b":{}}}`,
		filepath.Join(".ok", "mcp.json"):     `{"mcpServers":{"c":{}}}`, // control: the rule is live
	}
	for rel, content := range planted {
		writeFile(t, filepath.Join(root, rel), content)
	}

	c := New(&exec.FakeRunner{})
	hideTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (hide): %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, hideTargets, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	for rel := range planted {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("MCP carrier %q survived Hide under its own name, stat err = %v", rel, err)
		}
	}

	restoreTargets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (restore): %v", err)
	}
	if !equalStrings(hideTargets, restoreTargets) {
		t.Fatalf("target list changed across Hide:\n before %v\n  after %v", hideTargets, restoreTargets)
	}
	moves, err := ComputeMoves(root, SuffixQuarantined, restoreTargets)
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, want := range planted {
		if got := readFile(t, filepath.Join(root, rel)); got != want {
			t.Errorf("round-trip corrupted %q: got %q, want %q", rel, got, want)
		}
	}
}

// TestExpandTargets_MCPPatternIsBounded guards the blast radius. The pattern is
// one level deep and dot-directories only: a nested `sub/.mcp.json` was
// measured NOT read by the launched harness, so a full-tree sweep would add
// reversibility risk to defend a hole that does not exist — and would start
// quarantining ordinary source directories that happen to hold an mcp.json.
func TestExpandTargets_MCPPatternIsBounded(t *testing.T) {
	root := t.TempDir()
	unmatched := []string{
		filepath.Join("sub", "mcp.json"),                  // non-dot directory
		filepath.Join("sub", ".mcp.json"),                 // non-dot directory
		filepath.Join(".aurora", "deep", "mcp.json"),      // two levels down
		filepath.Join("testdata", "fixtures", "mcp.json"), // ordinary fixture
	}
	for _, rel := range unmatched {
		writeFile(t, filepath.Join(root, rel), `{"mcpServers":{}}`)
	}

	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	for _, rel := range unmatched {
		if containsStr(got, rel) {
			t.Errorf("pattern rule over-reached to %q; got %v", rel, got)
		}
	}
}

// TestExpandTargets_DropsPatternEntries pins the contract Hide depends on:
// Hide resolves a target with os.Lstat, so a glob metacharacter surviving
// expansion would be a silent no-op — the carrier would look covered by the
// list and be quarantined by nothing.
func TestExpandTargets_DropsPatternEntries(t *testing.T) {
	root := t.TempDir()
	got, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	for _, entry := range got {
		if isPattern(entry) {
			t.Errorf("pattern %q survived expansion into the Hide target list: %v", entry, got)
		}
	}
	for _, p := range mcpConfigPatterns {
		if containsStr(got, p) {
			t.Errorf("pattern %q was passed through literally", p)
		}
	}
}

// TestHide_RefusesPreexistingQuarantinedMCP pins the fail-closed behavior
// across the widened list. Fetched PR content is hostile input, so a head
// carrying BOTH .mcp.json and .mcp.json.quarantined must fail the review
// rather than let os.Rename silently destroy one of them. Widening the carrier
// list widens the set of inputs that can trip this — deliberately.
func TestHide_RefusesPreexistingQuarantinedMCP(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".mcp.json"), "the live carrier")
	writeFile(t, filepath.Join(root, ".mcp.json.quarantined"), "the planted decoy")

	c := New(&exec.FakeRunner{})
	targets, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, targets, false); err == nil {
		t.Fatal("Hide must refuse an occupied quarantine destination, not clobber it")
	}
	if got := readFile(t, filepath.Join(root, ".mcp.json.quarantined")); got != "the planted decoy" {
		t.Errorf("the pre-existing file was clobbered: %q", got)
	}
}

// TestExpandTargets_MCPRoundTripIsReversible is the invariant a future
// widening of the list will break first: the same ExpandTargets call must
// return the identical list before Hide and against the already-renamed tree,
// because teardown recomputes it from scratch and holds no persisted moves.
func TestExpandTargets_MCPRoundTripIsReversible(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".mcp.json":                                   `{"root":true}`,
		filepath.Join(".aurora", "mcp.json"):          `{"unenumerated":true}`,
		filepath.Join(".cursor", "mcp.json"):          `{"inside a wholesale-hidden dir":true}`,
		filepath.Join(".cursor", "rules", "x.md"):     "cursor rules",
		filepath.Join("src", "main.go"):               "package main",
		filepath.Join("packages", "api", "CLAUDE.md"): "nested prose carrier",
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(root, rel), content)
	}

	c := New(&exec.FakeRunner{})
	before, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (before): %v", err)
	}
	if _, err := c.Hide(context.Background(), root, SuffixQuarantined, before, false); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	after, err := ExpandTargets(root, SuffixQuarantined, DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets (after): %v", err)
	}
	if !equalStrings(before, after) {
		t.Fatalf("target list is not direction-agnostic:\n before %v\n  after %v", before, after)
	}

	moves, err := ComputeMoves(root, SuffixQuarantined, after)
	if err != nil {
		t.Fatalf("ComputeMoves: %v", err)
	}
	if err := c.Restore(context.Background(), moves); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for rel, want := range files {
		if got := readFile(t, filepath.Join(root, rel)); got != want {
			t.Errorf("round-trip corrupted %q: got %q, want %q", rel, got, want)
		}
	}
}

func TestUndecorate_CaseInsensitive(t *testing.T) {
	for _, tc := range []struct {
		scheme   Scheme
		in, want string
	}{
		{SuffixQuarantined, "AGENTS.md.QUARANTINED", "AGENTS.md"},
		{SuffixQuarantined, "AGENTS.md.quarantined", "AGENTS.md"},
		{SuffixQuarantined, "AGENTS.md", "AGENTS.md"},
		{PrefixUnderscore, "_AGENTS.md", "AGENTS.md"},
		{PrefixUnderscore, "AGENTS.md", "AGENTS.md"},
	} {
		if got := undecorate(tc.scheme, tc.in); got != tc.want {
			t.Errorf("undecorate(%v, %q) = %q, want %q", tc.scheme, tc.in, got, tc.want)
		}
	}
}

// TestGlobFold_CharacterClassSemanticsSurviveFolding pins the constraint the
// case-fold has to respect: folding is for LITERALS, and a character class is
// not a literal.
//
// The first implementation lowercased the whole segment and the whole entry
// name. That is correct for `mcp.json` vs `MCP.json` and silently wrong for
// any class: `[^a-z]foo` matched `Afoo` before the fold and matched NOTHING
// after it, because the name's `A` folded to `a` and the class excludes `a`.
// `[a-z]foo` broke the other way, picking up `Afoo` that filepath.Match would
// never have given it. Both directions are pinned here — the second matters
// most, because globFold feeds the destructive `strip` step and a class that
// suddenly matches more is a class that deletes more.
//
// The literal cases are the control: they prove the class fix did not simply
// turn the fold off.
func TestGlobFold_CharacterClassSemanticsSurviveFolding(t *testing.T) {
	root := t.TempDir()
	// NOTE: no two fixture names may differ only by case — t.TempDir() lands on
	// APFS, where they would collide rather than coexist.
	for _, rel := range []string{"Afoo", "zfoo", "MCP.json"} {
		writeFile(t, filepath.Join(root, rel), "x")
	}

	tests := []struct {
		name    string
		pattern string
		want    []string
	}{
		{"negated class keeps excluding lowercase", "[^a-z]foo", []string{"Afoo"}},
		{"lowercase class keeps excluding uppercase", "[a-z]foo", []string{"zfoo"}},
		{"class combines with a folded literal", "[^a-z]FOO", []string{"Afoo"}},
		{"lowercase literal still finds the uppercase file", "mcp.json", []string{"MCP.json"}},
		{"uppercase literal still finds the mixed-case file", "MCP.JSON", []string{"MCP.json"}},
		{"wildcards are untouched by folding", "*.JSON", []string{"MCP.json"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matches, err := globFold(root, tc.pattern)
			if err != nil {
				t.Fatalf("globFold(%q): %v", tc.pattern, err)
			}
			var got []string
			for _, m := range matches {
				rel, relErr := filepath.Rel(root, m)
				if relErr != nil {
					t.Fatalf("Rel(%q): %v", m, relErr)
				}
				got = append(got, rel)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("globFold(%q) = %v, want %v", tc.pattern, got, tc.want)
			}
			for _, want := range tc.want {
				if !containsStr(got, want) {
					t.Errorf("globFold(%q) = %v, missing %q", tc.pattern, got, want)
				}
			}
		})
	}

	// A malformed class must still surface as ErrBadPattern rather than being
	// swallowed by the rewrite that copies it.
	if _, err := globFold(root, "[a-foo"); err == nil {
		t.Error("globFold must propagate filepath.Match's ErrBadPattern for an unterminated class")
	}
}

// TestPathPrefixPairs_DetectsSwallowingPair pins the detector itself against
// synthetic input, so the invariant test below can run over the REAL list
// without a bad pair ever being planted there.
//
// The `.cursor` / `.cursorrules` negative is the case that matters most: those
// two genuinely coexist in tier2Carriers, so a detector that compared raw
// string prefixes would fail the real list and invite the wrong fix — deleting
// a carrier that covers a distinct file.
func TestPathPrefixPairs_DetectsSwallowingPair(t *testing.T) {
	tests := []struct {
		name    string
		targets []string
		want    bool
	}{
		{"outer before inner", []string{".cursor", ".cursor/rules"}, true},
		{"inner before outer", []string{".cursor/rules", ".cursor"}, true},
		{"trailing slash still swallows", []string{".claude/", ".claude/settings.json"}, true},
		{"sibling sharing a prefix is not nesting", []string{".cursor", ".cursorrules"}, false},
		{"pattern entries are not paths", []string{".*/mcp.json", ".*/.mcp.json", ".cursor"}, false},
		{"identical entries do not nest", []string{".cursor", ".cursor"}, false},
		// The two rows that actually EXERCISE the isPattern skips. Without
		// them the guards are decorative: deleting both from pathPrefixPairs
		// leaves every other row above green, because none of those strings
		// is a raw prefix of another anyway. A pattern is not a path — the
		// pattern/literal overlap it would otherwise flag is already handled
		// at expansion time by coveredRootEntries.
		{"outer literal, inner pattern", []string{".cursor", ".cursor/*.json"}, false},
		{"outer pattern, inner pattern", []string{".*", ".*/mcp.json"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pairs := pathPrefixPairs(tc.targets)
			if got := len(pairs) > 0; got != tc.want {
				t.Fatalf("pathPrefixPairs(%v) = %v, want detected=%v", tc.targets, pairs, tc.want)
			}
		})
	}
}

// TestDefaultTargets_NoEntryIsPathPrefixOfAnother is a regression guard, not a
// live bug fix: DefaultTargets carries no such pair today. It exists because
// the next carrier addition is the one that would introduce it, and neither
// ordering of the resulting list produces a visible failure at runtime.
func TestDefaultTargets_NoEntryIsPathPrefixOfAnother(t *testing.T) {
	for _, pair := range pathPrefixPairs(DefaultTargets) {
		t.Errorf("DefaultTargets carries %q inside %q — replace the inner entry, do not join it.\n"+
			"Hiding %q first strands the tree: Hide leaves it inside the renamed outer directory and "+
			"Restore never reaches it, both returning nil. Hiding %q first round-trips, but then %q "+
			"can never match anything and is dead config that looks like coverage.",
			pair[1], pair[0], pair[1], pair[0], pair[1])
	}
	for _, scheme := range []Scheme{PrefixUnderscore, SuffixQuarantined} {
		if _, err := ExpandTargets(t.TempDir(), scheme, DefaultTargets); err != nil {
			t.Errorf("DefaultTargets violate the runtime move-graph classifier for scheme %v: %v", scheme, err)
		}
	}
}
