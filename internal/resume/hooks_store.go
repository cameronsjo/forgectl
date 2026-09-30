package resume

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// HookStore is where RunHooks keeps its state: the last version recorded per
// harness, and the audit trail. FileHookStore is production; tests supply
// their own to inject a crash between the hooks and the record.
type HookStore interface {
	// Lock serializes runs; release is always non-nil on success.
	Lock(ctx context.Context) (release func(), err error)
	Recorded(harness string) (version string, ok bool, err error)
	Record(harness, version string) error
	Append(run HookRun) error
}

// File names inside the hooks state directory.
const (
	hookStateName = "state.json"
	hookRunsName  = "runs.jsonl"
	hookLockName  = "hooks.lock"
	// HookLogName is the watcher's launchd log (StandardOutPath).
	HookLogName = "watcher.log"
)

// hookRunsMaxBytes is where the audit trail rotates: runs.jsonl moves to
// runs.jsonl.1 (replacing the previous one) before an append would grow it
// past this. One record is a few hundred bytes, so this keeps thousands.
const hookRunsMaxBytes = 1 << 20

// FileHookStore keeps hook state as files under Dir.
type FileHookStore struct {
	Dir string
}

var _ HookStore = FileHookStore{}

// hookState is state.json.
type hookState struct {
	Versions map[string]string `json:"versions"`
}

func (s FileHookStore) path(name string) (string, error) {
	if s.Dir == "" {
		return "", errors.New("no forgectl state directory for update hooks")
	}
	return filepath.Join(s.Dir, name), nil
}

func (s FileHookStore) readState() (hookState, error) {
	p, err := s.path(hookStateName)
	if err != nil {
		return hookState{}, err
	}
	data, err := os.ReadFile(p) // #nosec G304 -- fixed name under forgectl's own state dir
	if errors.Is(err, fs.ErrNotExist) {
		return hookState{}, nil
	}
	if err != nil {
		return hookState{}, fmt.Errorf("read %s: %w", termsafe.QuotePath(p), termsafe.Error(err))
	}
	var st hookState
	if err := json.Unmarshal(data, &st); err != nil {
		return hookState{}, fmt.Errorf("%s is not valid JSON (delete it to record a fresh baseline): %w", termsafe.QuotePath(p), err)
	}
	return st, nil
}

// Recorded implements HookStore. A recorded value that is not a version is
// refused rather than passed on: it reaches a hook's environment and the log.
func (s FileHookStore) Recorded(harness string) (string, bool, error) {
	st, err := s.readState()
	if err != nil {
		return "", false, err
	}
	v, ok := st.Versions[harness]
	if !ok {
		return "", false, nil
	}
	if _, err := ParseVersion(v); err != nil {
		return "", false, fmt.Errorf("recorded %s version in %s: %w (delete the file to record a fresh baseline)", harness, hookStateName, err)
	}
	return v, true, nil
}

// Record implements HookStore. The write is atomic (temp file, then rename):
// a run killed mid-write must leave the previous record, never a truncated
// one that reads as corrupt.
func (s FileHookStore) Record(harness, version string) error {
	st, err := s.readState()
	if err != nil {
		return err
	}
	if st.Versions == nil {
		st.Versions = map[string]string{}
	}
	st.Versions[harness] = version
	// termsafe:allow-raw-json persisted hook state, never command output
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	final, err := s.path(hookStateName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", termsafe.QuotePath(s.Dir), termsafe.Error(err))
	}
	tmp, err := os.CreateTemp(s.Dir, hookStateName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create a temp state file in %s: %w", termsafe.QuotePath(s.Dir), termsafe.Error(err))
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // gone after a successful rename; this only cleans up a failure
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return termsafe.Error(err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // the sync error is the one worth reporting
		return termsafe.Error(err)
	}
	if err := tmp.Close(); err != nil {
		return termsafe.Error(err)
	}
	return termsafe.Error(os.Rename(tmpName, final))
}

// Append implements HookStore: one JSON line per hook run, in a 0600 file.
func (s FileHookStore) Append(run HookRun) error {
	p, err := s.path(hookRunsName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", termsafe.QuotePath(s.Dir), termsafe.Error(err))
	}
	if fi, err := os.Lstat(p); err == nil && fi.Size() >= hookRunsMaxBytes {
		if err := os.Rename(p, p+".1"); err != nil {
			return fmt.Errorf("rotate %s: %w", termsafe.QuotePath(p), termsafe.Error(err))
		}
	}
	// termsafe:allow-raw-json persisted audit record, never command output
	line, err := json.Marshal(run)
	if err != nil {
		return err
	}
	f, err := openAppendNoFollow(p)
	if err != nil {
		return fmt.Errorf("open %s: %w", termsafe.QuotePath(p), termsafe.Error(err))
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	return termsafe.Error(errors.Join(werr, cerr))
}

// RecentRuns returns up to n of the newest audit records, oldest first. A
// line that does not parse is skipped: one torn write must not hide the rest.
func (s FileHookStore) RecentRuns(n int) ([]HookRun, error) {
	p, err := s.path(hookRunsName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p) // #nosec G304 -- fixed name under forgectl's own state dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", termsafe.QuotePath(p), termsafe.Error(err))
	}
	var runs []HookRun
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		var r HookRun
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			runs = append(runs, r)
		}
	}
	if len(runs) > n {
		runs = runs[len(runs)-n:]
	}
	return runs, nil
}

// RecordedVersions returns every recorded version, keyed by harness.
func (s FileHookStore) RecordedVersions() (map[string]string, error) {
	st, err := s.readState()
	return st.Versions, err
}
