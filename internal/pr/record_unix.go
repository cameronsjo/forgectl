//go:build unix

package pr

import (
	"os"

	"golang.org/x/sys/unix"
)

// OpenExclusive creates path as a fresh 0600 file. O_EXCL refuses a
// pre-placed entry, O_NOFOLLOW refuses a pre-placed symlink, and both refuse
// at the open rather than after a write.
func (osRecordFS) OpenExclusive(path string) (recordFile, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openRegularInRoot opens name read-only through a pinned directory handle
// and returns it only if it is a regular file. It is the root-relative
// counterpart of readRecordFile (forgectl#621): every caller Lstats the entry
// first, but that checks the name, not what the open reaches, so a FIFO
// swapped in between would block a plain root.Open under the lifecycle lock.
// O_NONBLOCK makes the open itself unable to block; the descriptor is then
// Fstat'ed, and only a regular file is kept, with O_NONBLOCK cleared so it
// reads exactly as a plain open would. os.Root already refuses a symlink that
// leaves the root.
func openRegularInRoot(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &os.PathError{Op: "open", Path: name, Err: errRecordNotRegular}
	}
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	var nbErr error
	if err := rc.Control(func(fd uintptr) { nbErr = unix.SetNonblock(int(fd), false) }); err != nil {
		nbErr = err
	}
	if nbErr != nil {
		_ = f.Close()
		return nil, &os.PathError{Op: "fcntl", Path: name, Err: nbErr}
	}
	return f, nil
}
