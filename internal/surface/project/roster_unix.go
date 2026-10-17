//go:build unix

package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

const (
	projectDirectoryMode = 0o700
	maxRosterBytes       = 1 * 1024 * 1024
	projectDirectoryName = "team"
	rosterName           = "roster.json"
	rosterLockName       = "roster.lock"
)

type rosterSnapshot struct {
	present bool
	byName  map[string]mail.Worker
}

type rosterDocument struct {
	Version int           `json:"v"`
	Workers []mail.Worker `json:"workers"`
}

func readRoster(rootFD int, projectID string) (rosterSnapshot, error) {
	projectFD, err := openPrivateDirectory(rootFD, projectID)
	if errors.Is(err, privdir.ErrAbsent) {
		return rosterSnapshot{}, nil
	}
	if err != nil {
		return rosterSnapshot{}, stateError("open project ledger: %s", privdir.Reason(err))
	}
	defer func() { _ = unix.Close(projectFD) }()
	teamFD, err := openPrivateDirectory(projectFD, projectDirectoryName)
	if errors.Is(err, privdir.ErrAbsent) {
		return rosterSnapshot{}, nil
	}
	if err != nil {
		return rosterSnapshot{}, stateError("open team ledger: %s", privdir.Reason(err))
	}
	defer func() { _ = unix.Close(teamFD) }()
	if err := checkRosterLock(teamFD); err != nil {
		return rosterSnapshot{}, err
	}
	rosterFile, err := privdir.OpenFileChecked(teamFD, rosterName, unix.O_RDONLY, stateMode)
	if errors.Is(err, os.ErrNotExist) {
		return rosterSnapshot{}, nil
	}
	if err != nil {
		return rosterSnapshot{}, stateError("open roster: %s", privdir.Reason(err))
	}
	defer func() { _ = rosterFile.Close() }()
	data, err := io.ReadAll(io.LimitReader(rosterFile, maxRosterBytes+1))
	if err != nil {
		return rosterSnapshot{}, stateError("read roster: %v", err)
	}
	if len(data) > maxRosterBytes {
		return rosterSnapshot{}, stateError("roster exceeds %d bytes", maxRosterBytes)
	}
	document, err := decodeRoster(data)
	if err != nil {
		return rosterSnapshot{}, err
	}
	workers := make(map[string]mail.Worker, len(document.Workers))
	runIDs := make(map[string]struct{}, len(document.Workers))
	coordinators := 0
	for _, worker := range document.Workers {
		if err := validateRosterWorker(worker); err != nil {
			return rosterSnapshot{}, err
		}
		if worker.Coordinator {
			coordinators++
			if coordinators > 1 {
				return rosterSnapshot{}, stateError("roster has multiple coordinators")
			}
		}
		if _, exists := workers[worker.Name]; exists {
			return rosterSnapshot{}, stateError("roster has duplicate member %q", worker.Name)
		}
		if worker.RunID != "" {
			if _, exists := runIDs[worker.RunID]; exists {
				return rosterSnapshot{}, stateError("roster has duplicate run ID %q", worker.RunID)
			}
			runIDs[worker.RunID] = struct{}{}
		}
		workers[worker.Name] = worker
	}
	return rosterSnapshot{present: true, byName: workers}, nil
}

func openPrivateDirectory(parentFD int, name string) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_RDONLY, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, privdir.ErrAbsent
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, fmt.Errorf("%w: %s is not a directory", privdir.ErrUnsafe, name)
	case err != nil:
		return -1, fmt.Errorf("%w: open %s: %v", privdir.ErrUnsafe, name, err)
	}
	var pinnedStat unix.Stat_t
	if err := unix.Fstat(fd, &pinnedStat); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%w: stat %s: %v", privdir.ErrUnsafe, name, err)
	}
	if pinnedStat.Mode&unix.S_IFMT != unix.S_IFDIR || int(pinnedStat.Uid) != os.Geteuid() || uint32(pinnedStat.Mode)&0o7777 != projectDirectoryMode {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%w: unsafe directory %s", privdir.ErrUnsafe, name)
	}
	var namedStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &namedStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%w: stat %s entry: %v", privdir.ErrUnsafe, name, err)
	}
	if namedStat.Dev != pinnedStat.Dev || namedStat.Ino != pinnedStat.Ino {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%w: %s entry changed", privdir.ErrUnsafe, name)
	}
	return fd, nil
}

