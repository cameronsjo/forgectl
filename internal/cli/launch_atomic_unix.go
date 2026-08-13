//go:build unix

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func syncDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errConfigDestinationNonRegular
	}
	return f, nil
}

func pinConfigTemp(f *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}

func ensureNormalConfigParent(path string, ops configParentOps) ([]string, error) {
	return ensureConfigParentDurable(path, ops)
}
