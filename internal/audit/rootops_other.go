//go:build !unix

package audit

import "os"

// dirOpenFlags is plain read-only where O_DIRECTORY and O_NONBLOCK do not
// exist; such platforms have no FIFO to block on.
const dirOpenFlags = os.O_RDONLY
