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
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrConfigNonRegular
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return data, nil
}
