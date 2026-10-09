//go:build unix

package config

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// maxMergeConfigBytes caps the config file the merge policy reads.
const maxMergeConfigBytes = 1 << 20

// readMergeConfig opens path without following a symlink and reads it only
// when it is a regular file owned by check.UID with permission bits exactly
// 0600. The checks run on the open descriptor, so a swap after them changes
// nothing read.
func readMergeConfig(path string, check MergeFileCheck) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink; the merge policy reads only a regular file", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck // read-only descriptor
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if int(st.Uid) != check.UID {
		return nil, fmt.Errorf("%s is owned by uid %d, expected %d", path, st.Uid, check.UID)
	}
	if perm := st.Mode & 0o7777; perm != 0o600 {
		return nil, fmt.Errorf("%s has mode %04o, expected 0600", path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMergeConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxMergeConfigBytes {
		return nil, fmt.Errorf("%s is over %d bytes", path, maxMergeConfigBytes)
	}
	return data, nil
}

// LocalMergeFileCheck is the check for this process's user.
func LocalMergeFileCheck() MergeFileCheck { return MergeFileCheck{UID: os.Geteuid()} }
