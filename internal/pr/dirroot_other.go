//go:build !unix

package pr

import (
	"io/fs"
	"os"
)

// openDirRoot off Unix is os.OpenRoot plus an os.SameFile check against an
// os.Stat of path, which is weaker than the Unix build's check. On Windows
// os.Stat reads a directory's attributes by path and leaves its file ID
// unfilled; SameFile fills it later by opening the path again. So the
// reference is taken after os.OpenRoot rather than before it, and a
// directory swapped in between the Stat and the OpenRoot passes the check.
// The Unix build takes its reference from an opened descriptor first.
// There is no FIFO-at-a-directory-path case to refuse here, and no shipped
// binary runs off Unix (goreleaser builds linux and darwin).
func openDirRoot(path string) (*os.Root, error) {
	want, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return pinDirRoot(path, want)
}

// probeChildDir off Unix is parent.Lstat(name), which opens nothing a
// writer could hold up and does not follow a symlink at name: a symlink's
// Lstat is not a directory, so openChildDirRoot refuses it. os.Root's Lstat
// reads through an opened handle, so on Windows the file ID is filled when
// the probe runs, not later by path.
func probeChildDir(parent *os.Root, name string) (fs.FileInfo, error) {
	return parent.Lstat(name)
}
