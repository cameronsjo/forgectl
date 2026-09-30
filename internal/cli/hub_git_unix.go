//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
)

// openGitMeta opens a .git pointer or HEAD read-only without following a
// symlink and without blocking: a path swapped to a FIFO after readSmallFile's
// Lstat returns here instead of waiting for a writer, and the handle re-check
// then refuses it.
func openGitMeta(path string) (*os.File, error) {
	return os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// ownedByMe reports whether info names a file this process's user owns —
// git's safe.directory default. Unknown ownership is treated as not owned.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
