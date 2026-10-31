//go:build unix

package workerwatch

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// errBriefDirSymlink reports a brief dir that is itself a symlink.
var errBriefDirSymlink = errors.New("the brief dir is a symlink")

// openBriefDir opens the brief dir once, refusing a symlink in its last
// component. Every later read is relative to this descriptor, so swapping
// the path for a symlink after the open changes nothing the scan reads.
func openBriefDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		// A symlink fails the open with ELOOP on Linux and ENOTDIR on macOS
		// (O_DIRECTORY is checked first). Lstat only names the refusal; the
		// open already refused.
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, errBriefDirSymlink
			}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openReportAt opens a file in the brief dir for reading without following
// a symlink and without blocking: a worker owns its brief dir and could swap
// a FIFO in between the directory read and the open, which a blocking open
// would wait on forever.
func openReportAt(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0) //nolint:gosec // G115: a descriptor fits an int
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// statAt stats name in the open brief dir without following a symlink.
// Only the type bits the scan reads (regular file or not) are kept.
func statAt(dir *os.File, name string) (fs.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil { //nolint:gosec // G115: a descriptor fits an int
		return nil, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	mode := fs.FileMode(st.Mode & 0o777)
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	default:
		mode |= fs.ModeIrregular
	}
	return statInfo{name: name, size: st.Size, mode: mode, mod: time.Unix(st.Mtim.Unix())}, nil
}
