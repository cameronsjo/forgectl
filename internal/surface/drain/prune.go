package drain

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// Prune (atelier P4, T10.3) decides which old rows `surface prune` and the
// drain's daily prune remove: closed, failed, expired and reported queue
// rows, and closed ledger rows, older than a cutoff. A queue row whose own
// ledger row still has a live herdr workspace, or may be in a ledger file
// that could not be read, stays; when herdr cannot be read no ledger row is
// removed at all.

// Workspace is what is known about a ledger row's herdr workspace.
type Workspace int

const (
	// WorkspaceNone: there is nothing to ask herdr about: the row recorded
	// no workspace, or surface close already closed it (stage closed).
	WorkspaceNone Workspace = iota
	// WorkspaceGone: herdr proved the workspace absent.
	WorkspaceGone
	// WorkspaceLive: herdr says the workspace exists.
	WorkspaceLive
	// WorkspaceUnknown: herdr could not say.
	WorkspaceUnknown
)

// PruneLedger is one ledger as prune read it.
type PruneLedger struct {
	ID   worker.LedgerID
	Rows []worker.Row
	// Err says why the ledger could not be read; its rows are then unknown.
	Err string
}

// PruneInput is everything a prune decision reads.
type PruneInput struct {
	Now       time.Time
	OlderThan time.Duration
	Queue     []worker.QueueRow
	Ledgers   []PruneLedger
	// HerdrErr, when set, says herdr cannot be read: no ledger row is
	// removed, and a queue row whose own ledger row may have a workspace is
	// kept.
	HerdrErr string
	// Workspace asks herdr about one ledger row's workspace. It is called
	// only for a row that recorded one and is not closed, and only when
	// HerdrErr is empty.
	Workspace func(id worker.LedgerID, row worker.Row) Workspace
	// Cache is the status cache's entries. One older than the cutoff goes:
	// an entry serves only the head it names, and rows do not record heads.
	Cache []worker.StatusCacheEntry
	// BadLedgers names the ledger files that could not be read at all (no
	// repo or session is known for them). While any is listed, a launched
	// queue row whose ledger is not among Ledgers is kept: its ledger row
	// may be in one of them.
	BadLedgers []string
	// WorktreeGone reports whether a closed ledger row's recorded worktree
	// path no longer exists; an error means it cannot say. It is asked only
	// for a closed row with no closed_at.
	WorktreeGone func(path string) (bool, error)
}

// Prune item kinds.
const (
	PruneKindQueue  = "queue"
	PruneKindLedger = "ledger"
	PruneKindCache  = "status-cache"
)

// PruneItem is one row prune removes or keeps, with the reason.
type PruneItem struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Session string `json:"session,omitempty"`
	// State is the queue state or the ledger stage.
	State string `json:"state"`
	// At is the queue row's state_at, or the ledger row's closed_at
	// (started_at for a row closed before closed_at was recorded).
	At      time.Time `json:"at"`
	CostUSD *float64  `json:"cost_usd,omitempty"`
	Reason  string    `json:"reason"`

	queueRow  worker.QueueRow
	ledgerRow worker.Row
	ledgerID  worker.LedgerID
	cache     worker.StatusCacheEntry
}

// QueueRow is the queue row a queue item names.
func (it PruneItem) QueueRow() worker.QueueRow { return it.queueRow }

// LedgerRow is the ledger row a ledger item names, and its ledger.
func (it PruneItem) LedgerRow() (worker.LedgerID, worker.Row) { return it.ledgerID, it.ledgerRow }

// CacheEntry is the status cache entry a cache item names.
func (it PruneItem) CacheEntry() worker.StatusCacheEntry { return it.cache }

// PrunePlan is what a prune removes and keeps.
type PrunePlan struct {
	Remove []PruneItem
	Keep   []PruneItem
	// Notes say what was skipped wholesale: herdr unreadable, a ledger
	// that could not be read.
	Notes []string
}

