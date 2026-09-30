//go:build unix

package pr

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// openDirRoot is os.OpenRoot for a directory that must never block the open
// (forgectl#792). os.OpenRoot opens its path with neither O_DIRECTORY nor
// O_NONBLOCK, so a FIFO planted at the path blocks it until a writer appears.
// Every pinned open of the sessions dir and of the findings store goes through
// here, so that guarantee does not depend on the caller holding the lifecycle
// lock: the findings cleanup preview reaches ownerRecordLive without it.
//
// There is no way to hand an existing descriptor to os.Root, so this is two
// opens. The first is O_DIRECTORY|O_NONBLOCK, which refuses a FIFO (or any
// non-directory) with ENOTDIR at once and cannot wait for a writer. The second
// is os.OpenRoot itself, and its result is kept only if it is the very
// directory the first open reached (os.SameFile), so a directory swapped in
// between is refused rather than pinned. A FIFO swapped in during that
// two-syscall window would still block the second open; that needs a same-uid
// racer with write access to the parent, the residual the lock's contract
// already accepts. Symlinks along path are followed, as os.OpenRoot follows
// them: a symlinked sessions dir or store still opens its target.
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

// probeChildDir opens name inside parent with O_DIRECTORY and O_NONBLOCK and
// returns what the open reached, the form internal/docs uses. A FIFO or other
// non-directory fails ENOTDIR in the kernel without being opened, so a writer
// blocked on the FIFO is not released, and it comes back wrapping
// errNotADirectory. The open goes through parent.OpenFile against parent's
// own descriptor, so it needs only search permission on parent, as
// parent.OpenRoot does.
//
// parent.OpenFile passes O_NOFOLLOW to the kernel, but on ELOOP it resolves a
// symlink that stays inside parent and opens its target. So the probe then
// takes parent.Lstat(name), which must be a plain directory and the same file
// the open reached: a symlink at name is refused as not a directory rather
// than followed to another store child, and a directory swapped in between is
// refused as moved.
func probeChildDir(parent *os.Root, name string) (fs.FileInfo, error) {
	f, err := parent.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil, &os.PathError{Op: "open", Path: name, Err: errNotADirectory}
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	reached, err := f.Stat()
	if err != nil {
		return nil, err
	}
	at, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !at.IsDir() {
		return nil, &os.PathError{Op: "open", Path: name, Err: errNotADirectory}
	}
	if !os.SameFile(reached, at) {
		return nil, &os.PathError{Op: "open", Path: name, Err: errDirRootMoved}
	}
	return reached, nil
}
