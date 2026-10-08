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

// sameFile reports whether names a and b are one file; see the unix version.
func (d *dirPin) sameFile(a, b string) (bool, error) {
	ia, err := os.Lstat(filepath.Join(d.path, a))
	if err != nil {
		return false, err
	}
	ib, err := os.Lstat(filepath.Join(d.path, b))
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}

func (d *dirPin) createTemp(prefix string) (*os.File, string, error) {
	f, err := os.CreateTemp(d.path, prefix+"*.tmp")
	if err != nil {
		return nil, "", err
	}
	return f, filepath.Base(f.Name()), nil
}

func (d *dirPin) names() ([]string, error) {
	f, err := os.Open(d.path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

func (d *dirPin) remove(name string) error {
	return os.Remove(filepath.Join(d.path, name))
}

// mkScratchDir creates the scratch directory by path; see the unix version.
func (d *dirPin) mkScratchDir(prefix string) (*dirPin, string, error) {
	dir, err := MakeScratchDir(d.path, prefix)
	if err != nil {
		return nil, "", err
	}
	return &dirPin{path: dir}, filepath.Base(dir), nil
}

// unlinkScratchEntry is the unix version's test seam; see there.
var unlinkScratchEntry = func(sub *dirPin, name string) error { return sub.remove(name) }

// removeScratchDir unlinks own, then removes the directory by the teardown
// rule; see the unix version.
func (d *dirPin) removeScratchDir(sub *dirPin, name string, own ...string) error {
	for _, n := range own {
		_ = unlinkScratchEntry(sub, n)
	}
	return RemoveScratchDir(filepath.Join(d.path, name))
}

func (d *dirPin) renameFrom(sub *dirPin, from, to string) error {
	return os.Rename(filepath.Join(sub.path, from), filepath.Join(d.path, to))
}
