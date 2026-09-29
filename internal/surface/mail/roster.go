package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Harness names the agent CLI a worker runs, which picks its Adapter.
type Harness string

const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
	HarnessPi     Harness = "pi"
	HarnessPane   Harness = "pane"
)

// WorkerState is a worker's coarse activity as forgectl last saw it.
type WorkerState string

const (
	StateUnknown WorkerState = "unknown"
	StateAbsent  WorkerState = "absent"
	StateIdle    WorkerState = "idle"
	StateBusy    WorkerState = "busy"
	StateWaiting WorkerState = "waiting"
)

// Worker is one roster entry. It is the messaging slice of the ledger entry
// #536 T1 writes; that ledger can embed or replace this record.
type Worker struct {
	Name        string  `json:"name"`
	Harness     Harness `json:"harness"`
	Worktree    string  `json:"worktree"`
	Coordinator bool    `json:"coordinator,omitempty"`
	// Socket is the claude inbox socket when known directly (the coordinator's
	// own CLAUDE_CODE_MESSAGING_SOCKET), or the pi extension's socket.
	Socket string `json:"socket,omitempty"`
	// ThreadID is the codex thread, learned from its first turn-complete notify.
	ThreadID string      `json:"thread_id,omitempty"`
	PaneID   string      `json:"pane_id,omitempty"`
	State    WorkerState `json:"state,omitempty"`
	StateAt  time.Time   `json:"state_at"`
	// Watchers get one notice from forgectl when this worker next goes idle.
	Watchers []string `json:"watchers,omitempty"`
}

// ErrUnknownWorker is returned for a name that is not in the roster.
var ErrUnknownWorker = errors.New("unknown worker")

// Roster is the address book the Service reads and updates.
type Roster interface {
	Get(name string) (Worker, error)
	List() ([]Worker, error)
	Put(w Worker) error
	Update(name string, fn func(*Worker) error) error
}

// FileRoster keeps the roster as roster.json in a ledger directory. Writes
// hold an exclusive lock and replace the file atomically; reads need no lock.
type FileRoster struct {
	Dir string
}

type rosterFile struct {
	V       int      `json:"v"`
	Workers []Worker `json:"workers"`
}

func (r FileRoster) path() string     { return filepath.Join(r.Dir, "roster.json") }
func (r FileRoster) lockPath() string { return filepath.Join(r.Dir, "roster.lock") }

func (r FileRoster) load() (rosterFile, error) {
	data, err := os.ReadFile(r.path())
	if errors.Is(err, fs.ErrNotExist) {
		return rosterFile{V: 1}, nil
	}
	if err != nil {
		return rosterFile{}, err
	}
	var f rosterFile
	if err := json.Unmarshal(data, &f); err != nil {
		return rosterFile{}, fmt.Errorf("roster %s: %w", r.path(), err)
	}
	return f, nil
}

func (r FileRoster) save(f rosterFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path(), append(data, '\n'))
}

// Get returns the named worker or ErrUnknownWorker.
func (r FileRoster) Get(name string) (Worker, error) {
	f, err := r.load()
	if err != nil {
		return Worker{}, err
	}
	for _, w := range f.Workers {
		if w.Name == name {
			return w, nil
		}
	}
	return Worker{}, fmt.Errorf("%w: %s", ErrUnknownWorker, quoteTrunc(name))
}

// List returns every worker in roster order.
func (r FileRoster) List() ([]Worker, error) {
	f, err := r.load()
	if err != nil {
		return nil, err
	}
	return f.Workers, nil
}

// Put adds a worker or replaces the one with the same name.
func (r FileRoster) Put(w Worker) error {
	if err := ValidateName(w.Name); err != nil {
		return err
	}
	return r.locked(func(f *rosterFile) error {
		for i := range f.Workers {
			if f.Workers[i].Name == w.Name {
				f.Workers[i] = w
				return nil
			}
		}
		f.Workers = append(f.Workers, w)
		return nil
	})
}

// Update applies fn to the named worker under the roster lock.
func (r FileRoster) Update(name string, fn func(*Worker) error) error {
	return r.locked(func(f *rosterFile) error {
		for i := range f.Workers {
			if f.Workers[i].Name == name {
				return fn(&f.Workers[i])
			}
		}
		return fmt.Errorf("%w: %s", ErrUnknownWorker, quoteTrunc(name))
	})
}

func (r FileRoster) locked(fn func(*rosterFile) error) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(r.lockPath())
	if err != nil {
		return fmt.Errorf("roster lock: %w", err)
	}
	defer unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	f.V = 1
	return r.save(f)
}

// writeFileAtomic writes data beside path and renames it into place, mode 0600.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
