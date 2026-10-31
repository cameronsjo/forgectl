//go:build unix

package workerwatch

import (
	"os"
	"syscall"
)

// openReport opens a brief-dir file for reading without following a symlink
// and without blocking: a worker owns its brief dir and could swap a FIFO in
// between the directory read and the open, which a blocking open would wait
// on forever.
func openReport(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: a name reportNameRe matched, inside the brief dir
}
