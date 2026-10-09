package drain

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// Prune (atelier P4, T10.3) decides which old rows `surface prune` and the
// drain's daily prune remove: closed, failed and expired queue rows, and
// closed ledger rows, older than a cutoff. A queue row whose own ledger row
// still has a live herdr workspace stays, and when herdr cannot be read no
// ledger row is removed at all.

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
	// At is the queue row's state_at, or the ledger row's started_at.
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

// prunableQueueStates are the queue states prune removes. A reported row
// waits for the closer, or for surface close.
var prunableQueueStates = []worker.QueueState{worker.QueueClosed, worker.QueueFailed, worker.QueueExpired}

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
			it.Reason = fmt.Sprintf("state %s is not pruned (only closed, failed and expired rows are)", q.State)
		case !q.StateAt.Before(cutoff):
			it.Reason = fmt.Sprintf("in state %s since %s, not older than the cutoff %s", q.State, q.StateAt.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339))
		default:
			it.Reason = queueKeepReason(q, ledgers, workspace)
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
			switch {
			case in.HerdrErr != "":
				it.Reason = "herdr could not be read; no ledger row is removed"
			case r.Stage != worker.StageClosed:
				it.Reason = fmt.Sprintf("stage %s is not pruned (only closed rows are)", r.Stage)
			case !r.StartedAt.Before(cutoff):
				it.Reason = fmt.Sprintf("started %s, not older than the cutoff %s", r.StartedAt.UTC().Format(time.RFC3339), cutoff.UTC().Format(time.RFC3339))
			default:
				it.Reason = fmt.Sprintf("closed, started %s, older than the cutoff", r.StartedAt.UTC().Format(time.RFC3339))
				p.Remove = append(p.Remove, it)
				continue
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

// queueKeepReason returns why an old queue row must stay, or "": its own
// ledger row (same name and launch id) cannot be read, or has a workspace
// herdr says is live or cannot rule out.
func queueKeepReason(q worker.QueueRow, ledgers map[worker.LedgerID]PruneLedger, workspace func(worker.LedgerID, worker.Row) Workspace) string {
	if q.LaunchID == "" || q.Session == "" {
		return "" // never launched, or nothing of its launch was left
	}
	id := worker.LedgerID{Repo: q.Repo, Session: q.Session}
	l, ok := ledgers[id]
	if !ok {
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

// UsageDay is one line of usage-daily.jsonl: the cost of the rows one prune
// removed whose state_at falls on Day (UTC).
type UsageDay struct {
	Day     string  `json:"day"`
	CostUSD float64 `json:"costUsd"`
	Rows    int     `json:"rows"`
}

// UsageRollup sums the cost of the priced rows by the UTC day of their
// state_at, oldest day first. Rows without a cost are not counted.
func UsageRollup(rows []worker.QueueRow) []UsageDay {
	by := map[string]*UsageDay{}
	for _, r := range rows {
		if r.CostUSD == nil {
			continue
		}
		day := r.StateAt.UTC().Format(worker.UTCDayLayout)
		u, ok := by[day]
		if !ok {
			u = &UsageDay{Day: day}
			by[day] = u
		}
		u.CostUSD += *r.CostUSD
		u.Rows++
	}
	out := make([]UsageDay, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b UsageDay) int { return strings.Compare(a.Day, b.Day) })
	return out
}

// PruneDue reports whether the drain's daily prune should run at now, given
// the UTC day it last ran ("" for never).
func PruneDue(lastDay string, now time.Time) bool {
	return lastDay != now.UTC().Format(worker.UTCDayLayout)
}
