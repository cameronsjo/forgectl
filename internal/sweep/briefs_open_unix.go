//go:build unix

package sweep

import (
	"os"
	"syscall"
)

// openNoBlock opens path read-only without following a symlink and without
// blocking, so a FIFO swapped in after the Lstat cannot hang the sweep; the
// caller's SameFile check then refuses it.
func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: path is under a validated worker directory
}