// prunableQueueStates are the queue states prune removes. A reported row is
// among them: one that neither closer nor surface close settled for the
// whole cutoff goes once its worker's workspace is not live, so a row with
// no PR and no operator does not live forever.
var prunableQueueStates = []worker.QueueState{worker.QueueClosed, worker.QueueFailed, worker.QueueExpired, worker.QueueReported}

// PlanPrune decides what to remove. It is pure but for in.Workspace.
func PlanPrune(in PruneInput) PrunePlan {
	var p PrunePlan
	cutoff := in.Now.Add(-in.OlderThan)
	ledgers := make(map[worker.LedgerID]PruneLedger, len(in.Ledgers))
	for _, l := range in.Ledgers {
		ledgers[l.ID] = l
		if l.Err != "" {
			p.Notes = append(p.Notes, fmt.Sprintf("the ledger for %s (herdr session %s) could not be read, so none of its rows is removed: %s", l.ID.Repo, l.ID.Session, l.Err))
		}
	}
	if in.HerdrErr != "" {
		p.Notes = append(p.Notes, "herdr could not be read, so no ledger row is removed: "+in.HerdrErr)
	}
	workspace := func(id worker.LedgerID, r worker.Row) Workspace {
		switch {
		case r.Stage == worker.StageClosed || len(r.Ref) == 0:
			return WorkspaceNone
		case in.HerdrErr != "" || in.Workspace == nil:
			return WorkspaceUnknown
		}
		return in.Workspace(id, r)
	}

	for _, q := range in.Queue {
		it := PruneItem{Kind: PruneKindQueue, Name: q.Name, Repo: q.Repo, Session: q.Session, State: string(q.State), At: q.StateAt, CostUSD: q.CostUSD, queueRow: q}
		switch {
		case !slices.Contains(prunableQueueStates, q.State):
			it.Reason = fmt.Sprintf("state %s is not pruned (only closed, failed, expired and reported rows are)", q.State)
		case !q.StateAt.Before(cutoff):
			it.Reason = fmt.Sprintf("in state %s since %s, not older than the cutoff %s", q.State, q.StateAt.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339))
		default:
			it.Reason = queueKeepReason(q, ledgers, in.BadLedgers, workspace)
			if it.Reason == "" {
				it.Reason = fmt.Sprintf("%s since %s, older than the cutoff", q.State, q.StateAt.UTC().Format(time.RFC3339))
				p.Remove = append(p.Remove, it)
				continue
			}
		}
		p.Keep = append(p.Keep, it)
	}

	for _, l := range in.Ledgers {
		for _, r := range l.Rows {
			it := PruneItem{Kind: PruneKindLedger, Name: r.Name, Repo: l.ID.Repo, Session: l.ID.Session, State: string(r.Stage), At: r.StartedAt, ledgerRow: r, ledgerID: l.ID}
			if r.ClosedAt != nil {
				it.At = *r.ClosedAt
			}
			switch {
			case in.HerdrErr != "":
				it.Reason = "herdr could not be read; no ledger row is removed"
			case r.Stage != worker.StageClosed:
				it.Reason = fmt.Sprintf("stage %s is not pruned (only closed rows are)", r.Stage)
			case !it.At.Before(cutoff):
				it.Reason = fmt.Sprintf("closed %s, not older than the cutoff %s", it.At.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339))
				if r.ClosedAt == nil {
					it.Reason = fmt.Sprintf("started %s and has no closed_at, not older than the cutoff %s", it.At.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339))
				}
			case r.ClosedAt != nil:
				it.Reason = fmt.Sprintf("closed %s, older than the cutoff", it.At.UTC().Format(time.RFC3339))
				p.Remove = append(p.Remove, it)
				continue
			default:
				// Closed before closed_at was recorded: the close time is
				// unknown, and the row is the only record of the worktree it
				// kept, so it goes only once that worktree is gone.
				it.Reason = closedWithoutTimeReason(r, in.WorktreeGone)
				if it.Reason == "" {
					it.Reason = fmt.Sprintf("closed with no closed_at, started %s, and its worktree %s no longer exists", it.At.UTC().Format(time.RFC3339), r.Worktree)
					p.Remove = append(p.Remove, it)
					continue
				}
			}
			p.Keep = append(p.Keep, it)
		}
	}

	for _, e := range in.Cache {
		if e.ModTime.Before(cutoff) {
			p.Remove = append(p.Remove, PruneItem{Kind: PruneKindCache, Name: e.Head, State: "cached", At: e.ModTime,
				Reason: "written " + e.ModTime.UTC().Format(time.RFC3339) + ", older than the cutoff", cache: e})
		}
	}
	return p
}

