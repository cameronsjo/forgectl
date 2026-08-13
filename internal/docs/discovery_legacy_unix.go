//go:build darwin || linux

package docs

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// openLegacyRecord opens the pre-generation record under the same descriptor
// checks a v1 record gets.
//
// The old format predates every one of these checks, so the file on disk may
// well be fine — but it is still an attacker-writable path carrying a bearer
// token, and the reader is new code. O_NOFOLLOW refuses a symlink swapped in
// for the record; O_NONBLOCK means a FIFO left at that path cannot park
// `docs open` on the open call; and the fstat that follows refuses a
// hardlinked or group-readable file whose contents this directory's
// permissions do not actually govern.
//
// This is the same shape as config.ReadPath (forgectl#274), deliberately not
// reused: ReadPath follows a leaf symlink for config compatibility and checks
// only that the result is regular, which is the right contract for a config
// file and the wrong one for a credential.
func openLegacyRecord(path string) (io.ReadCloser, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		return nil, errUnsafeRecord
	}
	file := os.NewFile(uintptr(fd), path)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close() //nolint:errcheck // already failing
		return nil, errUnsafeRecord
	}
	switch {
	case stat.Mode&unix.S_IFMT != unix.S_IFREG:
	case uint64(stat.Uid) != uint64(os.Geteuid()): //nolint:unconvert // Uid width varies by platform
	case stat.Nlink != 1:
	case stat.Mode&0o077 != 0:
	default:
		return file, nil
	}
	file.Close() //nolint:errcheck // refusing this record
	return nil, errUnsafeRecord
}
