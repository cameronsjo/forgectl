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
		name string
		rows []LedgerRow
		want map[string]float64
	}{
		{
			name: "groups by parentSessionId when present, else sessionId (ADR-0017)",
			rows: []LedgerRow{
				{SessionID: "child-1", ParentSessionID: "root-a", CostUSD: f64(1.5)},
				{SessionID: "child-2", ParentSessionID: "root-a", CostUSD: f64(0.5)},
				{SessionID: "root-b", CostUSD: f64(2.0)},
			},
			want: map[string]float64{"root-a": 2.0, "root-b": 2.0},
		},
		{
			name: "rows without cost or id contribute nothing",
			rows: []LedgerRow{
				{SessionID: "x"},
				{CostUSD: f64(9.9)},
			},
			want: map[string]float64{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RootCostMap(tt.rows)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d roots, want %d: %v", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("root %s: got %v, want %v", k, got[k], v)
				}
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
	rootCost := map[string]float64{"s2": 4.25}

	rows, invalid := BuildSessions(ledger, rootCost, "test-machine")

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
	rows, invalid := BuildSessions(nil, map[string]float64{}, "m")
	if len(rows) != 0 || invalid != 0 {
		t.Errorf("empty ledger should build nothing: rows=%d invalid=%d", len(rows), invalid)
	}
}
