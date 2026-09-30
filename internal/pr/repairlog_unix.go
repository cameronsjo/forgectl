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
//
// Some refusals arrive as an errno from the open rather than as a file type
// from the Fstat after it, and each is named for what it is (forgectl#621):
//
//   - ELOOP is a symlink as the log itself only when an Lstat of the path says
//     so. The same errno comes back for a symlink loop in a directory ABOVE
//     the log, and that error names the directory, not the log. (FreeBSD and
//     NetBSD report O_NOFOLLOW on a symlink as EMLINK and EFTYPE; no shipped
//     binary runs there, and they get the plain open error.)
//   - ENXIO is a socket (and EISDIR a directory opened for writing): the
//     kernel refuses the open before the Fstat can name the type, and
//     "no such device or address" would not tell the operator what is wrong.
//
// The Lstat only names the refusal; nothing is opened on its word.
func openRepairLogNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := openNoFollowNonblock(path, flag, perm)
	if err == nil {
		return f, nil
	}
	switch {
	case errors.Is(err, unix.ELOOP):
		if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink: %w; refusing to follow it",
				termsafe.QuotePath(path), errRepairLogNotRegular)
		}
		return nil, fmt.Errorf("a directory above %s loops back through symlinks: %w",
			termsafe.QuotePath(path), unix.ELOOP)
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EISDIR):
		if info, lerr := os.Lstat(path); lerr == nil && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is a %s: %w; refusing it",
				termsafe.QuotePath(path), fileKind(info.Mode()), errRepairLogNotRegular)
		}
	}
	return nil, err
}

// openNoFollowNonblock is the open under openRepairLogNoFollow, shared with the
// findings owner-marker reader (forgectl#558): O_NOFOLLOW on the final
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
