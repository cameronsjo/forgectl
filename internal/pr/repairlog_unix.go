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
	fd, err := unix.Open(path, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s is a symlink; refusing to follow it",
				errRepairLogNotRegular, termsafe.QuotePath(path))
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fcntl", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
