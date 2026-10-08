package tasks

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cacheSnap(title string) Snapshot {
	return Snapshot{
		FetchedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Projects:  []Project{{ID: 1, Title: "Inbox"}},
		Tasks:     []Task{{ID: 7, Title: title, ProjectID: 1}},
	}
}

// TestSaveCache_AFailedWriteLeavesThePreviousCacheIntact: the cache is the
// fallback a later network outage reads, so a write that fails partway must
// not be what destroys it. The directory is made unwritable after the first
// save, so a second save cannot create anything beside the file; a save that
// wrote the file in place would still succeed here and replace its content.
func TestSaveCache_AFailedWriteLeavesThePreviousCacheIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the write cannot be made to fail this way")
	}
	dir := filepath.Join(t.TempDir(), "forgectl")
	path := filepath.Join(dir, "tasks-cache.json")
	if err := SaveCache(path, cacheSnap("first")); err != nil {
		t.Fatalf("first SaveCache: %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: a path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: a directory, read-only on purpose so nothing can be created in it
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a directory, restored so t.TempDir can remove it

	if err := SaveCache(path, cacheSnap("second")); err == nil {
		t.Fatal("SaveCache into a directory nothing can be created in = nil, want an error")
	}
	after, err := os.ReadFile(path) //nolint:gosec // G304: a path under t.TempDir
	if err != nil {
		t.Fatalf("the previous cache is gone after a failed write: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a failed write changed the previous cache:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := LoadCache(path); err != nil {
		t.Fatalf("the previous cache no longer decodes: %v", err)
	}
}

// TestSaveCache_AFailedRenameLeavesNoTemporaryFile: the target is a directory,
// so the final rename fails after the temporary file was written. That file
// must not be left behind for every later failure to add another.
func TestSaveCache_AFailedRenameLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks-cache.json")
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveCache(path, cacheSnap("x")); err == nil {
		t.Fatal("SaveCache over a non-empty directory = nil, want an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "tasks-cache.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the cache directory holds %v after a failed save, want only the original entry", names)
	}
}

// TestSaveCache_TheFileIsOwnerOnly: a cache left by an older build, or created
// by hand, may be readable by group or other. The saved file is a new file, so
// it is 0600 whatever was there before.
func TestSaveCache_TheFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks-cache.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the wide mode an older cache may have, which the save must not keep
		t.Fatal(err)
	}
	if err := SaveCache(path, cacheSnap("x")); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the cache is mode %04o, want 0600", mode)
	}
	snap, err := LoadCache(path)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].Title != "x" {
		t.Fatalf("the saved cache does not hold the snapshot: %+v", snap)
	}
}
