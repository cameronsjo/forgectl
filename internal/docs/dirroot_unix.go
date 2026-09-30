//go:build unix

package docs

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// openDirRoot is os.OpenRoot for a root directory that must never block the
// open (forgectl#798). os.OpenRoot opens its path with neither O_DIRECTORY nor
// O_NONBLOCK, so a FIFO at a root's path, put there after the root was
// indexed or between CanonicalizeRoot and the open, blocks it until a writer
// appears: every request's openPinnedRoot, the watcher's registration, and
// docs check's ResolveInRoot all opened that way.
//
// There is no way to hand an existing descriptor to os.Root, so this is two
// opens, the pattern internal/pr's openDirRoot uses. The first is
// O_DIRECTORY|O_NONBLOCK, which refuses a FIFO (or any non-directory) with
// ENOTDIR at once and cannot wait for a writer. The second is os.OpenRoot
// itself, and its result is kept only if it is the very directory the first
// open reached (os.SameFile), so a directory swapped in between is refused
// rather than pinned. A FIFO swapped in during that two-syscall window would
// still block the second open; that needs a writer to the root's parent
// racing the open itself. Symlinks along path are followed, as os.OpenRoot
// follows them.
func openDirRoot(path string) (*os.Root, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	probe := os.NewFile(uintptr(fd), path)
	defer func() { _ = probe.Close() }()
	want, err := probe.Stat()
	if err != nil {
		return nil, err
	}
	return pinDirRoot(path, want)
}

// probeChildDir opens name inside parent with O_DIRECTORY and O_NONBLOCK
// through parent.OpenFile, which adds O_NOFOLLOW itself, and returns what
// the open reached. A FIFO or other non-directory fails ENOTDIR at once,
// without waiting for a writer, and comes back wrapping errNotADirectory.
// A symlink at name is resolved only within parent, as parent.OpenRoot
// resolves it; the callers' Lstat comparison refuses one swapped in.
func probeChildDir(parent *os.Root, name string) (fs.FileInfo, error) {
	f, err := parent.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil, &os.PathError{Op: "open", Path: name, Err: errNotADirectory}
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}
