//go:build unix

package history

import (
	"os"
	"syscall"
)

// openHistory opens the history file without blocking. O_NONBLOCK closes the
// window the pre-open kind check cannot: if the path is swapped to a fifo
// between that stat and this open, a blocking open would hang forever waiting
// for a writer. With O_NONBLOCK the open returns and the caller's re-check
// against the open handle rejects it.
func openHistory(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
