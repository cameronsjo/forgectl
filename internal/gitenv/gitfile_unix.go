//go:build unix

package gitenv

import (
	"os"

	"golang.org/x/sys/unix"
)

// openGitfile opens a gitfile for reading without following a symbolic
// link at it and without blocking. The caller has already found a regular
// file there with Lstat; O_NOFOLLOW and O_NONBLOCK close the window in
// which it is swapped for a link or a FIFO before the open, and the caller
// checks the open handle again.
func openGitfile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0) //nolint:gosec // G304: a gitfile the caller found with Lstat, opened without following a link
}
