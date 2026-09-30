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

// errNotADirectory is openChildDirRoot's refusal of a child that is not a
// directory when it is opened, a FIFO or a file swapped in among them.
var errNotADirectory = errors.New("not a directory")

// openChildDirRoot is parent.OpenRoot(name), for name a single component
// directly inside parent, that a FIFO at name cannot block (forgectl#798).
// Root.OpenRoot, like os.OpenRoot, opens with neither O_DIRECTORY nor
// O_NONBLOCK, so a FIFO swapped in for a directory after the caller's Lstat
// would park the walk until a writer appears. It is this package's one
// caller of Root.OpenRoot (TestOpenChildDirRoot_IsTheOnlyChildRootOpener).
//
// The first open is probeChildDir, which refuses a non-directory at once
// and cannot wait for a writer; what it reached must be a directory. The
// second is parent.OpenRoot, kept only if it is the very directory the
// probe reached (os.SameFile), so a directory swapped in between is refused
// rather than pinned. As with openDirRoot, a
// FIFO swapped in during that two-syscall window would still block the
// second open; that needs a same-uid racer with write access to parent
// racing the open itself. Callers still compare the result with their own
// Lstat: this checks only that the open did not change under the probe.
func openChildDirRoot(parent *os.Root, name string) (*os.Root, error) {
	want, err := probeChildDir(parent, name)
	if err != nil {
		return nil, err
	}
	if !want.IsDir() {
		return nil, &os.PathError{Op: "open", Path: name, Err: errNotADirectory}
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	got, err := child.Stat(".")
	if err == nil && !os.SameFile(want, got) {
		err = &os.PathError{Op: "open", Path: name, Err: errDirRootMoved}
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}