func checkRosterLock(teamFD int) error {
	lock, err := privdir.OpenFileChecked(teamFD, rosterLockName, unix.O_RDONLY, stateMode)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return stateError("open roster lock: %s", privdir.Reason(err))
	}
	if err := lock.Close(); err != nil {
		return stateError("close roster lock: %v", err)
	}
	return nil
}

func decodeRoster(data []byte) (rosterDocument, error) {
	if len(data) == 0 || !utf8.Valid(data) {
		return rosterDocument{}, stateError("roster is not valid UTF-8")
	}
	if err := scanJSON(data); err != nil {
		return rosterDocument{}, stateError("decode roster: %v", err)
	}
	if err := checkRosterSchemaKeys(data); err != nil {
		return rosterDocument{}, stateError("decode roster: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document rosterDocument
	if err := decoder.Decode(&document); err != nil {
		return rosterDocument{}, stateError("decode roster: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return rosterDocument{}, stateError("decode roster: trailing data")
		}
		return rosterDocument{}, stateError("decode roster: trailing data: %v", err)
	}
	if document.Version != 1 {
		return rosterDocument{}, stateError("unsupported roster version %d", document.Version)
	}
	return document, nil
}

func checkRosterSchemaKeys(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("roster must be an object")
	}
	if err := checkKeys(raw, "v", "workers"); err != nil {
		return err
	}
	if err := requireFields(raw, "v", "workers"); err != nil {
		return err
	}
	var workers []json.RawMessage
	if err := json.Unmarshal(raw["workers"], &workers); err != nil {
		return fmt.Errorf("workers: %w", err)
	}
	for workerIndex, workerRaw := range workers {
		var worker map[string]json.RawMessage
		if err := json.Unmarshal(workerRaw, &worker); err != nil {
			return fmt.Errorf("workers[%d]: %w", workerIndex, err)
		}
		if err := checkKeys(worker, "name", "harness", "worktree", "coordinator", "run_id", "socket", "thread_id", "pane_id", "state", "state_at", "watchers"); err != nil {
			return fmt.Errorf("workers[%d]: %w", workerIndex, err)
		}
		if err := requireFields(worker, "name", "harness", "worktree", "state_at"); err != nil {
			return fmt.Errorf("workers[%d]: %w", workerIndex, err)
		}
		for key, value := range worker {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("workers[%d].%s is null", workerIndex, key)
			}
		}
	}
	return nil
}

func validateRosterWorker(worker mail.Worker) error {
	if err := mail.ValidateName(worker.Name); err != nil {
		return stateError("roster member: %v", err)
	}
	if err := validateHarness(worker.Harness, "roster harness"); err != nil {
		return err
	}
	if worker.Harness == mail.HarnessPi || worker.Harness == mail.HarnessHermes {
		if worker.Socket == "" {
			return stateError("roster member %q has no harness socket", worker.Name)
		}
	}
	if !filepath.IsAbs(worker.Worktree) {
		return stateError("roster member %q has a non-absolute worktree", worker.Name)
	}
	if worker.Socket != "" && !filepath.IsAbs(worker.Socket) {
		return stateError("roster member %q has a non-absolute socket", worker.Name)
	}
	if err := validateOptionalToken(worker.RunID, "roster run ID"); err != nil {
		return stateError("roster member %q: %v", worker.Name, err)
	}
	if err := validateOptionalToken(worker.ThreadID, "roster native session ID"); err != nil {
		return stateError("roster member %q: %v", worker.Name, err)
	}
	if err := validateOptionalToken(worker.PaneID, "roster pane ID"); err != nil {
		return stateError("roster member %q: %v", worker.Name, err)
	}
	if worker.State != "" {
		switch worker.State {
		case mail.StateUnknown, mail.StateAbsent, mail.StateIdle, mail.StateBusy, mail.StateWaiting:
		default:
			return stateError("roster member %q has an unknown state", worker.Name)
		}
	}
	for _, watcher := range worker.Watchers {
		if err := mail.ValidateName(watcher); err != nil {
			return stateError("roster member %q has an invalid watcher", worker.Name)
		}
	}
	return nil
}
