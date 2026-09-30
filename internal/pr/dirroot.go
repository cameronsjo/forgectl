package pr

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
// direct caller of os.OpenRoot (TestOpenDirRoot_IsTheOnlyPathRootOpener).
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
// directory, a FIFO among them.
var errNotADirectory = errors.New("not a directory")

// openChildDirRoot is openDirRoot for name, a single component directly
// inside parent: parent.OpenRoot for a directory that must never block the
// open (forgectl#798). Root.OpenRoot, like os.OpenRoot, opens with neither
// O_DIRECTORY nor O_NONBLOCK, so a FIFO swapped in for a store child blocks
// it until a writer appears, and the findings cleanup preview opens store
// children outside the lifecycle lock.
//
// The first open is openInRootNoFollowNonblock, an openat against parent's
// own descriptor that cannot wait for a writer and does not follow a symlink
// at name; its fstat must say directory. The second is parent.OpenRoot, kept
// only if it is the very directory the probe reached (os.SameFile), so a
// directory swapped in between is refused rather than pinned. As with
// openDirRoot, a FIFO swapped in during that two-syscall window would still
// block the second open; that needs a same-uid racer inside a store already
// verified private.
func openChildDirRoot(parent *os.Root, name string) (*os.Root, error) {
	probe, err := openInRootNoFollowNonblock(parent, name)
	if err != nil {
		return nil, err
	}
	want, err := probe.Stat()
	_ = probe.Close()
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
