//go:build unix

package cli

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openDocsTokenFile(path string) (openedDocsTokenFile, error) {
	displayPath := safeDocsTokenPath(path)
	// O_NONBLOCK prevents a hostile FIFO from hanging the process before fstat
	// can reject it. It has no effect on the required regular-file descriptor.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open token file %s: %w", displayPath, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd) //nolint:errcheck // fd ownership was not transferred
		return nil, fmt.Errorf("open token file %s: invalid descriptor", displayPath)
	}

	info, statErr := file.Stat()
	if statErr != nil {
		// The raw path: wrapDocsTokenDescriptorError renders it itself, and
		// QuotePath is not idempotent.
		return nil, closeInvalidDocsTokenFile(file, wrapDocsTokenDescriptorError("inspect", path, statErr))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, closeInvalidDocsTokenFile(file, fmt.Errorf("inspect token file %s: unsupported descriptor metadata", displayPath))
	}
	if !info.Mode().IsRegular() {
		return nil, closeInvalidDocsTokenFile(file, fmt.Errorf("token file is not a regular file: %s", displayPath))
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return nil, closeInvalidDocsTokenFile(file, fmt.Errorf("token file is not owned by the current user: %s", displayPath))
	}
	if stat.Nlink != 1 {
		return nil, closeInvalidDocsTokenFile(file, fmt.Errorf("token file must have exactly one link: %s", displayPath))
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, closeInvalidDocsTokenFile(file, fmt.Errorf("token file permissions allow group or other access: %s", displayPath))
	}
	return file, nil
}
