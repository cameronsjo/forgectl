package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type commitState uint8

const (
	commitNone commitState = iota
	commitRenamed
	commitDurable
)

var (
	errConfigDestinationNonRegular    = errors.New("config destination is not a regular file or leaf symlink")
	errDirectoryDurabilityUnsupported = errors.New("directory durability is unsupported on this platform")
)

type atomicWriteOps struct {
	lstat       func(string) (os.FileInfo, error)
	createTemp  func(string, string) (*os.File, error)
	writeAll    func(*os.File, []byte) error
	chmodFile   func(*os.File, os.FileMode) error
	syncFile    func(*os.File) error
	closeFile   func(*os.File) error
	rename      func(string, string) error
	syncDir     func(string) error
	remove      func(string) error
	openRegular func(string) (*os.File, error)
	readAll     func(io.Reader) ([]byte, error)
}

func nativeAtomicWriteOps() atomicWriteOps {
	return atomicWriteOps{
		lstat:      os.Lstat,
		createTemp: os.CreateTemp,
		writeAll: func(f *os.File, data []byte) error {
			for len(data) > 0 {
				n, err := f.Write(data)
				if err != nil {
					return err
				}
				if n == 0 {
					return io.ErrShortWrite
				}
				data = data[n:]
			}
			return nil
		},
		chmodFile:   (*os.File).Chmod,
		syncFile:    (*os.File).Sync,
		closeFile:   (*os.File).Close,
		rename:      os.Rename,
		syncDir:     syncDirectory,
		remove:      os.Remove,
		openRegular: openRegularNoFollow,
		readAll:     io.ReadAll,
	}
}

func cleanupAtomicTemp(tmp *os.File, tmpPath string, ops atomicWriteOps) error {
	if tmp != nil {
		_ = tmp.Close()
	}
	if err := ops.remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove partial config temp: %w", err)
	}
	if err := ops.syncDir(filepath.Dir(tmpPath)); err != nil {
		return fmt.Errorf("sync config parent after temp cleanup: %w", err)
	}
	return nil
}

func withCleanupError(primary error, tmp *os.File, tmpPath string, ops atomicWriteOps) error {
	if cleanupErr := cleanupAtomicTemp(tmp, tmpPath, ops); cleanupErr != nil {
		return errors.Join(primary, cleanupErr)
	}
	return primary
}

// writeConfigAtomicWithOps is lock-agnostic. Its caller owns the complete
// read/decide/write lock and parent creation. The returned phase distinguishes
// a pre-rename failure from a visible-but-not-durable rename.
func writeConfigAtomicWithOps(path string, data []byte, ops atomicWriteOps) (commitState, error) {
	dir := filepath.Dir(path)
	var existingPerm *os.FileMode
	if info, err := ops.lstat(path); err == nil {
		switch {
		case info.Mode().IsRegular():
			perm := info.Mode().Perm()
			existingPerm = &perm
		case info.Mode()&os.ModeSymlink != 0:
			// Rename replaces only the leaf symlink; its target is untouched.
		default:
			return commitNone, errConfigDestinationNonRegular
		}
	} else if !os.IsNotExist(err) {
		return commitNone, fmt.Errorf("inspect config destination: %w", err)
	}

	tmp, err := ops.createTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return commitNone, fmt.Errorf("create config temp: %w", err)
	}
	tmpPath := tmp.Name()
	if err := ops.writeAll(tmp, data); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("write config temp: %w", err), tmp, tmpPath, ops)
	}
	info, err := tmp.Stat()
	if err != nil {
		return commitNone, withCleanupError(fmt.Errorf("stat config temp: %w", err), tmp, tmpPath, ops)
	}
	finalPerm := info.Mode().Perm() & 0o600
	if existingPerm != nil {
		finalPerm &= *existingPerm
	}
	if err := ops.chmodFile(tmp, finalPerm); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("set private config permissions: %w", err), tmp, tmpPath, ops)
	}
	if err := ops.syncFile(tmp); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("sync config temp: %w", err), tmp, tmpPath, ops)
	}
	if err := ops.closeFile(tmp); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("close config temp: %w", err), tmp, tmpPath, ops)
	}
	tmp = nil
	if err := ops.rename(tmpPath, path); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("rename config into place: %w", err), nil, tmpPath, ops)
	}
	if err := ops.syncDir(dir); err != nil {
		return commitRenamed, fmt.Errorf("sync config parent after rename: %w", err)
	}
	return commitDurable, nil
}

func confirmConfigDurableWithOps(path string, expected []byte, ops atomicWriteOps) error {
	f, err := ops.openRegular(path)
	if err != nil {
		return fmt.Errorf("open current config for durability confirmation: %w", err)
	}
	data, err := ops.readAll(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("read current config for durability confirmation: %w", err)
	}
	if !bytes.Equal(data, expected) {
		_ = f.Close()
		return fmt.Errorf("current config bytes changed before durability confirmation")
	}
	if err := ops.syncFile(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync current config for durability confirmation: %w", err)
	}
	if err := ops.closeFile(f); err != nil {
		return fmt.Errorf("close current config after durability confirmation: %w", err)
	}
	if err := ops.syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync config parent for durability confirmation: %w", err)
	}
	return nil
}
