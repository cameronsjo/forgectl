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

// --- ExpandTargets: nested instruction files (recursive quarantine) ---
//
// Test plan
//   [x] A nested CLAUDE.md / AGENTS.md is discovered; the root literal survives
//   [x] Non-nestable entries (.claude/, .cursor/rules, …) are never expanded
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
		".claude/", ".cursor/rules", ".github/copilot-instructions.md",
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
// recursive sweep for `.cursor/rules` or an explicitly-pathed target would
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
