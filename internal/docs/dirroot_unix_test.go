//go:build unix

package docs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Test plan for openDirRoot (forgectl#798, item 2)
//
//   [x] A FIFO at an indexed root's path fails openPinnedRoot fast, the open
//       every request and every watcher registration makes
//   [x] A FIFO at a canonical root fails openRootDir fast (index time)
//   [x] A FIFO at a canonical root fails ResolveInRoot fast (docs check)
//   [x] A symlinked directory still opens its target
//   [x] os.OpenRoot on a path has one caller in the package, pinDirRoot
//       (dirroot_test.go)
//   [x] A FIFO swapped in for a child directory fails openChildDirRoot and
//       openDirVerified fast, the opens the index walk and request
//       resolution make below a root
//   [x] A plain child directory still opens
//   [x] Root.OpenRoot has one caller in the package, openChildDirRoot
//       (dirroot_test.go)

// fifoInPlaceOf makes a FIFO at path and, at cleanup, opens it for writing
// without blocking, which releases a reader a regressed open left stuck in
// the kernel so the goroutine failsFast abandoned can exit.
func fifoInPlaceOf(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(fd)
		}
	})
}

// failsFast runs open under a deadline and returns its error. A FIFO opened
// without O_NONBLOCK waits for a writer that never comes, so a regressed
// open times out here instead of hanging the suite.
func failsFast(t *testing.T, what string, open func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- open() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s blocked on a FIFO (forgectl#798)", what)
		return nil
	}
}

// A root indexed as a directory, then moved aside and replaced by a FIFO,
// must not hang the open every Index.Open and watcher registration makes.
//
// Mutation that turns it red: open the root in openPinnedRoot with
// os.OpenRoot(r.Path) again (the pre-#798 form). The open blocks, and
// failsFast times out.
func TestOpenPinnedRoot_FIFOAtRootPathFailsFast(t *testing.T) {
	parent := mustCanonicalRoot(t, t.TempDir())
	dir := filepath.Join(parent, "root")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rt, info, err := openRootDir(dir)
	if err != nil {
		t.Fatalf("openRootDir: %v", err)
	}
	_ = rt.Close()
	if err := os.Rename(dir, filepath.Join(parent, "aside")); err != nil {
		t.Fatal(err)
	}
	fifoInPlaceOf(t, dir)

	err = failsFast(t, "openPinnedRoot", func() error {
		rt, err := openPinnedRoot(Root{Path: dir, dirInfo: info})
		if rt != nil {
			_ = rt.Close()
		}
		return err
	})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("openPinnedRoot = %v, want ErrOutsideRoot", err)
	}
}

// Mutation that turns it red: os.OpenRoot(canonical) in openRootDir again.
func TestOpenRootDir_FIFOFailsFast(t *testing.T) {
	fifo := filepath.Join(mustCanonicalRoot(t, t.TempDir()), "root")
	fifoInPlaceOf(t, fifo)
	err := failsFast(t, "openRootDir", func() error {
		rt, _, err := openRootDir(fifo)
		if rt != nil {
			_ = rt.Close()
		}
		return err
	})
	if err == nil {
		t.Error("openRootDir on a FIFO succeeded, want an error")
	}
}

// Mutation that turns it red: os.OpenRoot(root) in ResolveInRoot again.
func TestResolveInRoot_FIFORootFailsFast(t *testing.T) {
	fifo := filepath.Join(mustCanonicalRoot(t, t.TempDir()), "root")
	fifoInPlaceOf(t, fifo)
	err := failsFast(t, "ResolveInRoot", func() error {
		_, err := ResolveInRoot(fifo, "doc.md")
		return err
	})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("ResolveInRoot = %v, want ErrOutsideRoot", err)
	}
}

// The probe follows symlinks as os.OpenRoot does, so a root reached through
// a symlinked ancestor still opens.
//
// Mutation that turns it red: add O_NOFOLLOW to the probe's open flags.
func TestOpenDirRoot_SymlinkedDirOpensTarget(t *testing.T) {
	parent := mustCanonicalRoot(t, t.TempDir())
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	rt, err := openDirRoot(link)
	if err != nil {
		t.Fatalf("openDirRoot(symlink to dir): %v", err)
	}
	defer func() { _ = rt.Close() }()
	got, err := rt.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Error("openDirRoot(symlink) opened something other than its target")
	}
}

// fifoChild returns a root holding a FIFO named "child".
func fifoChild(t *testing.T) *os.Root {
	t.Helper()
	dir := mustCanonicalRoot(t, t.TempDir())
	fifoInPlaceOf(t, filepath.Join(dir, "child"))
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	return parent
}

// Mutation that turns it red: open the child in openChildDirRoot with
// parent.OpenRoot(name) alone, skipping probeChildDir (the pre-#798 form).
// The open blocks for a writer that never comes, and failsFast times out.
func TestOpenChildDirRoot_FIFOChildFailsFast(t *testing.T) {
	parent := fifoChild(t)
	err := failsFast(t, "openChildDirRoot on a FIFO child", func() error {
		child, err := openChildDirRoot(parent, "child")
		if err == nil {
			_ = child.Close()
		}
		return err
	})
	if !errors.Is(err, errNotADirectory) {
		t.Errorf("openChildDirRoot = %v, want errNotADirectory", err)
	}
}

// The FIFO is swapped in after the resolver's Lstat judged a directory, so
// openDirVerified is handed a directory's identity and reaches the open.
//
// Mutation that turns it red: open the child in openDirVerified with
// parent.OpenRoot(name) again. The open blocks, and failsFast times out.
func TestOpenDirVerified_FIFOChildFailsFast(t *testing.T) {
	parent := fifoChild(t)
	checked, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = failsFast(t, "openDirVerified on a FIFO child", func() error {
		sub, err := openDirVerified(parent, "child", checked)
		if err == nil {
			_ = sub.Close()
		}
		return err
	})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("openDirVerified = %v, want ErrOutsideRoot", err)
	}
}

// Mutation that turns it red: invert openChildDirRoot's IsDir check, which
// refuses every real directory.
func TestOpenChildDirRoot_PlainDirOpens(t *testing.T) {
	dir := mustCanonicalRoot(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()
	want, err := parent.Lstat("child")
	if err != nil {
		t.Fatal(err)
	}
	child, err := openChildDirRoot(parent, "child")
	if err != nil {
		t.Fatalf("openChildDirRoot(plain dir): %v", err)
	}
	defer func() { _ = child.Close() }()
	got, err := child.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(want, got) {
		t.Error("openChildDirRoot opened a directory other than the child")
	}
}
