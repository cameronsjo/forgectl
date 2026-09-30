//go:build !unix

package docs

import "os"

// openDirRoot off Unix is os.OpenRoot plus the same identity check the Unix
// build makes. There is no FIFO-at-a-directory-path case to refuse here.
func openDirRoot(path string) (*os.Root, error) {
	want, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return pinDirRoot(path, want)
}
