//go:build unix

package pr

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindingsRemove_StoreSwappedAfterCheckStaysInStore replays forgectl#644.
// The seam runs after every check has passed and just before the removal, and
// swaps the store for a symlink to an outside dir that holds a same-named
// findings dir. A path-based RemoveAll re-resolves the store path at that
// moment, follows the link, and deletes the outside dir. The removal through
// the store handle opened before the checks reaches only the original store.
//
// Mutation that turns it red: make the findingsRemoveAll default path-based,
// `func(r *os.Root, n string) error { return os.RemoveAll(filepath.Join(r.Name(), n)) }`
// (the outside dir and its sentinel are deleted).
func TestFindingsRemove_StoreSwappedAfterCheckStaysInStore(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "store")
	moved := filepath.Join(base, "store-moved")
	outside := filepath.Join(base, "outside")
	name := findingsDirPrefix + "swap"
	target := filepath.Join(store, name)
	mustMkdir(t, target)
	decoy := filepath.Join(outside, name)
	mustMkdirUnmarked(t, decoy)
	sentinel := filepath.Join(decoy, "keep-me")
	if err := os.WriteFile(sentinel, []byte("outside the store"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := findingsClient(t, store)

	orig := findingsRemoveAll
	t.Cleanup(func() { findingsRemoveAll = orig })
	swapped := false
	findingsRemoveAll = func(root *os.Root, n string) error {
		if err := os.Rename(store, moved); err != nil {
			t.Fatalf("swap: rename store: %v", err)
		}
		if err := os.Symlink(outside, store); err != nil {
			t.Fatalf("swap: symlink store: %v", err)
		}
		swapped = true
		return orig(root, n)
	}

	if _, err := c.FindingsRemove(t.Context(), []string{target}); err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if !swapped {
		t.Fatal("the removal seam never ran, so the swap was not exercised")
	}
	if _, err := os.Stat(filepath.Clean(sentinel)); err != nil {
		t.Errorf("a file outside the store was removed through the swapped store: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(moved, name)); !os.IsNotExist(err) {
		t.Errorf("the checked dir survived in the original store (err=%v), want it removed through the handle", err)
	}
}
