// Package drain holds the decisions `forgectl surface drain` makes on each
// tick, as pure functions over what the tick read: the queue rows, each
// row's ledger row, one screen read per live worker, the clock and the
// settings. Nothing here reads a file, runs a command or sleeps; the CLI
// reads, calls these, and writes what they return.
package drain

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const (
	// MaxAttempts is how many launches of one row may fail having created
	// nothing before the row is failed.
	MaxAttempts = 3
	// ExpireAfter is how long a row may sit queued before it expires.
	ExpireAfter = 7 * 24 * time.Hour
	// PruneAfter is how long a terminal row stays before it is removed.
	PruneAfter = 30 * 24 * time.Hour
	// StaleTicks is how many intervals may pass without a tick before a
	// running drain reads as stale.
	StaleTicks = 3
	// WorkerBranchPrefix starts every drain worker's branch name.
	WorkerBranchPrefix = "worker/"
)

// Reasons a row is in needs-you, kept in its last_error.
const (
	// ReasonIdle: at its prompt with no report for idle_minutes.
	ReasonIdle = "idle without report"
	// reasonBlockedPrefix starts a blocked reason; the screen's name follows.
	reasonBlockedPrefix = "blocked: "
)

// Branch is the branch a drain worker named name runs on.
func Branch(name string) string { return WorkerBranchPrefix + name }

// LedgerState is what the tick found in a queue row's ledger.
type LedgerState int

const (
	// LedgerAbsent: no ledger row has the queue row's name.
	LedgerAbsent LedgerState = iota
	// LedgerOther: a ledger row has the name but another launch id.
	LedgerOther
	// LedgerOurs: the ledger row the drain's claim launched.
	LedgerOurs
	// LedgerUnreadable: the ledger could not be read.
	LedgerUnreadable
)

// Ledger is a queue row's ledger row as the tick read it.
type Ledger struct {
	State LedgerState
	Row   worker.Row
	// Err says why the ledger is unreadable.
	Err string
}

// MatchLedger finds q's ledger row in rows: the row with q's name, which is
// the drain's only when its launch id is q's. err is the ledger read's error.
func MatchLedger(q worker.QueueRow, rows []worker.Row, err error) Ledger {
	if err != nil {
		return Ledger{State: LedgerUnreadable, Err: err.Error()}
	}
	for _, r := range rows {
		if r.Name != q.Name {
			continue
		}
		if q.LaunchID == "" || r.LaunchID != q.LaunchID {
			return Ledger{State: LedgerOther, Row: r}
		}
		return Ledger{State: LedgerOurs, Row: r}
	}
	return Ledger{State: LedgerAbsent}
}

// Change is what a decision does to one queue row. The zero Change (beyond
// Name and From) does nothing.
type Change struct {
	Name string
	From worker.QueueState
	// To is the new state, or "" to keep it.
	To worker.QueueState
	// Error is the new last_error. It is written whenever To is set, and
	// otherwise only with SetError.
	Error    string
	SetError bool
	// Attempts is the new attempt count, written with SetAttempts.
	Attempts    int
	SetAttempts bool
	// ClearLaunch drops the row's launch id: it goes back to the queue and
	// the next claim writes a fresh one.
	ClearLaunch bool
	// Notify asks for one needs-you notification: the row is entering
	// needs-you.
	Notify bool
	// Note is a non-state event to record once, such as a row becoming
	// unreadable.
	Note string
}

// Writes reports whether c changes the row on disk.
func (c Change) Writes() bool {
	return c.To != "" || c.SetError || c.SetAttempts || c.ClearLaunch
}

// Apply writes c into r.
func (c Change) Apply(r *worker.QueueRow) {
	if c.To != "" {
		r.State = c.To
		r.LastError = c.Error
	} else if c.SetError {
		r.LastError = c.Error
	}
	if c.SetAttempts {
		r.Attempts = c.Attempts
	}
	if c.ClearLaunch {
		r.LaunchID = ""
	}
}

