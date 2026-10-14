//go:build unix

package privdir

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// OpenFile opens name in the directory dirFD pins (a descriptor from Pin or
// OpenChecked) and returns it only after proving it is safe to use: a
// regular file this user owns, with one link, that is the object the name
// still resolves to, at mode. It is the one file check every store under a
// pinned directory goes through (forgectl#1048), so a hardening change here
// reaches all of them.
//
// The order is deliberate. The open never follows a symbolic link
// (O_NOFOLLOW) and never blocks (O_NONBLOCK), so a FIFO or a device swapped
// in for the name cannot hang it, and the refusal below gets control back.
// Every property is then checked against the open descriptor, never by
// consulting the name again, except the identity check, which compares the
// two. The one mutating step, narrowing a broader mode to mode, runs last,
// after type, ownership, link count and identity are proven: a chmod on a
// hardlinked inode would change the mode of a file that is also somebody
// else's, and a chmod on a swapped-in object would land on something that
// was never ours.
//
// flags is the access mode plus any of O_CREAT, O_EXCL, O_APPEND and
// O_TRUNC; O_CLOEXEC, O_NOFOLLOW and O_NONBLOCK are always added. mode is
// the permission a created file gets and an existing one is narrowed to;
// permission bits only.
//
// A name that does not exist (and is not created) is fs.ErrNotExist. Every
// refusal is ErrUnsafeFile, with a terminal-safe reason that names the file.
func OpenFile(dirFD int, name string, flags int, mode fs.FileMode) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if mode.Perm() == 0 || mode != mode.Perm() {
		return nil, unsafeFile("%s: mode must be permission bits only", name)
	}
	want := uint32(mode.Perm())
	fd, err := unix.Openat(dirFD, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, want)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, fs.ErrNotExist
	case errors.Is(err, unix.ELOOP):
		return nil, unsafeFile("%s is a symlink", name)
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EISDIR), errors.Is(err, unix.ENOTDIR):
		return nil, unsafeFile("%s is not a regular file", name)
	case err != nil:
		return nil, unsafeFile("open %s: %s", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if err := verifyFile(dirFD, name, fd, want); err != nil {
		f.Close() //nolint:errcheck,gosec // refusing; nothing was written
		return nil, err
	}
	return f, nil
}

// verifyFile is OpenFile's check of the open descriptor fd against name in
// dirFD.
func verifyFile(dirFD int, name string, fd int, want uint32) error {
	var pinned unix.Stat_t
	if err := unix.Fstat(fd, &pinned); err != nil {
		return unsafeFile("stat %s: %s", name, err)
	}
	switch {
	case pinned.Mode&unix.S_IFMT != unix.S_IFREG:
		return unsafeFile("%s is not a regular file", name)
	case int(pinned.Uid) != os.Geteuid():
		return unsafeFile("%s is owned by another user", name)
	case pinned.Nlink != 1:
		// More than one name for this inode means writing here also writes
		// somewhere forgectl was never pointed at.
		return unsafeFile("%s is hardlinked", name)
	}

	// The name must still resolve to the object that was opened.
	var named unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unsafeFile("stat %s entry: %s", name, err)
	}
	if named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		return unsafeFile("%s entry does not match the opened file", name)
	}

	// Stat_t.Mode is uint16 on Darwin and uint32 on Linux; widen explicitly.
	if uint32(pinned.Mode)&0o7777 == want {
		return nil
	}
	if err := unix.Fchmod(fd, want); err != nil {
		return unsafeFile("restrict %s mode: %s", name, err)
	}
	// The re-check covers the window during the repair, reachable only by a
	// concurrent swap, so no static fixture exercises it. Keep it.
	var rechecked unix.Stat_t
	if err := unix.Fstat(fd, &rechecked); err != nil {
		return unsafeFile("re-stat %s: %s", name, err)
	}
	if rechecked.Dev != pinned.Dev || rechecked.Ino != pinned.Ino || rechecked.Nlink != 1 ||
		uint32(rechecked.Mode)&0o7777 != want || int(rechecked.Uid) != os.Geteuid() {
		return unsafeFile("%s changed identity while being restricted", name)
	}
	return nil
}
