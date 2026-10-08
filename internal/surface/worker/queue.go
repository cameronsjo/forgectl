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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/config"
)

// The queue holds briefs waiting for `surface drain` to launch them as
// workers. There is one queue per machine, in queue.json beside the ledgers,
// written through the same pinned directory, verified open, flock and atomic
// rename. A row's name is unique across the whole queue, whatever its repo.

// QueueState is where a queue row is in its life.
type QueueState string

const (
	// QueueQueued: waiting for the drain to claim it.
	QueueQueued QueueState = "queued"
	// QueueClaimed: the drain took it under the queue lock and is launching.
	QueueClaimed QueueState = "claimed"
	// QueueLaunched: the worker is running.
	QueueLaunched QueueState = "launched"
	// QueueNeedsYou: the worker is stopped at a prompt or idle without a
	// report, waiting for the operator.
	QueueNeedsYou QueueState = "needs-you"
	// QueueReported: the worker printed its report line.
	QueueReported QueueState = "reported"
	// QueueFailed: the launch or the worker failed.
	QueueFailed QueueState = "failed"
	// QueueClosed: `surface close` closed the worker.
	QueueClosed QueueState = "closed"
	// QueueExpired: the row sat queued too long and was never launched.
	QueueExpired QueueState = "expired"
)

// Live reports whether a row in state s may have a worker running: the drain
// claimed it and has not seen it end.
func (s QueueState) Live() bool {
	return s == QueueClaimed || s == QueueLaunched || s == QueueNeedsYou
}

// Terminal reports whether s is an end state. A terminal row keeps its brief
// hash and drops its brief text.
func (s QueueState) Terminal() bool {
	switch s {
	case QueueReported, QueueFailed, QueueClosed, QueueExpired:
		return true
	}
	return false
}

func (s QueueState) valid() bool {
	return s == QueueQueued || s.Live() || s.Terminal()
}

