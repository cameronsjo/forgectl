//go:build unix

package pr

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// openRepairLogNoFollow opens the repair audit log without following a
// symlink and without blocking in the open (forgectl#614).
//
// O_NOFOLLOW refuses a symlink at the open, so neither a reader nor the
// appender ever reaches a file the log merely points at. O_NONBLOCK is for the
// OPEN only: opening a FIFO for reading blocks until a writer appears, which
// would hang before openRepairLogFile's regular-file check ever runs. It is
// cleared before the descriptor is handed back, so a regular file reads and
// writes exactly as it did through a plain open.
func openRepairLogNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := openNoFollowNonblock(path, flag, perm)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%w: %s is a symlink; refusing to follow it",
			errRepairLogNotRegular, termsafe.QuotePath(path))
	}
	return f, err
}

// openNoFollowNonblock is the open under openRepairLogNoFollow, shared with
// ownerRecordLive's session-record read (forgectl#558); the findings marker
// itself is read with its openat twin, openInRootNoFollowNonblock
// (forgectl#685): O_NOFOLLOW on the final
// component, O_NONBLOCK for the open only (cleared before return), O_CLOEXEC.
// A symlink comes back as a *os.PathError wrapping ELOOP, and the caller still
// owes an Fstat regular-file check on the returned handle.
func openNoFollowNonblock(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := unix.Open(path, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fcntl", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
