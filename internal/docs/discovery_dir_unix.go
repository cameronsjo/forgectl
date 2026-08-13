//go:build darwin || linux

package docs

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Fixed refusals for a discovery directory or record that fails its safety
// checks. None of them names the path: the leaf holds names any process running
// as this user can choose.
var (
	errUnsafeDirOwner = errors.New("the docs discovery directory is not owned by this user")
	errUnsafeDirMode  = errors.New("the docs discovery directory is readable by other users")
	errUnsafeDirType  = errors.New("the docs discovery directory path is not a directory")
	errUnsafeRecord   = errors.New("a docs discovery record failed its ownership or type checks")
)

// unixDir is a discovery directory pinned to one open descriptor.
//
// Every create, open, remove, and rename below is relative to that descriptor
// rather than to the path it was opened from. That is what makes lease removal
// honest: between publication and shutdown the path can be replaced with a
// symlink to somewhere else, and a path-relative unlink would follow it. A
// descriptor-relative one cannot leave the directory it was handed.
type unixDir struct {
	file *os.File
	fd   int
}

// openDiscoveryDir opens (and optionally creates) the leaf, refusing anything
// that is not a private directory this user owns.
//
// Parent directories are deliberately not audited. The config directory's own
// safety is the config package's business, and a parent symlink is an ordinary
// setup (a home directory on another volume). The leaf and its children are the
// security boundary this change establishes, because they are what carries
// bearer tokens.
func openDiscoveryDir(path string, create bool) (discoveryDir, error) {
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
			// A symlink or a plain file where the leaf should be. Following
			// either would put records somewhere this code never checked.
			return nil, errUnsafeDirType
		}
		return nil, err
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd) //nolint:errcheck // already failing
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd) //nolint:errcheck // refusing this handle
		return nil, errUnsafeDirType
	}
	if uint64(stat.Uid) != uint64(os.Geteuid()) { //nolint:unconvert // Uid width varies by platform
		unix.Close(fd) //nolint:errcheck // refusing this handle
		return nil, errUnsafeDirOwner
	}
	if stat.Mode&0o077 != 0 {
		unix.Close(fd) //nolint:errcheck // refusing this handle
		return nil, errUnsafeDirMode
	}

	return &unixDir{file: os.NewFile(uintptr(fd), path), fd: fd}, nil
}

// CreateTemp exclusively creates a hidden temp inside the pinned directory.
//
// O_EXCL on a random name is the create-side counterpart of the no-replace
// install: this publisher writes only into a name nothing else holds, so a temp
// can never be another process's file.
func (d *unixDir) CreateTemp() (discoveryWriteFile, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		suffix, err := randomTempSuffix()
		if err != nil {
			return nil, "", err
		}
		name := ".tmp-" + suffix
		fd, err := unix.Openat(d.fd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return nil, "", err
		}
		return &unixFile{file: os.NewFile(uintptr(fd), name), fd: fd}, name, nil
	}
	return nil, "", errors.New("could not create a private docs discovery temp file")
}

// OpenRecord opens a candidate record and refuses it unless it is a plain,
// private, single-linked file this user owns.
//
// The checks run on the DESCRIPTOR, after the open, not on a stat of the path
// before it. Checking the path first and opening second is the classic
// time-of-check gap: the name can be swapped in between. O_NOFOLLOW plus an
// fstat closes it, and O_NONBLOCK means a FIFO left in the directory cannot
// park `docs open` indefinitely on the open itself.
func (d *unixDir) OpenRecord(name string) (io.ReadCloser, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close() //nolint:errcheck // already failing
		return nil, err
	}
	switch {
	case stat.Mode&unix.S_IFMT != unix.S_IFREG:
		// A directory, device, socket, or FIFO wearing a record's name.
	case uint64(stat.Uid) != uint64(os.Geteuid()): //nolint:unconvert // Uid width varies by platform
	case stat.Nlink != 1:
		// A hardlink means the bytes are reachable under another name, so the
		// record's contents are not governed by this directory's permissions.
	case stat.Mode&0o077 != 0:
	default:
		return file, nil
	}
	file.Close() //nolint:errcheck // refusing this record
	return nil, errUnsafeRecord
}

func (d *unixDir) ReadDir(n int) ([]fs.DirEntry, error) {
	return d.file.ReadDir(n)
}

// InstallNoReplace makes the record visible under its final name.
//
// The warning return is always nil here; it exists for the portable
// implementation, where publication is a link followed by a temp removal that
// can fail after the record is already complete and usable.
func (d *unixDir) InstallNoReplace(tempName, finalName string) (error, error) {
	if err := installNoReplace(d.fd, tempName, finalName); err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *unixDir) Remove(name string) error {
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fs.ErrNotExist
		}
		return err
	}
	return nil
}

func (d *unixDir) Sync() error {
	return unix.Fsync(d.fd)
}

func (d *unixDir) Close() error {
	return d.file.Close()
}

// unixFile is a temp record opened through the pinned directory.
type unixFile struct {
	file *os.File
	fd   int
}

func (f *unixFile) Write(p []byte) (int, error) { return f.file.Write(p) }
func (f *unixFile) Sync() error                 { return f.file.Sync() }
func (f *unixFile) Close() error                { return f.file.Close() }

// Chmod goes through fchmod on the already-open descriptor rather than through
// the name, so it cannot land on a different file than the one being written.
func (f *unixFile) Chmod(mode fs.FileMode) error {
	return unix.Fchmod(f.fd, uint32(mode.Perm()))
}

func randomTempSuffix() (string, error) {
	raw := make([]byte, 8)
	if _, err := io.ReadFull(cryptoRandReader, raw); err != nil {
		return "", fmt.Errorf("name a docs discovery temp file: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
