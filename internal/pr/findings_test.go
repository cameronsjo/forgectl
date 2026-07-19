package pr

// Test plan for findings.go
//
// FindingsList (Classification: read-only enumeration over a forgectl-owned dir)
//   [x] Returns one entry per direct-child findings dir
//   [x] Absent findings dir returns (nil, nil), mirroring List's os.IsNotExist handling
//
// FindingsCleanup (Classification: deletion guard — no path parameter, containment-checked)
//   [x] Only removes dirs older than the cutoff
//   [x] Dry-run (apply=false) reports what would be removed and deletes nothing
//   [x] An escaping symlink at the top level is skipped, never removed
//   [x] A plain file at the top level is ignored, never removed

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

func TestFindingsCleanup_SkipsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	c := New(nil, WithFindingsDir(dir))

	link := filepath.Join(dir, findingsDirPrefix+"escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	// Lutimes isn't portable via os.Chtimes (it follows the symlink), so age
	// the link's target instead — if the guard were broken this would still
	// be old enough to match the cutoff.
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsCleanup removed %v, want the escaping symlink skipped", removed)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink %q was removed, want it left alone: %v", link, err)
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

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}
