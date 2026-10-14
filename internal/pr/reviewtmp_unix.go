//go:build unix

package pr

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// reviewShortTmpBase is where a review's private temp dir goes when the
// workspace root is too long for it (claudeTmpDirMax): /tmp, which is short
// on every Unix. A seam for tests.
var reviewShortTmpBase = "/tmp"

// reviewSiblingOwner is the uid a review sibling must belong to for forgectl
// to treat it as one it made, a seam so a test can stand in for another user
// without root.
var reviewSiblingOwner = os.Geteuid

// ownedByOperator reports whether info, from an Lstat, belongs to the
// operator. In a shared temp dir, anyone can make an entry under a review
// sibling's name; only the operator's own can be one forgectl made.
func ownedByOperator(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == reviewSiblingOwner()
}

// openDirNoFollow opens path as a directory without following a symlink at
// it (O_NOFOLLOW) and without blocking on a FIFO (O_DIRECTORY|O_NONBLOCK).
func openDirNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
