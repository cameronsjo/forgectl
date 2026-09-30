//go:build unix

package docs

import (
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
