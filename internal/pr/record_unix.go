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
