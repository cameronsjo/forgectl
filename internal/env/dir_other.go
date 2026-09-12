//go:build !unix

package env

import (
	"errors"
	"os"
	"path/filepath"
)

// errIsSymlink and errNotRegular keep the sentinels available off Unix so
// callers compile unchanged.
var (
	errIsSymlink  = errors.New("path became a symlink after it was resolved")
	errNotRegular = errors.New("path is not a regular file")
)

// dirPin degrades to a plain directory path off Unix, for the same reason
// withFileLock is a no-op there: goreleaser ships linux and darwin only (both
// carry the unix build tag), so this file exists to keep `go build` working on
// a contributor's machine and never reaches a shipped binary. The openat
// anchoring is therefore absent here, which is a real gap in a hypothetical
// Windows build and not a gap in anything forgectl distributes.
type dirPin struct{ path string }

func pinDir(path string) (*dirPin, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errIsSymlink
	}
	return &dirPin{path: path}, nil
}

func (d *dirPin) close() {}

func (d *dirPin) openRegular(name string) (*os.File, error) {
	full := filepath.Join(d.path, name)
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errIsSymlink
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	return os.Open(full)
}

func (d *dirPin) openLock(name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(d.path, name), os.O_CREATE|os.O_RDWR, 0o600)
}

func (d *dirPin) lstat(name string) (perm os.FileMode, regular, exists bool, err error) {
	info, err := os.Lstat(filepath.Join(d.path, name))
	if os.IsNotExist(err) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	return info.Mode().Perm(), info.Mode().IsRegular(), true, nil
}

func (d *dirPin) createTemp(prefix string) (*os.File, string, error) {
	f, err := os.CreateTemp(d.path, prefix+"*.tmp")
	if err != nil {
		return nil, "", err
	}
	return f, filepath.Base(f.Name()), nil
}

func (d *dirPin) rename(from, to string) error {
	return os.Rename(filepath.Join(d.path, from), filepath.Join(d.path, to))
}

func (d *dirPin) remove(name string) error {
	return os.Remove(filepath.Join(d.path, name))
}
