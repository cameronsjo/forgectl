//go:build unix

package config

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ReadPath follows a leaf config symlink for compatibility, but opens it
// nonblocking and reads only a regular descriptor. This prevents a FIFO,
// socket, directory, or device from blocking startup or a lock-held writer.
func ReadPath(path string) ([]byte, error) {
	data, _, err := readPathTrust(path)
	return data, err
}

// readPathTrust is ReadPath plus a verdict on who could have written the file,
// judged on the descriptor that was read, so a swap after the open changes
// neither the bytes nor the verdict (see execChoiceVerdict). trust is nil when
// the file is acceptable for a setting that picks an executable to run.
func readPathTrust(path string) (data []byte, trust, err error) {
	return readPathTrustAs(path, os.Geteuid())
}

func readPathTrustAs(path string, uid int) (data []byte, trust, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, ErrConfigNonRegular
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	return data, execChoiceVerdict(path, info, uid), nil
}