// QueueRow is one queued brief.
type QueueRow struct {
	Name string `json:"name"`
	// Repo is the repo's top level: absolute, symlinks resolved.
	Repo string `json:"repo"`
	// Brief is the brief text, empty once the row is terminal.
	Brief string `json:"brief,omitempty"`
	// BriefSHA256 is the hex SHA-256 of the brief text as enqueued.
	BriefSHA256 string     `json:"brief_sha256"`
	Batch       string     `json:"batch,omitempty"`
	State       QueueState `json:"state"`
	// Attempts counts launches that failed having created nothing.
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	// LaunchID is written by the claim that took the row; the drain acts only
	// on rows whose LaunchID it wrote.
	LaunchID   string    `json:"launch_id,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
	StateAt    time.Time `json:"state_at"`
	// Session is the herdr session whose ledger holds the worker once it is
	// launched. With Repo and Name it names the ledger row (Open(Repo,
	// Session), then the row called Name).
	Session string `json:"session,omitempty"`
	// Profile is the [surface.profiles] name the worker runs under, or empty
	// for the launcher's own CLAUDE_CONFIG_DIR. Only the name is stored; the
	// drain resolves it from the config file when it launches.
	Profile string `json:"profile,omitempty"`
	// Model replaces the launch profile's model for this worker, or is empty.
	Model string `json:"model,omitempty"`
	// Harness is the harness the drain launches the worker with: claude,
	// codex, or pi. A row written before the field existed has none, and
	// runs claude.
	Harness string `json:"harness,omitempty"`
}

// QueueLaunch is what a queue row asks of its launch beyond the brief.
type QueueLaunch struct {
	// Profile is a [surface.profiles] name; "main" is stored as empty.
	Profile string
	// Model is a model name for the harness's --model.
	Model string
	// Harness is claude, codex, or pi; empty is claude.
	Harness string
}

// DefaultQueueHarness is the harness of a row that names none.
const DefaultQueueHarness = "claude"

// queueHarnesses are the harnesses a queue row may name.
var queueHarnesses = []string{"claude", "codex", "pi"}

// ErrProfileNotClaude reports a profile on a row whose harness is not
// claude: a profile sets CLAUDE_CONFIG_DIR, which only claude reads.
var ErrProfileNotClaude = errors.New("worker: a profile sets CLAUDE_CONFIG_DIR and applies to claude workers only")

// normalized fills in the default harness, so a row that names none and a
// row that names claude compare equal.
func (l QueueLaunch) normalized() QueueLaunch {
	if l.Harness == "" {
		l.Harness = DefaultQueueHarness
	}
	return l
}

// check refuses a harness outside the three, a profile for a harness other
// than claude, and a profile name or model outside its shape. Whether the
// profile exists is the config's question, asked at enqueue and at launch.
func (l QueueLaunch) check() error {
	l = l.normalized()
	if !slices.Contains(queueHarnesses, l.Harness) {
		return fmt.Errorf("worker: harness %q: want claude, codex, or pi", l.Harness)
	}
	if l.Profile != "" && l.Harness != "claude" {
		return fmt.Errorf("%w (harness %s)", ErrProfileNotClaude, l.Harness)
	}
	if l.Profile != "" {
		if err := config.CheckProfileName(l.Profile); err != nil {
			return fmt.Errorf("worker: profile %q: %w", l.Profile, err)
		}
	}
	if l.Model != "" {
		if err := config.CheckModelName(l.Model); err != nil {
			return fmt.Errorf("worker: model %q: %w", l.Model, err)
		}
	}
	return nil
}

// Launch is the row's QueueLaunch, with the default harness filled in.
func (r QueueRow) Launch() QueueLaunch {
	return QueueLaunch{Profile: r.Profile, Model: r.Model, Harness: r.Harness}.normalized()
}

// CheckLaunch re-checks a stored row's harness, profile name and model
// shape, for the drain, which reads rows the store checked only for version
// and state.
func (r QueueRow) CheckLaunch() error { return r.Launch().check() }

// queueVersion is the on-disk format version. A file with another version is
// refused rather than rewritten, as the ledger does.
const queueVersion = 1

type queueFile struct {
	Version int        `json:"version"`
	Rows    []QueueRow `json:"rows"`
}

// MaxQueueBytes caps the encoded queue document, well below the 1 MiB read
// cap every file in the surface directory shares.
const MaxQueueBytes = 768 << 10

// maxBatchLen bounds a batch id; it is printed in `surface queue`.
const maxBatchLen = 64

// queueStoreKey names the queue's files: queue.json, queue.json.tmp and
// queue.lock. A ledger key is 32 hex characters, so the names cannot meet.
const queueStoreKey = "queue"

var (
	// ErrQueueUnreadable reports a queue file that exists but cannot be used.
	ErrQueueUnreadable = errors.New("worker: the queue file is unreadable")
	// ErrQueueNoRow reports a name with no queue row.
	ErrQueueNoRow = errors.New("worker: no queue row with that name")
	// ErrQueueNameTaken reports an enqueue whose name already has a row with
	// another brief or repo.
	ErrQueueNameTaken = errors.New("worker: a queue row with that name already exists")
	// ErrQueueFull reports a write that would push the queue past MaxQueueBytes.
	ErrQueueFull = errors.New("worker: the queue is full")
	// ErrQueueRowLive reports a dequeue of a row whose worker may be running.
	ErrQueueRowLive = errors.New("worker: the worker is live; run surface close first")
	// ErrQueueNotQueued reports a claim of a row that is no longer queued.
	ErrQueueNotQueued = errors.New("worker: the queue row is not queued")
	// ErrQueueRowChanged reports a conditional change to a row that no longer
	// matches what the caller read.
	ErrQueueRowChanged = errors.New("worker: the queue row changed since it was read")
	// ErrInvalidBatch reports a batch id outside the allowed shape.
	ErrInvalidBatch = errors.New("worker: batch must be 1-64 characters of a-z, 0-9 and '-', starting with a letter or digit")
)

// BriefSHA256 returns the hex SHA-256 of brief text.
func BriefSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// CheckQueueBrief refuses brief text the queue will not store: anything
// CheckBrief refuses for a launch, and a leading '@', which the launch
// command line reads as "the brief is in this file".
func CheckQueueBrief(text string) error {
	if err := CheckBrief(text, ViaLaunch); err != nil {
		return err
	}
	if strings.HasPrefix(strings.TrimLeft(text, " \t\n"), "@") {
		return fmt.Errorf("%w: a queued brief cannot start with '@'", ErrInvalidBrief)
	}
	return nil
}

// validBatch is ValidName's rule with its own length and error.
func validBatch(batch string) error {
	if batch == "" {
		return nil
	}
	if len(batch) > maxBatchLen {
		return ErrInvalidBatch
	}
	for i, r := range batch {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return ErrInvalidBatch
		}
	}
	return nil
}

func decodeQueue(data []byte) (queueFile, error) {
	if len(data) == 0 {
		return queueFile{Version: queueVersion}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f queueFile
	if err := dec.Decode(&f); err != nil {
		return queueFile{}, fmt.Errorf("%w: %w", ErrQueueUnreadable, err)
	}
	if f.Version != queueVersion {
		return queueFile{}, fmt.Errorf("%w: version %d, this build reads %d", ErrQueueUnreadable, f.Version, queueVersion)
	}
	for _, r := range f.Rows {
		if !r.State.valid() {
			return queueFile{}, fmt.Errorf("%w: row %q has unknown state %q", ErrQueueUnreadable, r.Name, r.State)
		}
	}
	return f, nil
}

func encodeQueue(f queueFile) ([]byte, error) {
	// termsafe:allow-raw-json private 0600 queue file read back by forgectl, never written to a terminal
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// TrimTerminal drops the brief text of every terminal row, keeping its hash.
// Every queue write applies it, so a terminal row never carries a brief.
func TrimTerminal(rows []QueueRow) {
	for i := range rows {
		if rows[i].State.Terminal() {
			rows[i].Brief = ""
		}
	}
}

// errQueueUnchanged tells mutate the change is a no-op: write nothing.
var errQueueUnchanged = errors.New("worker: queue unchanged")

// enqueueRow adds row. When its name is already queued with the same brief and
// repo it returns that row and errQueueUnchanged, so nothing is written.
func enqueueRow(rows []QueueRow, row QueueRow) (out []QueueRow, existing QueueRow, added bool, err error) {
	for _, r := range rows {
		if r.Name != row.Name {
			continue
		}
		if r.Repo != row.Repo {
			return nil, r, false, fmt.Errorf("%w: %q is queued for repo %s (state %s)", ErrQueueNameTaken, r.Name, r.Repo, r.State)
		}
		if r.BriefSHA256 != row.BriefSHA256 {
			return nil, r, false, fmt.Errorf("%w: %q holds brief sha256 %s, this brief is sha256 %s (state %s); dequeue it first to replace it",
				ErrQueueNameTaken, r.Name, r.BriefSHA256, row.BriefSHA256, r.State)
		}
		if r.Launch() != row.Launch() {
			was, want := r.Launch(), row.Launch()
			return nil, r, false, fmt.Errorf("%w: %q is queued with harness %q, profile %q and model %q, this enqueue asks for harness %q, profile %q and model %q (state %s); dequeue it first to replace it",
				ErrQueueNameTaken, r.Name, was.Harness, was.Profile, was.Model, want.Harness, want.Profile, want.Model, r.State)
		}
		return nil, r, false, errQueueUnchanged
	}
	return append(rows, row), row, true, nil
}

// claimRow moves the row named name from queued to claimed, writing launchID
// and, when it is not empty, the herdr session the launch will use.
func claimRow(rows []QueueRow, name, launchID, session string, now time.Time) ([]QueueRow, QueueRow, error) {
	for i := range rows {
		if rows[i].Name != name {
			continue
		}
		if rows[i].State != QueueQueued {
			return nil, rows[i], fmt.Errorf("%w: %q is %s", ErrQueueNotQueued, name, rows[i].State)
		}
		rows[i].State = QueueClaimed
		rows[i].LaunchID = launchID
		if session != "" {
			rows[i].Session = session
		}
		rows[i].StateAt = now
		return rows, rows[i], nil
	}
	return nil, QueueRow{}, ErrQueueNoRow
}

// dequeueRow removes the row named name unless its worker may be live.
func dequeueRow(rows []QueueRow, name string) ([]QueueRow, QueueRow, error) {
	for i, r := range rows {
		if r.Name != name {
			continue
		}
		if r.State.Live() {
			return nil, r, fmt.Errorf("%w (%q is %s)", ErrQueueRowLive, name, r.State)
		}
		return append(rows[:i:i], rows[i+1:]...), r, nil
	}
	return nil, QueueRow{}, ErrQueueNoRow
}

// Queue is the machine's queue of briefs.
type Queue struct {
	store store
}

// OpenQueue returns the queue in $XDG_STATE_HOME/forgectl/surface/queue.json,
// defaulting to ~/.local/state.
func OpenQueue() (*Queue, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return nil, err
	}
	return openQueueAt(base), nil
}

func openQueueAt(stateBase string) *Queue {
	return &Queue{store: newFileStore(stateBase, queueStoreKey)}
}

// Rows returns every row.
func (q *Queue) Rows() ([]QueueRow, error) {
	data, err := q.store.read()
	if err != nil {
		return nil, err
	}
	f, err := decodeQueue(data)
	if err != nil {
		return nil, err
	}
	return f.Rows, nil
}

// Enqueue adds a queued row for name in repo carrying brief. It checks the
// name, the repo path, the batch and the brief, and hashes the brief itself.
// Enqueueing a name already present with the same brief and repo is a no-op
// that returns that row with added false; a different brief or repo is
// ErrQueueNameTaken naming both. A row that would push the document past
// MaxQueueBytes is ErrQueueFull, and nothing is written.
func (q *Queue) Enqueue(name, repo, brief, batch string, now time.Time) (row QueueRow, added bool, err error) {
	return q.EnqueueLaunch(name, repo, brief, batch, QueueLaunch{}, now)
}

// EnqueueLaunch is Enqueue for a row that names a harness, a profile or a
// model. The profile is stored by name ("main" as empty), never as a path,
// and the harness always (claude when none is named). A name already queued
// with another harness, profile or model is ErrQueueNameTaken.
func (q *Queue) EnqueueLaunch(name, repo, brief, batch string, launch QueueLaunch, now time.Time) (row QueueRow, added bool, err error) {
	if launch.Profile == config.MainProfile {
		launch.Profile = ""
	}
	launch = launch.normalized()
	if err := launch.check(); err != nil {
		return QueueRow{}, false, err
	}
	if err := ValidName(name); err != nil {
		return QueueRow{}, false, err
	}
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return QueueRow{}, false, errors.New("worker: a queue row needs a clean absolute repo path")
	}
	if err := validBatch(batch); err != nil {
		return QueueRow{}, false, err
	}
	if err := CheckQueueBrief(brief); err != nil {
		return QueueRow{}, false, err
	}
	now = now.UTC()
	candidate := QueueRow{
		Name: name, Repo: repo, Brief: brief, BriefSHA256: BriefSHA256(brief), Batch: batch,
		State: QueueQueued, EnqueuedAt: now, StateAt: now, Profile: launch.Profile, Model: launch.Model, Harness: launch.Harness,
	}
	err = q.mutate(MaxQueueBytes, func(rows []QueueRow) ([]QueueRow, error) {
		out, existing, ok, err := enqueueRow(rows, candidate)
		row, added = existing, ok
		return out, err
	})
	if err != nil {
		return row, false, err
	}
	return row, added, nil
}

// Dequeue removes the row named name and returns it, refusing a row whose
// worker may be live (claimed, launched, needs-you) with ErrQueueRowLive.
func (q *Queue) Dequeue(name string) (QueueRow, error) {
	var removed QueueRow
	err := q.mutate(maxLedgerBytes, func(rows []QueueRow) ([]QueueRow, error) {
		out, r, err := dequeueRow(rows, name)
		removed = r
		return out, err
	})
	return removed, err
}

// Claim moves the row named name from queued to claimed under the queue lock
// and records launchID, which must not be empty. Of two claimers of one row,
// exactly one succeeds; the other gets ErrQueueNotQueued.
func (q *Queue) Claim(name, launchID string, now time.Time) (QueueRow, error) {
	return q.ClaimFor(name, launchID, "", now)
}

// ClaimFor is Claim that also records session, the herdr session whose
// ledger the launch will write, in the same locked write. The drain uses it,
// so a claimed row always names the ledger a restart must read.
func (q *Queue) ClaimFor(name, launchID, session string, now time.Time) (QueueRow, error) {
	if launchID == "" {
		return QueueRow{}, errors.New("worker: a claim needs a launch id")
	}
	var claimed QueueRow
	err := q.mutate(maxLedgerBytes, func(rows []QueueRow) ([]QueueRow, error) {
		out, r, err := claimRow(rows, name, launchID, session, now.UTC())
		claimed = r
		return out, err
	})
	return claimed, err
}

// RemoveIf removes the row named name when match accepts it, and returns the
// row removed. It is the drain's prune: a row can be dequeued and enqueued
// again between the drain's read and its remove, and match (the row as read)
// keeps the new one. A row that no longer matches is ErrQueueRowChanged.
func (q *Queue) RemoveIf(name string, match func(QueueRow) bool) (QueueRow, error) {
	var removed QueueRow
	err := q.mutate(maxLedgerBytes, func(rows []QueueRow) ([]QueueRow, error) {
		for i := range rows {
			if rows[i].Name != name {
				continue
			}
			if !match(rows[i]) {
				return nil, ErrQueueRowChanged
			}
			removed = rows[i]
			return append(rows[:i:i], rows[i+1:]...), nil
		}
		return nil, ErrQueueNoRow
	})
	return removed, err
}

// SameRead matches a row that is still exactly as read: the same launch, the
// same state entered at the same time, and the same brief enqueued at the
// same time. Every drain write but the claim passes it to UpdateIf or
// RemoveIf, so a row the operator dequeued and enqueued again, or another
// writer moved on, is left alone.
func SameRead(was QueueRow) func(QueueRow) bool {
	return func(r QueueRow) bool {
		return r.LaunchID == was.LaunchID && r.State == was.State && r.StateAt.Equal(was.StateAt) &&
			r.EnqueuedAt.Equal(was.EnqueuedAt) && r.BriefSHA256 == was.BriefSHA256 && r.Repo == was.Repo
	}
}

// maxLastError caps a row's LastError, so a long error from a launch cannot
// grow the queue toward its read cap.
const maxLastError = 1024

// capLastError cuts s to maxLastError bytes on a rune boundary.
func capLastError(s string) string {
	if len(s) <= maxLastError {
		return s
	}
	cut := maxLastError
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// UpdateIf changes the row named name under the queue lock when match
// accepts it, and returns the row as written. A change of State moves
// StateAt to now. fn may not rename the row. Which state changes are allowed
// is the caller's rule: only Claim enforces one (queued to claimed).
func (q *Queue) UpdateIf(name string, match func(QueueRow) bool, now time.Time, fn func(*QueueRow)) (QueueRow, error) {
	var updated QueueRow
	err := q.mutate(maxLedgerBytes, func(rows []QueueRow) ([]QueueRow, error) {
		for i := range rows {
			if rows[i].Name != name {
				continue
			}
			if !match(rows[i]) {
				return nil, ErrQueueRowChanged
			}
			before := rows[i].State
			fn(&rows[i])
			if rows[i].Name != name {
				return nil, errors.New("worker: a queue update cannot rename a row")
			}
			if !rows[i].State.valid() {
				return nil, fmt.Errorf("worker: unknown queue state %q", rows[i].State)
			}
			if rows[i].State != before {
				rows[i].StateAt = now.UTC()
			}
			rows[i].LastError = capLastError(rows[i].LastError)
			// mutate trims every row before writing; trimming here too makes
			// the returned row match what was written.
			TrimTerminal(rows[i : i+1])
			updated = rows[i]
			return rows, nil
		}
		return nil, ErrQueueNoRow
	})
	return updated, err
}

// mutate runs fn over the rows under the lock, trims terminal rows, and
// refuses, before writing anything, a document that grows past limit. Only
// Enqueue passes MaxQueueBytes: the drain's own writes (a claim, a failure)
// must not be refused by a queue the operator filled, so they pass the read
// cap, which leaves 256 KiB of headroom. A write that shrinks the document is
// never refused. fn returns errQueueUnchanged to write nothing and report
// success.
func (q *Queue) mutate(limit int, fn func([]QueueRow) ([]QueueRow, error)) error {
	err := q.store.update(func(data []byte) ([]byte, error) {
		f, err := decodeQueue(data)
		if err != nil {
			return nil, err
		}
		rows, err := fn(f.Rows)
		if err != nil {
			return nil, err
		}
		TrimTerminal(rows)
		f.Rows = rows
		next, err := encodeQueue(f)
		if err != nil {
			return nil, err
		}
		if len(next) > limit && len(next) > len(data) {
			return nil, fmt.Errorf("%w: the queue file is %d bytes and this change would make it %d, limit %d; dequeue finished rows first",
				ErrQueueFull, len(data), len(next), limit)
		}
		return next, nil
	})
	if errors.Is(err, errQueueUnchanged) {
		return nil
	}
	return err
}
