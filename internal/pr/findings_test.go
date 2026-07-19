package pr

// Test plan for findings.go
//
// FindingsList (Classification: read-only enumeration over a forgectl-owned dir)
//   [x] Returns one entry per direct-child findings dir
//   [x] Absent findings dir returns (nil, nil), mirroring List's os.IsNotExist handling
//
// findingsRemovalCandidate (Classification: pure decision — the actual containment guard)
//   [x] Rejects a path outside findingsDir even with isDir forced true (the
//       branch a real symlink entry never reaches, since a real DirEntry's
//       IsDir() is already false for a symlink — this is the direct proof
//       the containment check itself works, independent of that filter)
//   [x] Accepts a contained, old-enough directory
//
// FindingsCleanup (Classification: deletion guard — no path parameter)
//   [x] Only removes dirs older than the cutoff
//   [x] Dry-run (apply=false) reports what would be removed and deletes nothing
//   [x] A top-level symlink is skipped, never removed (via the isDir filter —
//       see findingsRemovalCandidate's doc comment for why containment is a
//       second, currently-unreached line of defense for this exact case)
//   [x] A plain file at the top level is ignored, never removed
//
// FindingsRemove (Classification: TOCTOU-safe apply over an explicit, already-confirmed set)
//   [x] Removes exactly the given paths
//   [x] A path that stopped qualifying since the confirm (already gone) is
//       skipped, not treated as an error

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindingsList_ReturnsCreatedDirs(t *testing.T) {
	dir := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	mustMkdir(t, filepath.Join(dir, findingsDirPrefix+"aaa"))
	mustMkdir(t, filepath.Join(dir, findingsDirPrefix+"bbb"))

	entries, err := c.FindingsList()
	if err != nil {
		t.Fatalf("FindingsList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("FindingsList returned %d entries, want 2", len(entries))
	}
}

func TestFindingsList_EmptyOnAbsentDir(t *testing.T) {
	c := New(nil, WithFindingsDir(filepath.Join(t.TempDir(), "does-not-exist")))

	entries, err := c.FindingsList()
	if err != nil {
		t.Fatalf("FindingsList: %v", err)
	}
	if entries != nil {
		t.Errorf("FindingsList on an absent dir = %v, want nil", entries)
	}
}

func TestFindingsCleanup_RemovesOnlyOldDirsUnderApply(t *testing.T) {
	dir := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	oldDir := filepath.Join(dir, findingsDirPrefix+"old")
	newDir := filepath.Join(dir, findingsDirPrefix+"new")
	mustMkdir(t, oldDir)
	mustMkdir(t, newDir)

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 1 || removed[0] != oldDir {
		t.Fatalf("FindingsCleanup removed %v, want only %q", removed, oldDir)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("oldDir %q still exists after apply", oldDir)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("newDir %q was removed, want it left alone: %v", newDir, err)
	}
}

func TestFindingsCleanup_DryRunDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	oldDir := filepath.Join(dir, findingsDirPrefix+"old")
	mustMkdir(t, oldDir)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(24*time.Hour, false)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 1 || removed[0] != oldDir {
		t.Fatalf("FindingsCleanup dry-run reported %v, want only %q", removed, oldDir)
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("dry-run deleted %q, want it left alone: %v", oldDir, err)
	}
}

func TestFindingsCleanup_SkipsTopLevelSymlink(t *testing.T) {
	// This proves the observed BEHAVIOR (a symlink at the top level is never
	// removed) — the MECHANISM is the isDir filter in findingsRemovalCandidate,
	// not the containment check: a real os.DirEntry's IsDir() is false for a
	// symlink even when it targets a directory, so this symlink never reaches
	// sandbox.WithinWorkspace at all. TestFindingsRemovalCandidate_* below
	// exercises the containment check directly, with isDir forced true, since
	// this test structurally can't.
	dir := t.TempDir()
	outside := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	link := filepath.Join(dir, findingsDirPrefix+"escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	// Lutimes isn't portable via os.Chtimes (it follows the symlink), so age
	// the link's target instead — if the isDir filter were broken this would
	// still be old enough to match the cutoff.
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsCleanup removed %v, want the top-level symlink skipped", removed)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink %q was removed, want it left alone: %v", link, err)
	}
}

func TestFindingsRemovalCandidate_RejectsPathOutsideFindingsDir(t *testing.T) {
	findingsDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	old := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	// isDir forced true: this is the case a real symlink entry never
	// produces (DirEntry.IsDir() is false for a symlink), so this is the
	// only way to exercise the containment branch directly and prove it
	// independently rejects an escaping path rather than silently passing.
	if got := findingsRemovalCandidate(findingsDir, outside, true, old, cutoff); got {
		t.Errorf("findingsRemovalCandidate(%q, %q, isDir=true, ...) = true, want false (outside findingsDir)", findingsDir, outside)
	}
}

func TestFindingsRemovalCandidate_AcceptsOldContainedDir(t *testing.T) {
	findingsDir := t.TempDir()
	contained := filepath.Join(findingsDir, findingsDirPrefix+"old")
	// sandbox.WithinWorkspace resolves EvalSymlinks on both sides; findingsDir
	// (a real, existing t.TempDir()) resolves through macOS's /var ->
	// /private/var symlink, so contained must exist too, or the two sides
	// resolve asymmetrically and a genuinely-contained path reads as escaping.
	mustMkdir(t, contained)
	old := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	if got := findingsRemovalCandidate(findingsDir, contained, true, old, cutoff); !got {
		t.Errorf("findingsRemovalCandidate(%q, %q, isDir=true, old, ...) = false, want true (contained + old enough)", findingsDir, contained)
	}
}

func TestFindingsCleanup_IgnoresPlainFile(t *testing.T) {
	dir := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	file := filepath.Join(dir, findingsDirPrefix+"stray.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsCleanup removed %v, want the plain file ignored", removed)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("file %q was removed, want it left alone: %v", file, err)
	}
}

func TestFindingsRemove_RemovesGivenPaths(t *testing.T) {
	dir := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	a := filepath.Join(dir, findingsDirPrefix+"a")
	b := filepath.Join(dir, findingsDirPrefix+"b")
	mustMkdir(t, a)
	mustMkdir(t, b)

	removed, err := c.FindingsRemove([]string{a, b})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("FindingsRemove removed %v, want both paths", removed)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%q still exists after FindingsRemove", p)
		}
	}
}

func TestFindingsRemove_SkipsPathThatNoLongerQualifies(t *testing.T) {
	// Simulates the TOCTOU gap between a preview scan and the confirmed
	// apply: one path was already removed out-of-band before FindingsRemove
	// runs. It must be skipped with a note, not treated as an error, and
	// must not cause the other (still-valid) path to be re-scanned or
	// dropped.
	dir := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	gone := filepath.Join(dir, findingsDirPrefix+"gone")
	stillHere := filepath.Join(dir, findingsDirPrefix+"still-here")
	mustMkdir(t, stillHere)
	// gone is never created — stands in for "removed between preview and apply".

	removed, err := c.FindingsRemove([]string{gone, stillHere})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 1 || removed[0] != stillHere {
		t.Fatalf("FindingsRemove removed %v, want only %q", removed, stillHere)
	}
	if _, err := os.Stat(stillHere); !os.IsNotExist(err) {
		t.Errorf("%q still exists, want it removed", stillHere)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}
