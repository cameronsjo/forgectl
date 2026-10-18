//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package mail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func lockedPrivateMailbox(box Mailbox, fn func(*Tx) error) error {
	private, err := openPrivateMailbox(box.Dir)
	if err != nil {
		return err
	}
	defer private.close()
	unlock, err := private.lock("mailbox.lock")
	if err != nil {
		return fmt.Errorf("mailbox lock: %w", err)
	}
	defer unlock()
	return fn(&Tx{box: box, private: private})
}

func openPrivateMailbox(dir string) (*privateMailbox, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, fmt.Errorf("private mailbox directory must be an absolute clean path")
	}
	fd, err := privdir.OpenChecked(privdir.Spec{Base: filepath.Dir(dir), Leaf: filepath.Base(dir), Mode: 0o700})
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if uint32(stat.Mode)&0o7777 != 0o700 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("private mailbox directory has unsafe mode %04o", uint32(stat.Mode)&0o7777)
	}
	return &privateMailbox{dirFD: fd}, nil
}

func (store *privateMailbox) close() {
	if store == nil || store.dirFD < 0 {
		return
	}
	_ = unix.Close(store.dirFD)
	store.dirFD = -1
}

func (store *privateMailbox) lock(name string) (func(), error) {
	file, err := store.openCheckedOrCreate(name, unix.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	fd := int(file.Fd())
	deadline := time.Now().Add(lockWait)
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (store *privateMailbox) openRead(name string) (*os.File, error) {
	return privdir.OpenFileChecked(store.dirFD, name, unix.O_RDONLY, 0o600)
}

func (store *privateMailbox) openAppend(name string) (*os.File, error) {
	return store.openCheckedOrCreate(name, unix.O_RDWR|unix.O_APPEND, 0o600)
}

func (store *privateMailbox) openCheckedOrCreate(name string, flags int, mode fs.FileMode) (*os.File, error) {
	file, err := privdir.OpenFileChecked(store.dirFD, name, flags, mode)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	created, createErr := privdir.OpenFile(store.dirFD, name, flags|unix.O_CREAT|unix.O_EXCL, mode)
	if createErr != nil {
		return privdir.OpenFileChecked(store.dirFD, name, flags, mode)
	}
	if created != nil {
		_ = created.Close()
	}
	return privdir.OpenFileChecked(store.dirFD, name, flags, mode)
}

func (store *privateMailbox) worker(name string) (Worker, error) {
	unlock, err := store.lock("roster.lock")
	if err != nil {
		return Worker{}, fmt.Errorf("roster lock: %w", err)
	}
	defer unlock()
	file, err := store.openRead("roster.json")
	if errors.Is(err, fs.ErrNotExist) {
		return Worker{}, fmt.Errorf("%w: %s", ErrUnknownWorker, quoteTrunc(name))
	}
	if err != nil {
		return Worker{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Worker{}, err
	}
	if info.Size() > 8<<20 {
		return Worker{}, errors.New("private roster is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8<<20+1))
	if err != nil {
		return Worker{}, err
	}
	if len(data) > 8<<20 {
		return Worker{}, errors.New("private roster is too large")
	}
	var roster rosterFile
	if err := decodePrivateRoster(bytes.TrimSpace(data), &roster); err != nil {
		return Worker{}, fmt.Errorf("private roster: %w", err)
	}
	for _, worker := range roster.Workers {
		if worker.Name == name {
			return worker, nil
		}
	}
	return Worker{}, fmt.Errorf("%w: %s", ErrUnknownWorker, quoteTrunc(name))
}
