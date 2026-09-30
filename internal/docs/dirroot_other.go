//go:build !unix

package docs

import "os"

// openDirRoot off Unix is os.OpenRoot plus an os.SameFile check against an
// os.Stat of path, which is weaker than the Unix build's check. On Windows
// os.Stat reads a directory's attributes by path and leaves its file ID
// unfilled; SameFile fills it later by opening the path again. So the
// reference is taken after os.OpenRoot rather than before it, and a
// directory swapped in between the Stat and the OpenRoot passes the check.
// The Unix build takes its reference from an opened descriptor first.
// There is no FIFO-at-a-directory-path case to refuse here.
func openDirRoot(path string) (*os.Root, error) {
	want, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return pinDirRoot(path, want)
}
