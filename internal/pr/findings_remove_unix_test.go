//go:build unix

package pr

import (
	"errors"
	"io/fs"
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
// `func(r, _ *os.Root, n string, _ fs.FileInfo) error { return os.RemoveAll(filepath.Join(r.Name(), n)) }`
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
	findingsRemoveAll = func(root, child *os.Root, n string, judged fs.FileInfo) error {
		if err := os.Rename(store, moved); err != nil {
			t.Fatalf("swap: rename store: %v", err)
		}
		if err := os.Symlink(outside, store); err != nil {
			t.Fatalf("swap: symlink store: %v", err)
		}
		swapped = true
		return orig(root, child, n, judged)
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

// TestFindingsRemove_LiveDirRenamedOntoJudgedNameSurvives replays the #753
// review's race. After the judged dir passed every check, the seam parks it
// under another name and renames a LIVE review's dir onto its name, then lets
// the removal run. A removal by name deletes the live dir and logs it as
// reclaimed. Emptying through the judged dir's own handle reaches only the
// parked dir, and the rmdir on the name refuses the live dir, which still
// holds its marker.
//
// Mutation that turns it red: make removeJudgedFindingsDir
// `return store.RemoveAll(name)` (the live dir and its findings are deleted).
func TestFindingsRemove_LiveDirRenamedOntoJudgedNameSurvives(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	liveRecord(t, c, ownerRecord, `{"local":true}`)
	judged := markedDir(t, store, "judged", staleOwnerRecord)
	live := markedDir(t, store, "live", ownerRecord)
	liveFindings := filepath.Join(live, "findings.md")
	if err := os.WriteFile(liveFindings, []byte("a running review's findings"), 0o600); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(store, "parked")

	orig := findingsRemoveAll
	t.Cleanup(func() { findingsRemoveAll = orig })
	swapped := false
	findingsRemoveAll = func(root, child *os.Root, n string, info fs.FileInfo) error {
		if err := os.Rename(judged, parked); err != nil {
			t.Fatalf("park the judged dir: %v", err)
		}
		if err := os.Rename(live, judged); err != nil {
			t.Fatalf("rename the live dir onto the judged name: %v", err)
		}
		swapped = true
		return orig(root, child, n, info)
	}

	removed, err := c.FindingsRemove(t.Context(), []string{judged})
	if !swapped {
		t.Fatal("the removal seam never ran, so the race was not exercised")
	}
	if !errors.Is(err, errFindingsDirSwapped) {
		t.Errorf("FindingsRemove error = %v, want errFindingsDirSwapped", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v, want nothing reported removed", removed)
	}
	moved := filepath.Join(judged, "findings.md")
	if got, rerr := os.ReadFile(filepath.Clean(moved)); rerr != nil || string(got) != "a running review's findings" {
		t.Errorf("the live dir renamed onto the judged name lost its findings (read %q, err %v)", got, rerr)
	}
	if _, err := os.Stat(filepath.Join(judged, findingsOwnerMarker)); err != nil {
		t.Errorf("the live dir's owner marker is gone: %v", err)
	}
}

// An EMPTY dir renamed onto the judged name is not removed either: rmdir
// would take it, so the identity check is what refuses it.
//
// Mutation that turns it red: drop the os.SameFile check from
// removeJudgedFindingsDir (the swapped-in dir is removed).
func TestFindingsRemove_EmptyDirRenamedOntoJudgedNameSurvives(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	judged := markedDir(t, store, "judged", staleOwnerRecord)
	other := filepath.Join(store, "someone-elses-empty-dir")
	mustMkdirUnmarked(t, other)

	orig := findingsRemoveAll
	t.Cleanup(func() { findingsRemoveAll = orig })
	findingsRemoveAll = func(root, child *os.Root, n string, info fs.FileInfo) error {
		if err := os.Rename(judged, filepath.Join(store, "parked")); err != nil {
			t.Fatalf("park the judged dir: %v", err)
		}
		if err := os.Rename(other, judged); err != nil {
			t.Fatalf("rename the empty dir onto the judged name: %v", err)
		}
		return orig(root, child, n, info)
	}

	if _, err := c.FindingsRemove(t.Context(), []string{judged}); !errors.Is(err, errFindingsDirSwapped) {
		t.Errorf("FindingsRemove error = %v, want errFindingsDirSwapped", err)
	}
	wantKept(t, judged)
}
