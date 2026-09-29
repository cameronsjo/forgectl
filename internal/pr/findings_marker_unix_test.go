//go:build unix

package pr

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The link points at a regular file holding a VALID marker for a live local
// record, so a followed link would keep the dir.
//
// Mutation that turns it red: drop O_NOFOLLOW from openNoFollowNonblock in
// repairlog_unix.go.
func TestFindingsCleanup_SymlinkMarkerIsNotFollowed(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	liveRecord(t, c, ownerRecord, `{"local":true}`)
	target := filepath.Join(t.TempDir(), "valid-marker")
	if err := os.WriteFile(target, []byte(ownerRecord+"\n"), 0o600); err != nil {
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
	wantGone(t, d)
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
