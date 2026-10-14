//go:build !unix

package pr

import (
	"io/fs"
	"os"
)

// ownedByOperator off Unix accepts every entry: there is no uid to compare,
// and no shipped binary runs off Unix (goreleaser builds linux and darwin).
func ownedByOperator(fs.FileInfo) bool { return true }

// openDirNoFollow off Unix refuses a symlink at path by its Lstat and then
// opens it; openOwnReviewSibling's os.SameFile check catches a swap between.
func openDirNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "open", Path: path, Err: errNotADirectory}
	}
	return os.Open(path) //nolint:gosec // G304: a review sibling path forgectl derived
}
