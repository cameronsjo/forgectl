//go:build unix

package desk

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// Present reports whether dir is a desk: it holds at least one protocol
// subdirectory. It reads only. An absent dir, or one with none of the
// subdirectories, is (false, nil); anything else that stops the check (dir is
// a file or a symlink, a parent that cannot be searched, a protocol entry
// that is a symlink or a file) is an error, so a caller never reports "not a
// desk" for a path it could not read.
func Present(dir string) (bool, error) {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("desk: %s: %w", describe(dir), err)
	case !fi.IsDir():
		return false, fmt.Errorf("desk: %s is not a directory", describe(dir))
	}
	found := false
	for _, sub := range protocolDirs {
		fi, err := os.Lstat(filepath.Join(dir, sub))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return false, fmt.Errorf("desk: %s/: %w", sub, err)
		case !fi.IsDir():
			return false, fmt.Errorf("desk: %s/ is not a directory; refusing", sub)
		}
		found = true
	}
	return found, nil
}

// OpenReadOnly opens the desk at dir for reading. Unlike [Open] it creates
// nothing, migrates nothing and changes no mode; Scan on it writes no meta and
// moves no item (a changed item reads as refused), and every write method
// returns ErrReadOnly. It refuses what Open refuses, with the checks made
// read-only (privdir.OpenChecked, checkProtocolDir), as [PrunePlanAt] does. A
// directory that is absent or holds no protocol subdirectory is ErrNoDesk.
func OpenReadOnly(dir string) (*Desk, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("desk: the desk directory must be an absolute path")
	}
	dir = filepath.Clean(dir)
	ok, err := Present(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoDesk
	}
	fd, err := privdir.OpenChecked(privdir.Spec{Base: filepath.Dir(dir), Leaf: filepath.Base(dir), Mode: dirMode})
	switch {
	case errors.Is(err, privdir.ErrAbsent):
		return nil, ErrNoDesk
	case err != nil:
		return nil, fmt.Errorf("desk: open %s: %w", describe(dir), err)
	}
	for _, sub := range protocolDirs {
		if err := checkProtocolDir(fd, dir, sub); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("desk: open %s: %w", describe(dir), err)
	}
	d := &Desk{path: dir, fd: fd, root: root, now: time.Now, grace: DefaultGrace, readOnly: true}
	if err := d.bindRoot(); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// ReadOnly reports whether d was opened by [OpenReadOnly].
func (d *Desk) ReadOnly() bool { return d.readOnly }
