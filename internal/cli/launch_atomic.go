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
	errConfigTempIdentityLost         = errors.New("config temp name no longer identifies this write attempt")
	errConfigCommittedValidation      = errors.New("renamed config does not identify the validated private temp")
	errDirectoryDurabilityUnsupported = errors.New("directory durability is unsupported on this platform")
)

type atomicWriteOps struct {
	lstat       func(string) (os.FileInfo, error)
	createTemp  func(string, string) (*os.File, error)
	writeAll    func(*os.File, []byte) error
	chmodFile   func(*os.File, os.FileMode) error
	syncFile    func(*os.File) error
	closeFile   func(*os.File) error
	pinTemp     func(*os.File) (*os.File, error)
	closePinned func(*os.File) error
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
		pinTemp:     pinConfigTemp,
		closePinned: (*os.File).Close,
		rename:      os.Rename,
		syncDir:     syncDirectory,
		remove:      os.Remove,
		openRegular: openRegularNoFollow,
		readAll:     io.ReadAll,
	}
}

func verifyOwnedTempPath(path string, owner os.FileInfo, ops atomicWriteOps) error {
	if owner == nil {
		return errConfigTempIdentityLost
	}
	current, err := ops.lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %v", errConfigTempIdentityLost, err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(owner, current) {
		return errConfigTempIdentityLost
	}
	return nil
}

func cleanupAtomicTemp(handle *os.File, tmpPath string, owner os.FileInfo, ops atomicWriteOps) error {
	if handle != nil {
		opened, err := handle.Stat()
		if err != nil || owner == nil || !os.SameFile(owner, opened) {
			_ = handle.Close()
			return fmt.Errorf("preserve config temp with unproved descriptor identity: %w", errConfigTempIdentityLost)
		}
	}
	if err := verifyOwnedTempPath(tmpPath, owner, ops); err != nil {
		if handle != nil {
			_ = handle.Close()
		}
		return fmt.Errorf("preserve unowned config temp occupant: %w", err)
	}
	if err := ops.remove(tmpPath); err != nil && !os.IsNotExist(err) {
		if handle != nil {
			_ = handle.Close()
		}
		return fmt.Errorf("remove partial config temp: %w", err)
	}
	if handle != nil {
		if err := handle.Close(); err != nil {
			return fmt.Errorf("close removed config temp descriptor: %w", err)
		}
	}
	if err := ops.syncDir(filepath.Dir(tmpPath)); err != nil {
		return fmt.Errorf("sync config parent after temp cleanup: %w", err)
	}
	return nil
}

func withCleanupError(primary error, tmp *os.File, tmpPath string, owner os.FileInfo, ops atomicWriteOps) error {
	if cleanupErr := cleanupAtomicTemp(tmp, tmpPath, owner, ops); cleanupErr != nil {
		return errors.Join(primary, cleanupErr)
	}
	return primary
}

func validateRenamedConfig(path string, pinned *os.File, owner os.FileInfo, expected []byte, expectedMode os.FileMode, ops atomicWriteOps) error {
	entry, err := ops.lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspect renamed entry: %v", errConfigCommittedValidation, err)
	}
	if !entry.Mode().IsRegular() || !os.SameFile(owner, entry) || entry.Mode().Perm() != expectedMode {
		return errConfigCommittedValidation
	}
	opened, err := pinned.Stat()
	if err != nil || !os.SameFile(owner, opened) || opened.Mode().Perm() != expectedMode {
		return errConfigCommittedValidation
	}
	if _, err := pinned.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%w: rewind pinned temp descriptor: %v", errConfigCommittedValidation, err)
	}
	data, err := ops.readAll(pinned)
	if err != nil || !bytes.Equal(data, expected) {
		return errConfigCommittedValidation
	}
	return nil
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
	owner, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return commitNone, fmt.Errorf("identify config temp allocation: %w", err)
	}
	if err := ops.writeAll(tmp, data); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("write config temp: %w", err), tmp, tmpPath, owner, ops)
	}
	info, err := tmp.Stat()
	if err != nil {
		return commitNone, withCleanupError(fmt.Errorf("stat config temp: %w", err), tmp, tmpPath, owner, ops)
	}
	finalPerm := info.Mode().Perm() & 0o600
	if existingPerm != nil {
		finalPerm &= *existingPerm
	}
	if err := ops.chmodFile(tmp, finalPerm); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("set private config permissions: %w", err), tmp, tmpPath, owner, ops)
	}
	if err := ops.syncFile(tmp); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("sync config temp: %w", err), tmp, tmpPath, owner, ops)
	}
	pinned, err := ops.pinTemp(tmp)
	if err != nil {
		return commitNone, withCleanupError(fmt.Errorf("pin config temp identity: %w", err), tmp, tmpPath, owner, ops)
	}
	if err := ops.closeFile(tmp); err != nil {
		_ = pinned.Close()
		return commitNone, withCleanupError(fmt.Errorf("close config temp: %w", err), tmp, tmpPath, owner, ops)
	}
	tmp = nil
	if err := verifyOwnedTempPath(tmpPath, owner, ops); err != nil {
		_ = pinned.Close()
		return commitNone, fmt.Errorf("verify config temp before rename: %w", err)
	}
	if err := ops.rename(tmpPath, path); err != nil {
		return commitNone, withCleanupError(fmt.Errorf("rename config into place: %w", err), pinned, tmpPath, owner, ops)
	}
	if err := validateRenamedConfig(path, pinned, owner, data, finalPerm, ops); err != nil {
		_ = pinned.Close()
		return commitRenamed, fmt.Errorf("validate renamed config before durability claim: %w", err)
	}
	if err := ops.closePinned(pinned); err != nil {
		return commitRenamed, fmt.Errorf("close pinned config temp after rename validation: %w", err)
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
