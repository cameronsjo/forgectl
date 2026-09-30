//go:build !unix

package cli

import (
	"os"
	"path/filepath"
)

// openGitMeta opens a .git pointer or HEAD read-only. Off unix there is no
// O_NOFOLLOW or O_NONBLOCK to reach for; readSmallFile's Lstat and handle
// re-check still refuse a non-regular file, but the open itself can block on
// a named pipe swapped in between — the same recorded residual as
// internal/history's openHistory.
func openGitMeta(path string) (*os.File, error) {
	return os.Open(filepath.Clean(path))
}

// ownedByMe has no uid to compare off unix; ownership is not checked there.
func ownedByMe(os.FileInfo) bool { return true }
