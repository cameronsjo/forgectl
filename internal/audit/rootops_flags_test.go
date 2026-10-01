//go:build unix

package audit

import (
	"os"
	"syscall"
	"testing"
)

// TestDirOpenFlags_RefusesNonDirectoryWithoutBlocking pins both halves of the
// listing open's flags. TestRootOps_FIFONeverBlocks cannot: on Linux either flag
// alone makes a FIFO open fail fast (O_DIRECTORY refuses it, O_NONBLOCK
// returns before a writer appears and Readdirnames then fails), so it stays
// green with one of them removed.
//
// Mutation that turns it red: drop syscall.O_DIRECTORY or syscall.O_NONBLOCK
// from dirOpenFlags in rootops_unix.go.
func TestDirOpenFlags_RefusesNonDirectoryWithoutBlocking(t *testing.T) {
	if dirOpenFlags&syscall.O_DIRECTORY == 0 {
		t.Error("dirOpenFlags lacks O_DIRECTORY: a non-directory swapped in mid-scan would open")
	}
	if dirOpenFlags&syscall.O_NONBLOCK == 0 {
		t.Error("dirOpenFlags lacks O_NONBLOCK: a FIFO swapped in mid-scan could block the open")
	}
	if dirOpenFlags&(os.O_WRONLY|os.O_RDWR) != 0 {
		t.Error("dirOpenFlags opens for writing; the scan is read-only")
	}
}

// TestFileOpenFlags_ReadOnlyNonBlocking pins the sniff's open: read-only,
// and never blocking on a FIFO swapped in after the Lstat.
//
// Mutation that turns it red: drop syscall.O_NONBLOCK from fileOpenFlags in
// rootops_unix.go, or add os.O_RDWR.
func TestFileOpenFlags_ReadOnlyNonBlocking(t *testing.T) {
	if fileOpenFlags&syscall.O_NONBLOCK == 0 {
		t.Error("fileOpenFlags lacks O_NONBLOCK: a FIFO swapped in mid-scan could block the sniff")
	}
	if fileOpenFlags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		t.Error("fileOpenFlags can write or create; the scan is read-only")
	}
}
