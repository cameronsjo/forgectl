//go:build unix

package pr

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The link points at a regular file holding a VALID marker for a record that
// is GONE, so a followed link would read the dir as stale and remove it. Not
// followed, the open fails with ELOOP, which cannot classify the dir, so it is
// kept (forgectl#659).
//
// Mutations that turn it red: drop O_NOFOLLOW from openNoFollowNonblock in
// repairlog_unix.go (the link is followed to a stale marker); or map
// errFindingsMarkerUnreadable to findingsStale in findingsDirLiveness.
func TestFindingsCleanup_SymlinkMarkerIsNotFollowedAndIsKept(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	target := filepath.Join(t.TempDir(), "valid-marker")
	if err := os.WriteFile(target, []byte(staleOwnerRecord+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(store, findingsDirPrefix+"linked-marker")
	mustMkdirUnmarked(t, d)
	if err := os.Symlink(target, filepath.Join(d, findingsOwnerMarker)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.FindingsCleanup(context.Background(), 0, true); err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantKept(t, d)
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the link's target was touched: %v", err)
	}
}

// Mutation that turns it red: drop O_NONBLOCK from openNoFollowNonblock in
// repairlog_unix.go. Opening the FIFO for reading then blocks for a writer
// that never comes, and mustFailFast times out.
func TestFindingsCleanup_FIFOMarkerFailsFastAndIsStale(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	d := filepath.Join(store, findingsDirPrefix+"fifo-marker")
	mustMkdirUnmarked(t, d)
	if err := syscall.Mkfifo(filepath.Join(d, findingsOwnerMarker), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	err := mustFailFast(t, "FindingsCleanup", func() error {
		_, err := c.FindingsCleanup(context.Background(), 0, true)
		return err
	})
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	wantGone(t, d)
}
