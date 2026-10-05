//go:build !unix

package audit

import (
	"io/fs"
	"os"
)

// dirOpenFlags is plain read-only where O_DIRECTORY and O_NONBLOCK do not
// exist; such platforms have no FIFO to block on.
const dirOpenFlags = os.O_RDONLY

// fileOpenFlags is the sniff's open: read-only.
const fileOpenFlags = os.O_RDONLY

// permsMeaningful is false where a mode's permission bits are synthesized
// (Windows reports 0666 for every writable file), so loose is never claimed
// from them.
const permsMeaningful = false

// ownerUID has no uid to report off unix.
func ownerUID(fs.FileInfo) (int64, bool) { return 0, false }
