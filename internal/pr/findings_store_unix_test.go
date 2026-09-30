//go:build unix

package pr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantStoreRefused asserts err is the categorical store refusal and does not
// echo the store's path.
func wantStoreRefused(t *testing.T, err error, store string) {
	t.Helper()
	if !errors.Is(err, errFindingsStoreUnsafe) {
		t.Fatalf("error = %v, want errFindingsStoreUnsafe", err)
	}
	if strings.Contains(err.Error(), store) || strings.Contains(err.Error(), filepath.Base(store)) {
		t.Errorf("refusal %q echoes the store path %q", err, store)
	}
}

// A group- or world-writable store is refused before cleanup judges anything
// in it, and nothing in it is removed (forgectl#680). Both the preview and
// FindingsRemove refuse.
//
// Mutation that turns it red: drop the Perm()&0o022 check from
// verifyFindingsStore.
func TestFindingsCleanup_WritableStoreIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o707, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			store := t.TempDir()
			c := findingsClient(t, store)
			d := filepath.Join(store, findingsDirPrefix+"old")
			mustMkdir(t, d)
			if err := os.Chmod(store, mode); err != nil { //nolint:gosec // G302: a directory needs its x bits; the broad mode is the case under test
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(store, 0o700) }) //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable

			removed, err := c.FindingsCleanup(t.Context(), 0, true)
			wantStoreRefused(t, err, store)
			if len(removed) != 0 {
				t.Errorf("removed %v, want nothing", removed)
			}
			_, err = c.FindingsRemove(t.Context(), []string{d})
			wantStoreRefused(t, err, store)
			wantKept(t, d)
		})
	}
}

// A store owned by another user is refused (forgectl#680). The seam stands in
// for the other user, since chown needs root.
//
// Mutation that turns it red: drop the uid comparison from
// verifyFindingsStore.
func TestFindingsCleanup_ForeignOwnedStoreIsRefused(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := filepath.Join(store, findingsDirPrefix+"old")
	mustMkdir(t, d)
	orig := findingsStoreOwner
	t.Cleanup(func() { findingsStoreOwner = orig })
	findingsStoreOwner = func() int { return os.Geteuid() + 1 }

	_, err := c.FindingsCleanup(t.Context(), 0, true)
	wantStoreRefused(t, err, store)
	wantKept(t, d)
}

// The check runs on the directory a symlinked store resolves to, so a link to
// a group-writable target is refused. The other half, a private target still
// reclaimed through its link, is TestFindingsCleanup_SymlinkedStoreStillReclaims.
//
// Mutation that turns it red: drop the Perm()&0o022 check from
// verifyFindingsStore.
func TestFindingsCleanup_SymlinkedStoreIsCheckedAtItsTarget(t *testing.T) {
	realDir := t.TempDir()
	store := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(realDir, store); err != nil {
		t.Skipf("Symlink: %v", err)
	}
	c := findingsClient(t, store)
	target := filepath.Join(realDir, findingsDirPrefix+"old")
	mustMkdir(t, target)
	if err := os.Chmod(realDir, 0o775); err != nil { //nolint:gosec // G302: a directory needs its x bits; the broad mode is the case under test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(realDir, 0o700) }) //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable

	_, err := c.FindingsCleanup(t.Context(), 0, true)
	wantStoreRefused(t, err, store)
	wantKept(t, target)
}

// FindingsList makes the same store check as cleanup (forgectl#754): a
// group- or world-writable store, or one owned by another user, is refused
// rather than listed.
//
// Mutation that turns it red: open the store in FindingsList with
// os.OpenRoot instead of openFindingsStore (both stores are listed).
func TestFindingsList_UnsafeStoreIsRefused(t *testing.T) {
	t.Run("writable", func(t *testing.T) {
		store := t.TempDir()
		c := findingsClient(t, store)
		mustMkdir(t, filepath.Join(store, findingsDirPrefix+"old"))
		if err := os.Chmod(store, 0o777); err != nil { //nolint:gosec // G302: a directory needs its x bits; the broad mode is the case under test
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(store, 0o700) }) //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable

		entries, err := c.FindingsList()
		wantStoreRefused(t, err, store)
		if len(entries) != 0 {
			t.Errorf("entries = %v, want none", entries)
		}
	})
	t.Run("foreign owner", func(t *testing.T) {
		store := t.TempDir()
		c := findingsClient(t, store)
		mustMkdir(t, filepath.Join(store, findingsDirPrefix+"old"))
		orig := findingsStoreOwner
		t.Cleanup(func() { findingsStoreOwner = orig })
		findingsStoreOwner = func() int { return os.Geteuid() + 1 }

		_, err := c.FindingsList()
		wantStoreRefused(t, err, store)
	})
}
