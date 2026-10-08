package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Stage is how far a worker launch got. A launch writes StagePending before it
// creates anything and moves forward after each step, so a launch that dies
// partway leaves a row naming what it made.
type Stage string

const (
	// StagePending: the row exists, nothing else does yet.
	StagePending Stage = "pending"
	// StageWorktree: the worktree exists; no workspace has been started.
	StageWorktree Stage = "worktree"
	// StageLaunched: the harness started and the row carries its Ref.
	StageLaunched Stage = "launched"
	// StageFailed: a step failed. Worktree and Recovery say what may remain.
	StageFailed Stage = "failed"
	// StageClosed: `surface close` closed the workspace and kept the
	// worktree, because a removal check failed. A later close retries it.
	StageClosed Stage = "closed"
)

// Row is one worker in the ledger.
type Row struct {
	Name      string    `json:"name"`
	Harness   string    `json:"harness,omitempty"`
	Branch    string    `json:"branch"`
	Worktree  string    `json:"worktree,omitempty"`
	Base      string    `json:"base,omitempty"`
	Stage     Stage     `json:"stage"`
	StartedAt time.Time `json:"started_at"`
	// Ref is the encoded backend.Ref of the worker's workspace. It is kept
	// encoded here and decoded with backend.DecodeRef by whoever acts on it,
	// so this package never holds an unvalidated reference.
	Ref json.RawMessage `json:"ref,omitempty"`
	// Recovery is the ownership tag of a workspace a failed launch could not
	// close, when there is one.
	Recovery string `json:"recovery,omitempty"`
	// Failure is the error a failed step reported.
	Failure string `json:"failure,omitempty"`
	// Brief is the last brief recorded for the worker, or nil before one.
	// It is written before the brief is sent, so a send that dies partway
	// still leaves the marker its report will carry.
	Brief *Brief `json:"brief,omitempty"`
	// SessionID is the --session-id a claude worker was started with, and
	// Transcript the file Claude Code writes that session to. Both are empty
	// for a codex worker.
	SessionID  string `json:"session_id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	// LaunchID is the queue claim this row was launched for, written by
	// Begin. It is empty for a CLI launch. `surface drain` acts only on a
	// row whose LaunchID matches the queue row it claimed.
	LaunchID string `json:"launch_id,omitempty"`
}

// ledgerVersion is the on-disk format version. A file with another version is
// refused rather than rewritten, so an older build never drops fields a newer
// one wrote.
const ledgerVersion = 1

type ledgerFile struct {
	Version int    `json:"version"`
	Repo    string `json:"repo"`
	Session string `json:"session"`
	Workers []Row  `json:"workers"`
}

var (
	// ErrNameTaken reports a launch whose name already has a row.
	ErrNameTaken = errors.New("worker: a worker with that name is already in this repo's ledger")
	// ErrNoRow reports an update for a name with no row.
	ErrNoRow = errors.New("worker: no ledger row with that name")
	// ErrLedgerUnreadable reports a ledger file that exists but cannot be used.
	ErrLedgerUnreadable = errors.New("worker: the ledger file is unreadable")
)

// maxLedgerBytes caps a ledger file. A few hundred workers fit easily; a file
// past this is not one forgectl wrote.
const maxLedgerBytes = 1 << 20

// ledgerKey names the ledger file for a repo and herdr session. It is a hash,
// never the path text, so a repo path cannot shape a file name.
func ledgerKey(repo, session string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + session))
	return hex.EncodeToString(sum[:16])
}

// decodeLedger parses a ledger file. Empty data is a new ledger. A file whose
// recorded repo or session differs from the one asked for is refused: the key
// is a truncated hash, and acting on another repo's rows is the failure that
// matters.
func decodeLedger(data []byte, repo, session string) (ledgerFile, error) {
	if len(data) == 0 {
		return ledgerFile{Version: ledgerVersion, Repo: repo, Session: session}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f ledgerFile
	if err := dec.Decode(&f); err != nil {
		return ledgerFile{}, fmt.Errorf("%w: %w", ErrLedgerUnreadable, err)
	}
	if f.Version != ledgerVersion {
		return ledgerFile{}, fmt.Errorf("%w: version %d, this build reads %d", ErrLedgerUnreadable, f.Version, ledgerVersion)
	}
	if f.Repo != repo || f.Session != session {
		return ledgerFile{}, fmt.Errorf("%w: it belongs to another repo or herdr session", ErrLedgerUnreadable)
	}
	return f, nil
}

func encodeLedger(f ledgerFile) ([]byte, error) {
	// termsafe:allow-raw-json private 0600 ledger file read back by forgectl, never written to a terminal
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// insertRow adds row, refusing a name that is already present, with one
// exception. A failed launch keeps its row on purpose when it names a
// worktree, a workspace, or a recovery tag, because those may still hold work.
// A failed row that names none of them created nothing, so a retry under the
// same name replaces it.
func insertRow(rows []Row, row Row) ([]Row, error) {
	for i, r := range rows {
		if r.Name != row.Name {
			continue
		}
		if !CreatedNothing(r) {
			return nil, ErrNameTaken
		}
		rows[i] = row
		return rows, nil
	}
	return append(rows, row), nil
}

// CreatedNothing reports whether r is a failed row that names no worktree,
// workspace, or recovery tag: its launch left nothing behind, so a retry under
// the same name may replace it.
func CreatedNothing(r Row) bool {
	return r.Stage == StageFailed && r.Worktree == "" && len(r.Ref) == 0 && r.Recovery == ""
}

// updateRow applies fn to the row named name.
func updateRow(rows []Row, name string, fn func(*Row)) ([]Row, error) {
	for i := range rows {
		if rows[i].Name == name {
			fn(&rows[i])
			return rows, nil
		}
	}
	return nil, ErrNoRow
}

// store is the file the ledger lives in. update holds an exclusive lock for
// the whole read-modify-write.
type store interface {
	read() ([]byte, error)
	update(fn func([]byte) ([]byte, error)) error
}

// Ledger records the workers started for one repo in one herdr session.
type Ledger struct {
	repo    string
	session string
	store   store
}

// Open returns the ledger for repo (an absolute, symlink-resolved repo top)
// and the herdr session name. Ledgers live in $XDG_STATE_HOME/forgectl/surface,
// defaulting to ~/.local/state.
func Open(repo, session string) (*Ledger, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return nil, err
	}
	return openAt(base, repo, session)
}

func openAt(stateBase, repo, session string) (*Ledger, error) {
	if !filepath.IsAbs(repo) || session == "" {
		return nil, errors.New("worker: a ledger needs an absolute repo path and a session name")
	}
	return &Ledger{
		repo:    repo,
		session: session,
		store:   newFileStore(stateBase, ledgerKey(repo, session)),
	}, nil
}

// Begin writes a new pending row.
func (l *Ledger) Begin(row Row) error {
	row.Stage = StagePending
	return l.mutate(func(rows []Row) ([]Row, error) { return insertRow(rows, row) })
}

// Update changes the row named name.
func (l *Ledger) Update(name string, fn func(*Row)) error {
	return l.mutate(func(rows []Row) ([]Row, error) { return updateRow(rows, name, fn) })
}

// ErrRowChanged reports a conditional change to a row that no longer matches
// what the caller read: a launch reused the name, or the row moved on.
var ErrRowChanged = errors.New("worker: the ledger row changed since it was read")

// SameRow reports whether a row is still the one read as was: the same
// launch (start time) at the same stage. It is the guard close passes to
// RemoveIf and UpdateIf.
func SameRow(was Row) func(Row) bool {
	return func(r Row) bool { return r.StartedAt.Equal(was.StartedAt) && r.Stage == was.Stage }
}

// RemoveIf deletes the row named name when match accepts it.
func (l *Ledger) RemoveIf(name string, match func(Row) bool) error {
	return l.mutate(func(rows []Row) ([]Row, error) {
		for i := range rows {
			if rows[i].Name != name {
				continue
			}
			if !match(rows[i]) {
				return nil, ErrRowChanged
			}
			return append(rows[:i:i], rows[i+1:]...), nil
		}
		return nil, ErrNoRow
	})
}

// UpdateIf changes the row named name when match accepts it.
func (l *Ledger) UpdateIf(name string, match func(Row) bool, fn func(*Row)) error {
	return l.mutate(func(rows []Row) ([]Row, error) {
		for i := range rows {
			if rows[i].Name != name {
				continue
			}
			if !match(rows[i]) {
				return nil, ErrRowChanged
			}
			fn(&rows[i])
			return rows, nil
		}
		return nil, ErrNoRow
	})
}

// NameTaken reports whether Begin would refuse name with ErrNameTaken, without
// writing: a row with that name exists and is not a failed launch that created
// nothing. `surface launch --dry-run` asks it.
func (l *Ledger) NameTaken(name string) (bool, error) {
	rows, err := l.Rows()
	if err != nil {
		return false, err
	}
	_, err = insertRow(slices.Clone(rows), Row{Name: name})
	return errors.Is(err, ErrNameTaken), nil
}

// Rows returns every row.
func (l *Ledger) Rows() ([]Row, error) {
	data, err := l.store.read()
	if err != nil {
		return nil, err
	}
	f, err := decodeLedger(data, l.repo, l.session)
	if err != nil {
		return nil, err
	}
	return f.Workers, nil
}

func (l *Ledger) mutate(fn func([]Row) ([]Row, error)) error {
	return l.store.update(func(data []byte) ([]byte, error) {
		f, err := decodeLedger(data, l.repo, l.session)
		if err != nil {
			return nil, err
		}
		rows, err := fn(f.Workers)
		if err != nil {
			return nil, err
		}
		f.Workers = rows
		return encodeLedger(f)
	})
}
