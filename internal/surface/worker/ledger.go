package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

// insertRow adds row, refusing a name that is already present in any stage.
// A failed launch keeps its row on purpose: it names a worktree that may
// still hold work.
func insertRow(rows []Row, row Row) ([]Row, error) {
	for _, r := range rows {
		if r.Name == row.Name {
			return nil, ErrNameTaken
		}
	}
	return append(rows, row), nil
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
