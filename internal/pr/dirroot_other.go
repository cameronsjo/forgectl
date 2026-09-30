//go:build !unix

package pr

import "os"

// openDirRoot off Unix is os.OpenRoot plus the same identity check the Unix
// build makes. There is no named-pipe-at-a-directory-path case to refuse here,
// and no shipped binary runs off Unix (goreleaser builds linux and darwin).
func openDirRoot(path string) (*os.Root, error) {
	want, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return pinDirRoot(path, want)
}
