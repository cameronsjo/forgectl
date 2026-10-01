//go:build unix && !illumos && !solaris

// illumos and solaris are unix but have no syscall.Mkfifo, so this file would
// not compile there. rootops_flags_test.go still pins both flags on them.
// (aix lacks it too, but forgectl does not build for aix: forgectl#956.)

package audit

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRootOps_FIFONeverBlocks pins the listing open: a directory swapped for
// a FIFO between the parent's lstat and the open must fail fast. A plain
// O_RDONLY open of a FIFO blocks until a writer appears, hanging the scan.
func TestRootOps_FIFONeverBlocks(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "swapped"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ops := rootOps(r)
	done := make(chan error, 1)
	go func() {
		_, err := ops.names("swapped")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("listing a FIFO succeeded; want a not-a-directory error")
		}
	case <-time.After(5 * time.Second):
		// Unblock the stuck open so the goroutine can exit.
		if f, err := os.OpenFile(filepath.Clean(filepath.Join(root, "swapped")), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("listing a FIFO blocked: the directory open waits for a writer")
	}
}
