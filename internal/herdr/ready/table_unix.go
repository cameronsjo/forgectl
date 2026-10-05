//go:build unix

package ready

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openOverride opens the override without following a symlink and without
// blocking on a FIFO swapped in after the caller's Lstat, then checks the
// open file itself: a regular file, owned by this user, not writable by group
// or others.
func openOverride(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrTable, path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close() // read-only; the stat error is the one to report
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrTable, path, err)
	}
	if err := checkOverride(path, info); err != nil {
		_ = f.Close() // read-only; the refusal is the one to report
		return nil, nil, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		_ = f.Close() // read-only; the refusal is the one to report
		return nil, nil, fmt.Errorf("%w: %s is owned by uid %d, not this user (%d)", ErrTable, path, st.Uid, os.Geteuid())
	}
	return f, info, nil
}
