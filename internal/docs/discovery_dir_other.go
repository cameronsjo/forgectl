//go:build !darwin && !linux

package docs

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Fixed refusals shared with the Unix implementation's vocabulary, so callers
// see the same categories on every build target.
var (
	errUnsafeDirType = errors.New("the docs discovery directory path is not a directory")
	errUnsafeRecord  = errors.New("a docs discovery record failed its type checks")
)

// portableDir keeps production compiling and broadly working off the shipped
// macOS and Linux targets, WITHOUT claiming their security properties.
//
// There are no owner, mode, or link-count checks here, and mode bits are not an
// ACL claim. What it does keep is the property the whole design rests on:
// publication is no-replace. os.Link fails when the destination exists, so a
// record still cannot overwrite a live sibling's. A filesystem that does not
// support links fails publication rather than falling back to os.Rename, which
// would succeed at exactly the overwrite this is here to prevent.
type portableDir struct {
	path string
	file *os.File
}

func openDiscoveryDir(path string, create bool) (discoveryDir, error) {
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}

	// Lstat before open so a symlink at the leaf is refused rather than
	// followed to a directory this code never inspected.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, errUnsafeDirType
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close() //nolint:errcheck // already failing
		return nil, err
	}
	// Compare the opened directory's identity with the pre-open identity, so a
	// swap between the Lstat and the Open is caught where the platform reports
	// enough to see it.
	if !os.SameFile(info, opened) {
		file.Close() //nolint:errcheck // refusing this handle
		return nil, errUnsafeDirType
	}
	return &portableDir{path: path, file: file}, nil
}

func (d *portableDir) CreateTemp() (discoveryWriteFile, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		raw := make([]byte, 8)
		if _, err := io.ReadFull(cryptoRandReader, raw); err != nil {
			return nil, "", fmt.Errorf("name a docs discovery temp file: %w", err)
		}
		name := ".tmp-" + hex.EncodeToString(raw)
		file, err := os.OpenFile(filepath.Join(d.path, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return nil, "", err
		}
		return &portableFile{file: file}, name, nil
	}
	return nil, "", errors.New("could not create a private docs discovery temp file")
}

func (d *portableDir) OpenRecord(name string) (io.ReadCloser, error) {
	full := filepath.Join(d.path, name)
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errUnsafeRecord
	}
	file, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close() //nolint:errcheck // already failing
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		file.Close() //nolint:errcheck // refusing this record
		return nil, errUnsafeRecord
	}
	return file, nil
}

func (d *portableDir) ReadDir(n int) ([]fs.DirEntry, error) {
	return d.file.ReadDir(n)
}

// InstallNoReplace links the temp to its final name and then removes the temp.
//
// A removal failure AFTER a successful link is a warning, not an error: the
// final record is complete and usable, and the leftover temp is inert because
// readers ignore every name that is not an authoritative record.
func (d *portableDir) InstallNoReplace(tempName, finalName string) (error, error) {
	if err := os.Link(filepath.Join(d.path, tempName), filepath.Join(d.path, finalName)); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(d.path, tempName)); err != nil {
		return fmt.Errorf("remove the installed docs discovery temp file: %w", sanitizeFSError(err)), nil
	}
	return nil, nil
}

func (d *portableDir) Remove(name string) error {
	return os.Remove(filepath.Join(d.path, name))
}

func (d *portableDir) Sync() error {
	return d.file.Sync()
}

func (d *portableDir) Close() error {
	return d.file.Close()
}

type portableFile struct {
	file *os.File
}

func (f *portableFile) Write(p []byte) (int, error)  { return f.file.Write(p) }
func (f *portableFile) Chmod(mode fs.FileMode) error { return f.file.Chmod(mode) }
func (f *portableFile) Sync() error                  { return f.file.Sync() }
func (f *portableFile) Close() error                 { return f.file.Close() }