func change(q worker.QueueRow) Change { return Change{Name: q.Name, From: q.State} }

func (c Change) to(s worker.QueueState, why string) Change {
	c.To, c.Error = s, why
	return c
}

// HoldsSlot reports whether q holds one of the drain's slots. A claimed row
// holds one. Otherwise holding is read from the ledger, not the queue state:
// a row whose ledger row is pending, worktree or launched holds its slot
// whatever the queue says, so a failed row with a live worker keeps its
// repo's slot until the operator closes it. A reported row frees its slot
// (the plan's Known boundary). An unreadable ledger holds the slot of a row
// that may have a live worker, since nothing proves it ended.
func HoldsSlot(q worker.QueueRow, l Ledger) bool {
	switch {
	case q.State == worker.QueueClaimed:
		return true
	case q.State == worker.QueueReported, q.State == worker.QueueQueued:
		return false
	case l.State == LedgerUnreadable:
		return q.State == worker.QueueLaunched || q.State == worker.QueueNeedsYou || q.State == worker.QueueFailed
	case l.State != LedgerOurs:
		return false
	}
	switch l.Row.Stage {
	case worker.StagePending, worker.StageWorktree, worker.StageLaunched:
		return true
	}
	return false
}

// PlanClaims returns the queued rows to claim this tick, oldest first, while
// the global cap and the per-repo cap allow. ledgers is keyed by row name.
func PlanClaims(rows []worker.QueueRow, ledgers map[string]Ledger, s config.DrainSettings) []worker.QueueRow {
	held, perRepo := 0, map[string]int{}
	var queued []worker.QueueRow
	for _, r := range rows {
		if r.State == worker.QueueQueued {
			queued = append(queued, r)
			continue
		}
		if HoldsSlot(r, ledgers[r.Name]) {
			held++
			perRepo[r.Repo]++
		}
	}
	slices.SortStableFunc(queued, func(a, b worker.QueueRow) int {
		if c := a.EnqueuedAt.Compare(b.EnqueuedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	var out []worker.QueueRow
	for _, r := range queued {
		if held >= s.Cap {
			break
		}
		if perRepo[r.Repo] >= s.PerRepo {
			continue
		}
		out = append(out, r)
		held++
		perRepo[r.Repo]++
	}
	return out
}

// CheckClaimed re-checks a claimed row before it is launched: the store
// checks only version, fields and state when it reads, so the repo must be a
// clean absolute path, the brief must pass the queue's brief check, and its
// hash must be the one recorded at enqueue.
func CheckClaimed(q worker.QueueRow) error {
	if !filepath.IsAbs(q.Repo) || filepath.Clean(q.Repo) != q.Repo {
		return fmt.Errorf("the row's repo %q is not a clean absolute path", q.Repo)
	}
	if err := worker.CheckQueueBrief(q.Brief); err != nil {
		return fmt.Errorf("the row's brief fails the queue check: %w", err)
	}
	if got := worker.BriefSHA256(q.Brief); got != q.BriefSHA256 {
		return fmt.Errorf("the row's brief hashes to sha256 %s, expected the enqueued sha256 %s", got, q.BriefSHA256)
	}
	return nil
}

// Expire moves a row queued for longer than ExpireAfter to expired.
func Expire(q worker.QueueRow, now time.Time) Change {
	c := change(q)
	if q.State != worker.QueueQueued || now.Sub(q.EnqueuedAt) <= ExpireAfter {
		return c
	}
	return c.to(worker.QueueExpired, fmt.Sprintf("queued since %s, more than %s, and never launched", q.EnqueuedAt.UTC().Format(time.RFC3339), ExpireAfter))
}

// Prunable reports whether a terminal row has been in its state longer than
// PruneAfter and may be removed.
func Prunable(q worker.QueueRow, now time.Time) bool {
	return q.State.Terminal() && now.Sub(q.StateAt) > PruneAfter
}
