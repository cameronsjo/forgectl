//go:build unix

package launch

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// openUsageFileAt opens one fixed name relative to the pinned state leaf and
// proves it is safe before returning it. Writer, reader, and doctor all come
// through here, so a fixture that fools one of them cannot be safe for the
// others.
//
// The order is deliberate. Every property is checked while holding the
// descriptor — never by re-consulting the name — and the one mutating step
// (narrowing a broad mode) runs last, after type, ownership, link count, and
// identity are all proven. A hardlink is refused BEFORE that chmod, because
// chmod on a hardlinked inode changes the mode of a file that is also
// somebody else's.
func openUsageFileAt(stateFD int, name string, flags int, create bool) (*os.File, error) {
	// O_NONBLOCK so a FIFO substituted for the data or lock name cannot hang
	// the open itself — the refusal below only helps if control returns to it.
	openFlags := flags | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		openFlags |= unix.O_CREAT
	}
	fd, err := unix.Openat(stateFD, name, openFlags, usageFileMode)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, errUsageAbsent
	case errors.Is(err, unix.ELOOP):
		return nil, unsafeStore("%s is a symlink", name)
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EISDIR), errors.Is(err, unix.ENOTDIR):
		return nil, unsafeStore("%s is not a regular file", name)
	case err != nil:
		return nil, unsafeStore("open %s: %s", name, err)
	}

	file := os.NewFile(uintptr(fd), name)
	if err := verifyUsageFile(stateFD, name, file); err != nil {
		file.Close() //nolint:errcheck // refusing; nothing was written
		return nil, err
	}
	return file, nil
}

func verifyUsageFile(stateFD int, name string, file *os.File) error {
	fd := int(file.Fd())
	var pinned unix.Stat_t
	if err := unix.Fstat(fd, &pinned); err != nil {
		return unsafeStore("stat %s: %s", name, err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFREG {
		return unsafeStore("%s is not a regular file", name)
	}
	if int(pinned.Uid) != os.Geteuid() {
		return unsafeStore("%s is owned by another user", name)
	}
	if pinned.Nlink != 1 {
		// More than one name for this inode means writing here also writes
		// somewhere forgectl was never pointed at.
		return unsafeStore("%s is hardlinked", name)
	}

	var named unix.Stat_t
	if err := unix.Fstatat(stateFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unsafeStore("stat %s entry: %s", name, err)
	}
	if named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		return unsafeStore("%s entry does not match the opened file", name)
	}

	if pinned.Mode&0o7777 != usageFileMode {
		if err := unix.Fchmod(fd, usageFileMode); err != nil {
			return unsafeStore("restrict %s mode: %s", name, err)
		}
		var rechecked unix.Stat_t
		if err := unix.Fstat(fd, &rechecked); err != nil {
			return unsafeStore("re-stat %s: %s", name, err)
		}
		if rechecked.Ino != pinned.Ino || rechecked.Dev != pinned.Dev || rechecked.Nlink != 1 ||
			rechecked.Mode&0o7777 != usageFileMode || int(rechecked.Uid) != os.Geteuid() {
			return unsafeStore("%s changed identity while being restricted", name)
		}
	}
	return nil
}
