//go:build unix

package pr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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
//
// Test plan for openChildDirRoot (forgectl#798)
//
//   [x] A FIFO at a store child's name is refused at once, not waited on
//   [x] openFindingsChild and findingsChildSize, the two store-child opens,
//       fail fast on a FIFO child
//   [x] A plain directory child still opens
//
// Test plan for probeChildDir (forgectl#819)
//
//   [x] A FIFO child is refused without being opened, so a writer blocked
//       on it stays blocked
//   [x] A symlink at a child's name is refused, not followed to the other
//       store child it names, by openChildDirRoot and findingsChildSize
//   [x] A child of a store this user can search but not read still opens
//   [x] Root.OpenRoot has one caller in the package, openChildDirRoot
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

// fifoChild makes a store root holding a FIFO named "child", the state a
// same-uid racer leaves by swapping a checked findings dir for a FIFO.
func fifoChild(t *testing.T) *os.Root {
	t.Helper()
	dir := t.TempDir()
	fifoAt(t, filepath.Join(dir, "child"))
	store, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// Mutation that turns it red: open the child in openChildDirRoot with
// parent.OpenRoot(name) alone (the pre-#798 form). The open blocks for a
// writer that never comes, and mustFailFast times out.
func TestOpenChildDirRoot_FIFOChildFailsFast(t *testing.T) {
	store := fifoChild(t)
	err := mustFailFast(t, "openChildDirRoot on a FIFO child", func() error {
		child, _, err := openChildDirRoot(store, "child")
		if err == nil {
			_ = child.Close()
		}
		return err
	})
	if !errors.Is(err, errNotADirectory) {
		t.Fatalf("openChildDirRoot err = %v, want errNotADirectory", err)
	}
}

// The FIFO is swapped in after the caller's Lstat judged a plain directory,
// so openFindingsChild is handed a directory's identity; the cleanup preview
// makes this open outside the lifecycle lock. findingsChildSize is the list's
// size read of the same child.
//
// Mutations that turn it red: open the child in openFindingsChild, or in
// findingsChildSize, with store.OpenRoot(name) again. Either open blocks, and
// mustFailFast times out.
func TestFindingsChildOpens_FIFOChildFailsFast(t *testing.T) {
	store := fifoChild(t)
	checked, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = mustFailFast(t, "openFindingsChild on a FIFO child", func() error {
		child, err := openFindingsChild(store, "child", checked)
		if err == nil {
			_ = child.Close()
		}
		return err
	})
	if err == nil {
		t.Error("openFindingsChild opened a FIFO child")
	}
	var size int64
	_ = mustFailFast(t, "findingsChildSize on a FIFO child", func() error {
		size = findingsChildSize(store, "child")
		return nil
	})
	if size != 0 {
		t.Errorf("findingsChildSize = %d, want 0 for a child that is not a directory", size)
	}
}

// Mutation that turns it red: invert openChildDirRoot's IsDir check, which
// refuses every real directory.
func TestOpenChildDirRoot_PlainDirOpens(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child", "probe"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	child, _, err := openChildDirRoot(store, "child")
	if err != nil {
		t.Fatalf("openChildDirRoot(plain dir): %v", err)
	}
	defer func() { _ = child.Close() }()
	if _, err := child.Stat("probe"); err != nil {
		t.Errorf("the child root does not reach the directory's contents: %v", err)
	}
	if got := findingsChildSize(store, "child"); got != 3 {
		t.Errorf("findingsChildSize = %d, want 3", got)
	}
}

// A writer opening the FIFO blocks until a reader opens it. The probe must
// refuse the FIFO in the kernel (O_DIRECTORY) rather than open it for
// reading and close it, which would hand that writer a pipe with no reader.
// The sleep gives the writer time to enter its open; if it has not, the
// writer blocks after the probe instead, so a late writer can only let the
// mutation pass, never fail correct code.
//
// Mutation that turns it red: probe with openInRootNoFollowNonblock (the
// pre-#819 form, O_RDONLY|O_NONBLOCK without O_DIRECTORY). Its open of the
// FIFO succeeds as a reader, and the blocked writer is released.
func TestOpenChildDirRoot_FIFOChildDoesNotReleaseABlockedWriter(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "child")
	fifoAt(t, fifo)
	store, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	released := make(chan struct{})
	go func() {
		defer close(released)
		w, err := os.OpenFile(filepath.Clean(fifo), os.O_WRONLY, 0)
		if err == nil {
			_ = w.Close()
		}
	}()
	t.Cleanup(func() {
		// A non-blocking reader releases the writer if it is still waiting,
		// so its goroutine exits before the temp dir is removed.
		if r, err := os.OpenFile(filepath.Clean(fifo), os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			<-released
			_ = r.Close()
		}
	})
	time.Sleep(100 * time.Millisecond)

	err = mustFailFast(t, "openChildDirRoot on a FIFO child", func() error {
		child, _, err := openChildDirRoot(store, "child")
		if err == nil {
			_ = child.Close()
		}
		return err
	})
	if !errors.Is(err, errNotADirectory) {
		t.Fatalf("openChildDirRoot err = %v, want errNotADirectory", err)
	}
	select {
	case <-released:
		t.Fatal("the probe opened the FIFO: a writer blocked on it was released")
	case <-time.After(200 * time.Millisecond):
	}
}

// "child" is a symlink to "other", another directory in the same store.
// parent.OpenFile and parent.OpenRoot both resolve a symlink that stays
// inside the root, so only the probe's Lstat stands between the list's size
// read and the directory the link names.
//
// Mutation that turns it red: drop probeChildDir's Lstat step and return
// what the open reached. The link resolves to "other", a directory, so the
// child opens and findingsChildSize counts other's three bytes.
func TestOpenChildDirRoot_SymlinkChildIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other", "probe"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", filepath.Join(dir, "child")); err != nil {
		t.Fatal(err)
	}
	store, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	child, _, err := openChildDirRoot(store, "child")
	if err == nil {
		_ = child.Close()
		t.Fatal("openChildDirRoot followed a symlink child to another store dir")
	}
	if !errors.Is(err, errNotADirectory) {
		t.Errorf("err = %v, want errNotADirectory", err)
	}
	if got := findingsChildSize(store, "child"); got != 0 {
		t.Errorf("findingsChildSize = %d, want 0 for a symlink child", got)
	}
}

// The store is mode 0300: searchable and writable, not readable. The probe
// opens against the store's own descriptor, as parent.OpenRoot does, so it
// needs no read permission on the store. Root bypasses the mode, so this
// runs only unprivileged (CI's runner user).
//
// Mutation that turns it red (unprivileged only): probe with
// openInRootNoFollowNonblock again, whose dir.Open(".") needs read
// permission on the store and fails EACCES.
func TestOpenChildDirRoot_SearchOnlyStoreOpensChild(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permission")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := os.Chmod(dir, 0o300); err != nil { //nolint:gosec // G302: a search-only directory is the case under test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable
	child, _, err := openChildDirRoot(store, "child")
	if err != nil {
		t.Fatalf("openChildDirRoot in a search-only store: %v", err)
	}
	_ = child.Close()
}
