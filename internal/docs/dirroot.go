package docs

import (
	"errors"
	"io/fs"
	"os"
)

// errDirRootMoved is openDirRoot's refusal of a path that no longer names the
// directory its first open reached.
var errDirRootMoved = errors.New("directory changed between the check and the open")

// pinDirRoot opens path as an os.Root and keeps it only if it is the directory
// want describes. It is the second half of openDirRoot, and this package's one
// caller of os.OpenRoot on a path (TestOpenDirRoot_IsTheOnlyPathRootOpener).
func pinDirRoot(path string, want fs.FileInfo) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	got, err := root.Stat(".")
	if err == nil && !os.SameFile(want, got) {
		err = &os.PathError{Op: "open", Path: path, Err: errDirRootMoved}
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}
