//go:build unix

package audit

import (
	"os"
	"syscall"
)

// dirOpenFlags refuses a non-directory (O_DIRECTORY) and never blocks on a
// FIFO (O_NONBLOCK).
const dirOpenFlags = os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NONBLOCK
