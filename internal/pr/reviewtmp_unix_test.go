//go:build unix

package pr

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// strangerOwnsEverything makes every entry read as another user's, the way a
// sibling another local user made under a matching name looks.
func strangerOwnsEverything(t *testing.T) {
	t.Helper()
	orig := reviewSiblingOwner
	reviewSiblingOwner = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { reviewSiblingOwner = orig })
}

// TestRemoveReviewTmpDir_LeavesAnotherUsersEntryAlone: another local user who
// saw the workspace's name can make a sealed dir under a sibling name. Teardown
// must neither chmod nor remove it, and must not fail over it.
func TestRemoveReviewTmpDir_LeavesAnotherUsersEntryAlone(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	planted := plantedSibling(t, ws, reviewSettingsPrefix, "planted")
	if err := os.Mkdir(planted, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(planted, reviewSettingsFile)
	if err := os.WriteFile(keep, []byte("{}"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(planted, 0o500); err != nil { //nolint:gosec // G302: sealed like a settings dir, owner read and search only
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(planted, 0o700) }) //nolint:gosec // G302: test cleanup of its own dir

	strangerOwnsEverything(t)
	if err := removeReviewTmpDir(ws); err != nil {
		t.Errorf("removeReviewTmpDir = %v, want another user's entry skipped without failing teardown", err)
	}
	info, err := os.Lstat(planted)
	if err != nil {
		t.Fatalf("another user's entry was removed: %v", err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Errorf("another user's entry was chmodded to %o", info.Mode().Perm())
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("another user's file went: %v", err)
	}
}

// TestOpenOwnReviewSibling_RefusesWhatForgectlDidNotMake: the one way any
// sibling is opened for a chmod refuses a symlink, a non-directory and
// another user's directory, so no chmod ever lands on what they lead to.
func TestOpenOwnReviewSibling_RefusesWhatForgectlDidNotMake(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openOwnReviewSibling(dir)
	if err != nil {
		t.Fatalf("openOwnReviewSibling of the operator's own dir: %v", err)
	}
	_ = f.Close()
	for _, p := range []string{link, file} {
		if f, err := openOwnReviewSibling(p); !errors.Is(err, errForeignReviewSibling) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("openOwnReviewSibling(%s) = %v, want errForeignReviewSibling", filepath.Base(p), err)
		}
	}
	// A symlink swapped in after the Lstat is refused by the open itself.
	if f, err := openDirNoFollow(link); err == nil {
		_ = f.Close()
		t.Error("openDirNoFollow followed a symlink")
	}
	strangerOwnsEverything(t)
	if f, err := openOwnReviewSibling(dir); !errors.Is(err, errForeignReviewSibling) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("openOwnReviewSibling of another user's dir = %v, want errForeignReviewSibling", err)
	}
}
