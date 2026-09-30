//go:build unix

package pr

import (
	"os"

	"golang.org/x/sys/unix"
)

// openInRootNoFollowNonblock opens name, a single component, inside dir with
// openNoFollowNonblock's flags: O_NOFOLLOW on name, O_NONBLOCK for the open
// only (cleared before return), O_CLOEXEC. It is an openat against dir's own
// descriptor, so the result is a child of the directory dir pinned, never of
// whatever a path names now (forgectl#685).
//
// It does not go through dir.OpenFile, because os.Root resolves a symlink
// that stays inside the root: a marker linked to another file in the same
// dir would be followed, which is the #659 regression this reader exists to
// prevent. A symlink comes back as a *os.PathError wrapping ELOOP, and the
// caller still owes an Fstat regular-file check on the returned handle.
func openInRootNoFollowNonblock(dir *os.Root, name string) (*os.File, error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	fd, err := unix.Openat(int(d.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fcntl", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}
