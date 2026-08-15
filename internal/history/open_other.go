//go:build !unix

package history

import "os"

// openHistory opens the history file. There is no non-blocking open to reach
// for off unix, and the fifo case this guards against is a unix construct, so
// the plain open is the whole implementation here.
func openHistory(path string) (*os.File, error) {
	return os.Open(path)
}
