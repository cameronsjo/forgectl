package sessions

import (
	"sort"
	"time"
)

// SessionRow is one operational-index row bound for the mart's `session`
// table. Field shapes mirror the mart schema (scripts/sessions-mart/schema.sql
// in cameronsjo/claude-configurations).
type SessionRow struct {
	SessionID     string
	Machine       string
	Project       string
	GitBranch     string
	Model         string
	FirstTs       *time.Time
	LastTs        *time.Time
	Tokens        Tokens
	CostUSD       *float64
	CostSource    string
	Committed     bool
	LastMessageID string
}

// CostSource values recorded on each row — which ledger priced it.
const (
	CostFromCommits  = "commits.jsonl"
	CostFromSessions = "sessions.jsonl"
)

// CommitAttribution is the ADR-0017 fold over commits.jsonl: per-root cost
// sums, the set of roots that committed at all (a commit row with no cost
// still proves committedness), and a count of rows that carried no session id
// — surfaced on the receipt, never silently dropped.
type CommitAttribution struct {
	Costs     map[string]float64
	Committed map[string]bool
	Dropped   int
}

// RootCostMap aggregates commits.jsonl per ADR-0017: cost groups by the ROOT
// session (parentSessionId when present, else sessionId), summed.
func RootCostMap(commitRows []LedgerRow) CommitAttribution {
	att := CommitAttribution{
		Costs:     make(map[string]float64),
		Committed: make(map[string]bool),
	}
	for _, r := range commitRows {
		root := r.ParentSessionID
		if root == "" {
			root = r.SessionID
		}
		if root == "" {
			att.Dropped++
			continue
		}
		att.Committed[root] = true
		if r.CostUSD != nil {
			att.Costs[root] += *r.CostUSD
		}
	}
	return att
}

// BuildSessions folds the sessions.jsonl ledger into one row per session_id.
// Pure: same inputs, same rows.
//
// Merge rules:
//   - Scalar fields: the LATEST row wins (a resume/rewrite row supersedes an
//     earlier SessionEnd for the same id; each /clear segment is its own id).
//     Row order in the ledger is append order; ts breaks ties when present.
//   - FirstTs = earliest startTs seen; LastTs = latest endTs seen.
//   - Cost: ADR-0017 — commits.jsonl root aggregation wins when the session
//     is a cost root; the SessionEnd total is the fallback.
//
// Rows with no sessionId cannot be indexed; they come back in `invalid` so
// the receipt can surface them instead of swallowing.
func BuildSessions(ledger []LedgerRow, att CommitAttribution, machine string) (rows []SessionRow, invalid int) {
	type acc struct {
		latest  LedgerRow
		firstTs *time.Time
		lastTs  *time.Time
		order   int // ledger position of `latest`, for latest-wins
	}
	byID := make(map[string]*acc)
	for i, r := range ledger {
		if r.SessionID == "" {
			invalid++
			continue
		}
		a, ok := byID[r.SessionID]
		if !ok {
			a = &acc{latest: r, order: i}
			byID[r.SessionID] = a
		} else if i >= a.order {
			a.latest = r
			a.order = i
		}
		if r.StartTs != nil && (a.firstTs == nil || r.StartTs.Before(*a.firstTs)) {
			a.firstTs = r.StartTs
		}
		if r.EndTs != nil && (a.lastTs == nil || r.EndTs.After(*a.lastTs)) {
			a.lastTs = r.EndTs
		}
	}

	rows = make([]SessionRow, 0, len(byID))
	for id, a := range byID {
		row := SessionRow{
			SessionID:     id,
			Machine:       machine,
			Project:       a.latest.Repo,
			GitBranch:     a.latest.Branch,
			Model:         a.latest.Model,
			FirstTs:       a.firstTs,
			LastTs:        a.lastTs,
			Tokens:        a.latest.Tokens,
			LastMessageID: a.latest.LastMessageID,
		}
		row.Committed = att.Committed[id]
		if cost, ok := att.Costs[id]; ok {
			// ADR-0017: never recompute when a commit exists.
			c := cost
			row.CostUSD = &c
			row.CostSource = CostFromCommits
		} else if a.latest.CostUSD != nil {
			row.CostUSD = a.latest.CostUSD
			row.CostSource = CostFromSessions
		}
		rows = append(rows, row)
	}
	// Deterministic output order — receipts and tests read stably.
	sort.Slice(rows, func(i, j int) bool { return rows[i].SessionID < rows[j].SessionID })
	return rows, invalid
}
