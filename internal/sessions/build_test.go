package sessions

import (
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }

func ts(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestRootCostMap(t *testing.T) {
	tests := []struct {
		name          string
		rows          []LedgerRow
		wantCosts     map[string]float64
		wantCommitted []string
		wantDropped   int
	}{
		{
			name: "groups by parentSessionId when present, else sessionId (ADR-0017)",
			rows: []LedgerRow{
				{SessionID: "child-1", ParentSessionID: "root-a", CostUSD: f64(1.5)},
				{SessionID: "child-2", ParentSessionID: "root-a", CostUSD: f64(0.5)},
				{SessionID: "root-b", CostUSD: f64(2.0)},
			},
			wantCosts:     map[string]float64{"root-a": 2.0, "root-b": 2.0},
			wantCommitted: []string{"root-a", "root-b"},
		},
		{
			name: "costless row still proves committedness; id-less row is counted dropped",
			rows: []LedgerRow{
				{SessionID: "x"},
				{CostUSD: f64(9.9)},
			},
			wantCosts:     map[string]float64{},
			wantCommitted: []string{"x"},
			wantDropped:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RootCostMap(tt.rows)
			if len(got.Costs) != len(tt.wantCosts) {
				t.Fatalf("got %d cost roots, want %d: %v", len(got.Costs), len(tt.wantCosts), got.Costs)
			}
			for k, v := range tt.wantCosts {
				if got.Costs[k] != v {
					t.Errorf("root %s: got %v, want %v", k, got.Costs[k], v)
				}
			}
			if len(got.Committed) != len(tt.wantCommitted) {
				t.Fatalf("got %d committed roots, want %d: %v", len(got.Committed), len(tt.wantCommitted), got.Committed)
			}
			for _, id := range tt.wantCommitted {
				if !got.Committed[id] {
					t.Errorf("root %s should be committed", id)
				}
			}
			if got.Dropped != tt.wantDropped {
				t.Errorf("dropped = %d, want %d", got.Dropped, tt.wantDropped)
			}
		})
	}
}

func TestBuildSessionsMergeAndCost(t *testing.T) {
	ledger := []LedgerRow{
		// Two rows for one session: the later row supersedes scalars; the
		// time range spans both.
		{SessionID: "s1", Repo: "alpha", Branch: "main", Model: "m-old",
			StartTs: ts("2026-07-01T10:00:00Z"), EndTs: ts("2026-07-01T11:00:00Z"),
			CostUSD: f64(1.0), LastMessageID: "msg-early",
			Tokens: Tokens{Input: 10, Output: 20}},
		{SessionID: "s1", Repo: "alpha", Branch: "feat/x", Model: "m-new",
			StartTs: ts("2026-07-01T11:00:00Z"), EndTs: ts("2026-07-01T12:30:00Z"),
			CostUSD: f64(3.5), LastMessageID: "msg-late",
			Tokens: Tokens{Input: 100, Output: 200}},
		// A committed session priced from commits.jsonl, never its own total.
		{SessionID: "s2", Repo: "beta", CostUSD: f64(99.0), LastMessageID: "msg-s2"},
		// A row with no sessionId is invalid, surfaced not swallowed.
		{Repo: "gamma"},
	}
	att := CommitAttribution{
		Costs:     map[string]float64{"s2": 4.25},
		Committed: map[string]bool{"s2": true},
	}

	rows, invalid := BuildSessions(ledger, att, "test-machine")

	if invalid != 1 {
		t.Errorf("invalid = %d, want 1", invalid)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	s1 := rows[0]
	if s1.SessionID != "s1" {
		t.Fatalf("rows not sorted by id: %v", rows)
	}
	if s1.Machine != "test-machine" {
		t.Errorf("machine = %q", s1.Machine)
	}
	if s1.GitBranch != "feat/x" || s1.Model != "m-new" || s1.LastMessageID != "msg-late" {
		t.Errorf("latest row did not win scalars: %+v", s1)
	}
	if !s1.FirstTs.Equal(*ts("2026-07-01T10:00:00Z")) || !s1.LastTs.Equal(*ts("2026-07-01T12:30:00Z")) {
		t.Errorf("time range not spanned: first=%v last=%v", s1.FirstTs, s1.LastTs)
	}
	if s1.CostSource != CostFromSessions || *s1.CostUSD != 3.5 || s1.Committed {
		t.Errorf("uncommitted session should price from sessions.jsonl latest row: %+v", s1)
	}
	if s1.Tokens.Input != 100 || s1.Tokens.Output != 200 {
		t.Errorf("tokens should come from latest row: %+v", s1.Tokens)
	}

	s2 := rows[1]
	if s2.CostSource != CostFromCommits || *s2.CostUSD != 4.25 || !s2.Committed {
		t.Errorf("committed session must price from commits.jsonl (ADR-0017), got %+v", s2)
	}
}

func TestBuildSessionsEmptyLedger(t *testing.T) {
	rows, invalid := BuildSessions(nil, RootCostMap(nil), "m")
	if len(rows) != 0 || invalid != 0 {
		t.Errorf("empty ledger should build nothing: rows=%d invalid=%d", len(rows), invalid)
	}
}

func TestBuildSessionsCommittedWithoutCost(t *testing.T) {
	// A commit row with no costUsd must still mark the session committed;
	// pricing then falls back to the SessionEnd total.
	ledger := []LedgerRow{{SessionID: "s9", CostUSD: f64(1.1)}}
	att := RootCostMap([]LedgerRow{{SessionID: "s9"}})
	rows, _ := BuildSessions(ledger, att, "m")
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if !rows[0].Committed {
		t.Errorf("costless commit row must still mark committed")
	}
	if rows[0].CostSource != CostFromSessions || *rows[0].CostUSD != 1.1 {
		t.Errorf("pricing should fall back to sessions.jsonl: %+v", rows[0])
	}
}
