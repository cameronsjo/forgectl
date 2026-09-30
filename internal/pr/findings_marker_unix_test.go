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
// Mutations that turn it red: drop O_NOFOLLOW from openInRootNoFollowNonblock
// in findings_open_unix.go (the link is followed to a stale marker); or map
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

// Mutation that turns it red: drop O_NONBLOCK from openInRootNoFollowNonblock
// in findings_open_unix.go. Opening the FIFO for reading then blocks for a writer
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

// The marker write goes through a handle on the findings dir opened from the
// store (forgectl#754). A findings dir swapped for a symlink to a dir outside
// the store after MkdirTemp gets no marker, and neither does the link's
// target: the write is refused rather than following the link.
//
// Mutations that turn it red: drop the plain-directory check and open the
// dir by its path, os.OpenRoot(findingsDir), instead of openFindingsChild
// (the link is followed and the marker lands in the outside dir). The
// origin/main writer, a c.fs.OpenExclusive and os.Link on joined paths, fails
// it the same way: O_NOFOLLOW guards only the last component.
func TestWriteFindingsMarker_SymlinkedDirIsRefused(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	outside := t.TempDir()
	d := filepath.Join(store, findingsDirPrefix+"swapped")
	if err := os.Symlink(outside, d); err != nil {
		t.Fatal(err)
	}

	if err := c.writeFindingsMarker(d, filepath.Join(c.sessionsDir, ownerRecord)); err == nil {
		t.Fatal("writeFindingsMarker through a symlinked findings dir succeeded, want a refusal")
	}
	for _, name := range []string{findingsOwnerMarker, findingsMarkerTemp} {
		if _, err := os.Lstat(filepath.Join(outside, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written into the link's target (lstat err %v)", name, err)
		}
	}
}

// The owner record is opened through a handle on the sessions dir, with the
// marker reader's no-follow open, not by a joined path (forgectl#754).
//
// Mutation that turns it red: open the record in ownerRecordLive with
// openNoFollowNonblock(filepath.Join(c.sessionsDir, name), os.O_RDONLY, 0)
// again (the seam is never called).
func TestOwnerRecordLive_OpensThroughSessionsHandle(t *testing.T) {
	c := findingsClient(t, t.TempDir())
	liveRecord(t, c, ownerRecord, `{"local":true}`)
	orig := openOwnerRecord
	t.Cleanup(func() { openOwnerRecord = orig })
	var gotRoot, gotName string
	openOwnerRecord = func(dir *os.Root, name string) (*os.File, error) {
		gotRoot, gotName = dir.Name(), name
		return orig(dir, name)
	}

	if !c.ownerRecordLive(ownerRecord) {
		t.Error("ownerRecordLive = false for an existing local record")
	}
	if gotRoot != c.sessionsDir || gotName != ownerRecord {
		t.Errorf("record opened as (%q, %q), want (%q, %q) through the sessions handle",
			gotRoot, gotName, c.sessionsDir, ownerRecord)
	}
	if c.ownerRecordLive(staleOwnerRecord) {
		t.Error("ownerRecordLive = true for a record that does not exist")
	}
}
