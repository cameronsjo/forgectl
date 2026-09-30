//go:build unix

package pr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Test plan for openDirRoot (forgectl#792)
//
//   [x] A FIFO at the sessions-dir path does not hang the findings cleanup
//       preview, which asks ownerRecordLive outside the lifecycle lock, and
//       the dir it cannot classify is kept
//   [x] A FIFO at the findings-store path fails the cleanup fast
//   [x] A symlinked directory still opens its target
//   [x] A path that no longer names the checked directory is refused
//   [x] os.OpenRoot has one caller in the package, pinDirRoot
//       (dirroot_test.go)

// fifoAt makes a FIFO at path and, at cleanup, opens it for writing without
// blocking, which releases a reader a regressed open left stuck in the kernel
// so the goroutine mustFailFast abandoned can exit.
func fifoAt(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(fd)
		}
	})
}

// The marker names a record, so the preview reaches ownerRecordLive, whose
// sessions-dir open is the one outside the lock. A sessions dir that is not a
// directory cannot say the record is gone, so the dir reads as live and is
// kept.
//
// Mutation that turns it red: open the sessions dir in ownerRecordLive with
// os.OpenRoot(c.sessionsDir) again (the pre-#792 form). The open blocks for a
// writer that never comes, and mustFailFast times out.
func TestFindingsCleanup_FIFOSessionsDirFailsFastAndKeepsDir(t *testing.T) {
	store := t.TempDir()
	sessions := filepath.Join(t.TempDir(), "sessions")
	fifoAt(t, sessions)
	c := New(nil, WithFindingsDir(store), WithSessionsDir(sessions))
	d := markedDir(t, store, "fifo-sessions", ownerRecord)

	var preview []string
	err := mustFailFast(t, "FindingsCleanup preview with a FIFO sessions dir", func() error {
		var err error
		preview, err = c.FindingsCleanup(context.Background(), 0, false)
		return err
	})
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if contains558(preview, d) {
		t.Errorf("the preview offered %q, whose owner record cannot be checked; want it kept", d)
	}
	wantKept(t, d)
}

// Mutation that turns it red: open the store in openFindingsStore with
// os.OpenRoot(c.findingsDir) again. The open blocks, and mustFailFast times
// out.
func TestFindingsCleanup_FIFOStoreFailsFast(t *testing.T) {
	store := filepath.Join(t.TempDir(), "findings")
	fifoAt(t, store)
	c := findingsClient(t, store)

	err := mustFailFast(t, "FindingsCleanup with a FIFO store", func() error {
		_, err := c.FindingsCleanup(context.Background(), 0, false)
		return err
	})
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("FindingsCleanup err = %v, want ENOTDIR for a store that is not a directory", err)
	}
}

// Mutation that turns it red: add O_NOFOLLOW to openDirRoot's first open (the
// link is refused with ENOTDIR or ELOOP).
func TestOpenDirRoot_SymlinkedDirOpensItsTarget(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	root, err := openDirRoot(link)
	if err != nil {
		t.Fatalf("openDirRoot(symlink to a dir): %v", err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Stat("probe"); err != nil {
		t.Errorf("the root does not reach the link's target: %v", err)
	}
}

// pinDirRoot is handed the identity of a DIFFERENT directory, which is what
// it sees when the path is swapped for another directory between openDirRoot's
// two opens.
//
// Mutation that turns it red: drop the os.SameFile check in pinDirRoot.
func TestPinDirRoot_RefusesADirectoryThatIsNotTheCheckedOne(t *testing.T) {
	checked, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := pinDirRoot(t.TempDir(), checked)
	if err == nil {
		_ = root.Close()
		t.Fatal("pinDirRoot kept a directory that is not the checked one")
	}
	if !errors.Is(err, errDirRootMoved) {
		t.Errorf("err = %v, want errDirRootMoved", err)
	}
}