// closedWithoutTimeReason returns why a closed ledger row with no closed_at
// must stay, or "" when its recorded worktree is gone.
func closedWithoutTimeReason(r worker.Row, gone func(string) (bool, error)) string {
	if r.Worktree == "" {
		return fmt.Sprintf("closed with no closed_at and no recorded worktree path, so prune cannot tell its worktree is gone; run surface close %s", r.Name)
	}
	if gone == nil {
		return "closed with no closed_at, and its worktree cannot be checked"
	}
	ok, err := gone(r.Worktree)
	switch {
	case err != nil:
		return fmt.Sprintf("closed with no closed_at, and its worktree %s cannot be checked: %v", r.Worktree, err)
	case !ok:
		return fmt.Sprintf("closed with no closed_at, and its worktree %s still exists; run surface close %s once its work is saved", r.Worktree, r.Name)
	}
	return ""
}

// queueKeepReason returns why an old queue row must stay, or "": its own
// ledger row (same name and launch id) cannot be read, may be in a ledger
// file that could not be read at all, or has a workspace herdr says is live
// or cannot rule out.
func queueKeepReason(q worker.QueueRow, ledgers map[worker.LedgerID]PruneLedger, bad []string, workspace func(worker.LedgerID, worker.Row) Workspace) string {
	if q.LaunchID == "" || q.Session == "" {
		return "" // never launched, or nothing of its launch was left
	}
	id := worker.LedgerID{Repo: q.Repo, Session: q.Session}
	l, ok := ledgers[id]
	if !ok {
		if len(bad) > 0 {
			return "its ledger may be in a ledger file that could not be read: " + strings.Join(bad, ", ")
		}
		return ""
	}
	if l.Err != "" {
		return "its ledger could not be read: " + l.Err
	}
	for _, r := range l.Rows {
		if r.Name != q.Name || r.LaunchID != q.LaunchID {
			continue
		}
		switch workspace(id, r) {
		case WorkspaceLive:
			return fmt.Sprintf("its worker's herdr workspace is still live; run surface close %s", q.Name)
		case WorkspaceUnknown:
			return "herdr cannot say whether its worker's workspace is still live"
		}
		return ""
	}
	return ""
}

// UsageLine is one line of usage-daily.jsonl: the cost of one queue row
// prune removed. Day is the UTC day of its state_at. A row is identified by
// Name and LaunchID, so a reader that sums the unique (name, launch_id)
// lines per day counts each row once even if a failed prune wrote its line
// twice.
type UsageLine struct {
	Day      string  `json:"day"`
	Name     string  `json:"name"`
	LaunchID string  `json:"launch_id"`
	CostUSD  float64 `json:"costUsd"`
}

// UsageLines is one line per priced row, oldest day first, then by name.
// Rows without a cost are not counted.
func UsageLines(rows []worker.QueueRow) []UsageLine {
	out := []UsageLine{}
	for _, r := range rows {
		if r.CostUSD == nil {
			continue
		}
		out = append(out, UsageLine{Day: r.StateAt.UTC().Format(worker.UTCDayLayout), Name: r.Name, LaunchID: r.LaunchID, CostUSD: *r.CostUSD})
	}
	slices.SortFunc(out, func(a, b UsageLine) int {
		if c := strings.Compare(a.Day, b.Day); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// PruneDue reports whether the drain's daily prune should run at now, given
// the UTC day it last ran ("" for never).
func PruneDue(lastDay string, now time.Time) bool {
	return lastDay != now.UTC().Format(worker.UTCDayLayout)
}
