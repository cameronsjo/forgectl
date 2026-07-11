// Package sessions is the ops layer for `forgectl sessions`: the idempotent
// ETL that drains the local JSONL write-ahead log (~/.claude/metrics/) and
// the runbook markdown corpus (~/.claude/cadence/runbooks/) into the
// cross-machine operational mart — an always-on Postgres session index.
//
// Contract (docs/plans/2026-07-10-cadence-persistence-observability.md in
// cameronsjo/claude-configurations):
//
//   - JSONL is the write-ahead log; Postgres is the index. This package only
//     READS the local ledgers — hooks keep appending offline-safe, and no
//     hook ever blocks on DB reachability.
//   - Upsert key = session_id alone (a globally-unique UUID). `machine` is a
//     provenance column, never part of the key.
//   - lastMessageId is the incremental-sync cursor/watermark — sessions whose
//     watermark already matches the mart are skipped, never re-keyed.
//   - Cost attribution follows ADR-0017: per-session cost from commits.jsonl
//     grouped by parentSessionId//sessionId, never recomputed when a commit
//     exists; sessions.jsonl's SessionEnd total is the fallback.
//
// House pattern: the decision logic (ledger merge, cost attribution, runbook
// parsing) is pure and lives in build.go/runbooks.go; the effects (Postgres
// I/O) are isolated in mart.go; sync.go orchestrates. Cobra knows none of it
// (see internal/cli/sessions.go).
package sessions

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// LedgerRow is one line of sessions.jsonl or commits.jsonl — the superset of
// the fields the operational index consumes. Unknown fields are ignored.
type LedgerRow struct {
	SessionID       string     `json:"sessionId"`
	ParentSessionID string     `json:"parentSessionId"`
	Repo            string     `json:"repo"`
	Branch          string     `json:"branch"`
	Model           string     `json:"model"`
	CostUSD         *float64   `json:"costUsd"`
	LastMessageID   string     `json:"lastMessageId"`
	StartTs         *time.Time `json:"startTs"`
	EndTs           *time.Time `json:"endTs"`
	Ts              *time.Time `json:"ts"`
	Tokens          Tokens     `json:"tokens"`
}

// Tokens is the per-row token quartet from the ledgers.
type Tokens struct {
	Input       int64 `json:"input"`
	CacheCreate int64 `json:"cacheCreate"`
	CacheRead   int64 `json:"cacheRead"`
	Output      int64 `json:"output"`
}

// ReadLedger reads a JSONL ledger, tolerating a missing file (the WAL may not
// exist yet on a fresh machine) and skipping unparseable lines — a torn tail
// write from a live hook must not fail the whole sync. It reports how many
// lines were skipped so corruption is visible, never swallowed.
func ReadLedger(path string) (rows []LedgerRow, skipped int, err error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		slog.Debug("Ledger file absent, treating as empty.", "path", path)
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open ledger %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Ledger lines carry full byModel breakdowns; give the scanner headroom.
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var row LedgerRow
		if jsonErr := json.Unmarshal(line, &row); jsonErr != nil {
			skipped++
			continue
		}
		rows = append(rows, row)
	}
	if scanErr := sc.Err(); scanErr != nil {
		return nil, skipped, fmt.Errorf("scan ledger %s: %w", path, scanErr)
	}
	if skipped > 0 {
		slog.Warn("Ledger contained unparseable lines, skipped them.", "path", path, "skipped", skipped, "parsed", len(rows))
	}
	return rows, skipped, nil
}
