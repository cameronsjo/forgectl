//go:build unix

package docs

import "syscall"

// openNonblock makes Index.Open's open return at once on a FIFO instead of
// waiting for a writer. It changes nothing for the regular file Open
// accepts: reads from a regular file never block.
const openNonblock = syscall.O_NONBLOCK
