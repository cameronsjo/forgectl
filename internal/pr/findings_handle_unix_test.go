//go:build unix

package pr

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// openChildForTest opens the store child name as openFindingsChild does in
// cleanup, failing the test on any error.
func openChildForTest(t *testing.T, store string, name string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	info, err := root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	child, err := openFindingsChild(root, name, info)
	if err != nil {
		t.Fatalf("openFindingsChild: %v", err)
	}
	t.Cleanup(func() { _ = child.Close() })
	return child
}

// The marker judged is the marker of the directory the handle pinned, even
// once the path names a different directory (forgectl#685). The swap moves the
// checked dir away and puts a dir whose marker names another record at its
// path.
//
// Mutation that turns it red: make openInRootNoFollowNonblock path-based,
// `openNoFollowNonblock(filepath.Join(dir.Name(), name), os.O_RDONLY, 0)` (it
// then reads the swapped-in dir's marker).
func TestReadFindingsMarker_ReadsThroughTheHandleNotThePath(t *testing.T) {
	store := t.TempDir()
	d := markedDir(t, store, "pinned", ownerRecord)
	child := openChildForTest(t, store, filepath.Base(d))

	if err := os.Rename(d, filepath.Join(store, "moved-away")); err != nil {
		t.Fatal(err)
	}
	markedDir(t, store, "pinned", staleOwnerRecord)

	got, err := readFindingsMarker(child)
	if err != nil {
		t.Fatalf("readFindingsMarker: %v", err)
	}
	if got != ownerRecord {
		t.Errorf("readFindingsMarker = %q, want %q from the pinned dir, not the dir now at its path", got, ownerRecord)
	}
}

// A marker that is a symlink to another file in the SAME findings dir is not
// followed either. os.Root follows a symlink that stays inside the root, so a
// reader built on the child handle's own OpenFile would read the linked file,
// find a valid marker for a gone record, and remove the dir: the #659
// regression #685 warns about. Not followed, the open fails with ELOOP and the
// dir is kept.
//
// Mutation that turns it red: make openInRootNoFollowNonblock
// `return dir.OpenFile(name, os.O_RDONLY, 0)`.
func TestFindingsCleanup_InDirSymlinkMarkerIsNotFollowed(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := filepath.Join(store, findingsDirPrefix+"in-dir-link")
	mustMkdirUnmarked(t, d)
	if err := os.WriteFile(filepath.Join(d, "decoy"), []byte(staleOwnerRecord+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("decoy", filepath.Join(d, findingsOwnerMarker)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.FindingsCleanup(t.Context(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantKept(t, d)
}

// openFindingsChild refuses a name that resolves to a directory other than
// the one the caller checked, which is what an in-store symlink swapped in
// after the Lstat would produce.
//
// Mutation that turns it red: drop the os.SameFile comparison from
// openFindingsChild.
func TestOpenFindingsChild_RefusesADifferentDirectory(t *testing.T) {
	store := t.TempDir()
	checked := markedDir(t, store, "checked", ownerRecord)
	other := markedDir(t, store, "other", staleOwnerRecord)
	root, err := os.OpenRoot(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(filepath.Base(checked))
	if err != nil {
		t.Fatal(err)
	}

	child, err := openFindingsChild(root, filepath.Base(other), info)
	if err == nil {
		_ = child.Close()
		t.Fatal("openFindingsChild opened a different directory than the one checked")
	}
	if !errors.Is(err, errFindingsChildMoved) {
		t.Errorf("openFindingsChild error = %v, want errFindingsChildMoved", err)
	}
}

// A store that exists but cannot be opened stops FindingsRemove with ONE
// error before any path is tried, instead of a skip warning per path
// (forgectl#685).
//
// Mutation that turns it red: in FindingsRemove, return (nil, nil) for every
// openFindingsStore error rather than only for a missing store.
func TestFindingsRemove_UnopenableStoreStopsWithOneError(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "store")
	if err := os.WriteFile(store, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := findingsClient(t, store)

	removed, err := c.FindingsRemove(t.Context(), []string{
		filepath.Join(store, findingsDirPrefix+"a"),
		filepath.Join(store, findingsDirPrefix+"b"),
	})
	if err == nil {
		t.Fatal("FindingsRemove over an unopenable store succeeded, want one error")
	}
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing", removed)
	}
	if _, err := c.FindingsCleanup(t.Context(), 0, false); err == nil {
		t.Error("FindingsCleanup over an unopenable store succeeded, want an error")
	}
}
