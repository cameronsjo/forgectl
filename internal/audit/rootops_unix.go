//go:build unix

package audit

import (
	"io/fs"
	"os"
	"syscall"
)

// dirOpenFlags refuses a non-directory (O_DIRECTORY) and never blocks on a
// FIFO (O_NONBLOCK).
const dirOpenFlags = os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NONBLOCK

// fileOpenFlags is the sniff's open: read-only, and never blocking on a FIFO
// swapped in after the Lstat.
const fileOpenFlags = os.O_RDONLY | syscall.O_NONBLOCK

// permsMeaningful reports whether a mode's permission bits say who can read
// a file. They do on unix.
const permsMeaningful = true

// ownerUID is the owning uid in an Lstat result, when the platform has one.
func ownerUID(info fs.FileInfo) (int64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return int64(st.Uid), true
}
