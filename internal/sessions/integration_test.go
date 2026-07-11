package sessions

// Integration suite for the mart ETL — the executable form of the plan's
// Phase 3 acceptance gates (docs/plans/2026-07-10-cadence-persistence-
// observability.md in cameronsjo/claude-configurations):
//
//   - a live run UPSERTs; a second run reports zero net-new (idempotency,
//     watermark path)
//   - the completeness receipt reconciles (no MISSING)
//   - rows carry the correct machine
//   - the runbook index populates from markdown and answers full-text queries
//
// Gated on FORGECTL_TEST_MART_DSN — point it at a THROWAWAY postgres with the
// mart schema applied (testdata/schema.sql mirrors the canonical DDL). Tables
// are truncated at test start.
//
// Run (example):
//   docker run -d --name mart-it -p 15544:5432 -e POSTGRES_PASSWORD=it \
//     -e POSTGRES_DB=sessions_mart postgres:17-alpine
//   FORGECTL_TEST_MART_DSN='postgres://postgres:it@127.0.0.1:15544/sessions_mart' \
//     go test ./internal/sessions/ -run Integration -v

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func martDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("FORGECTL_TEST_MART_DSN")
	if dsn == "" {
		t.Skip("FORGECTL_TEST_MART_DSN unset; skipping mart integration test")
	}
	return dsn
}

func prepMart(t *testing.T, ctx context.Context, dsn string) *Mart {
	t.Helper()
	mart, err := ConnectMart(ctx, dsn)
	if err != nil {
		t.Fatalf("connect throwaway mart: %v", err)
	}
	t.Cleanup(func() { _ = mart.Close(context.Background()) })

	schema, err := os.ReadFile(filepath.Join("testdata", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema fixture: %v", err)
	}
	if _, err := mart.conn.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply schema fixture: %v", err)
	}
	if _, err := mart.conn.Exec(ctx, `TRUNCATE session, runbooks RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return mart
}

func TestIntegrationSyncIdempotencyAndSearch(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)

	opts := SyncOptions{
		DSN:         dsn,
		Machine:     "it-machine",
		MetricsDir:  filepath.Join("testdata", "metrics"),
		RunbooksDir: filepath.Join("testdata", "runbooks"),
		// Hermetic: never read the live machine's Syncthing config in tests.
		SyncthingConfig: filepath.Join("testdata", "syncthing-clean.xml"),
	}

	// Dry-run first: counts, no writes.
	dry, err := Sync(ctx, SyncOptions{Machine: "it-machine",
		MetricsDir: opts.MetricsDir, RunbooksDir: opts.RunbooksDir,
		SyncthingConfig: opts.SyncthingConfig, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if dry.SessionsFound != 3 || dry.RunbooksFound != 2 || dry.SessionsUpserted != 0 {
		t.Fatalf("dry-run receipt off: %+v", dry)
	}
	var n int
	if err := mart.conn.QueryRow(ctx, `SELECT count(*) FROM session`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("dry-run must not write (count=%d, err=%v)", n, err)
	}

	// First live run: everything upserts and reconciles.
	first, err := Sync(ctx, opts)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if first.SessionsFound != 3 || first.SessionsUpserted != 3 || first.SessionsUnchanged != 0 {
		t.Fatalf("first sync receipt off: %+v", first)
	}
	if !first.Complete() {
		t.Fatalf("first sync did not reconcile: MISSING %v", first.Missing)
	}
	if first.RunbooksUpserted != 2 {
		t.Fatalf("runbooks not indexed: %+v", first)
	}

	// Second run: watermarked sessions skip — zero net-new rows.
	second, err := Sync(ctx, opts)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if second.SessionsUpserted != 0 || second.SessionsUnchanged != 3 {
		t.Fatalf("second sync must skip all watermarked sessions: %+v", second)
	}
	if err := mart.conn.QueryRow(ctx, `SELECT count(*) FROM session`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("row count drifted after re-run (count=%d, err=%v)", n, err)
	}

	// --full bypasses the watermark but stays idempotent on session_id.
	full, err := Sync(ctx, SyncOptions{DSN: dsn, Machine: "it-machine",
		MetricsDir: opts.MetricsDir, RunbooksDir: opts.RunbooksDir,
		SyncthingConfig: opts.SyncthingConfig, Full: true})
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if full.SessionsUpserted != 3 {
		t.Fatalf("--full should re-upsert everything: %+v", full)
	}
	if err := mart.conn.QueryRow(ctx, `SELECT count(*) FROM session`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("--full multiplied rows (count=%d, err=%v)", n, err)
	}

	// Provenance and ADR-0017 cost attribution landed in the mart.
	var machine, costSource string
	var committed bool
	err = mart.conn.QueryRow(ctx,
		`SELECT machine, cost_source, committed FROM session
		 WHERE session_id = '11111111-1111-1111-1111-111111111111'`).
		Scan(&machine, &costSource, &committed)
	if err != nil {
		t.Fatalf("read committed session row: %v", err)
	}
	if machine != "it-machine" || costSource != CostFromCommits || !committed {
		t.Errorf("committed row wrong: machine=%s cost_source=%s committed=%v",
			machine, costSource, committed)
	}

	// Full-text search finds a runbook by keyword (the Phase 4 query path).
	hits, err := mart.SearchRunbooks(ctx, "colima split brain", "", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Path != "hearth/colima-split-brain.md" {
		t.Fatalf("search missed the known runbook: %+v", hits)
	}
	// Project filter excludes it; trigram fallback tolerates a partial token.
	if hits, _ = mart.SearchRunbooks(ctx, "colima split brain", "otherproj", 5); len(hits) != 0 {
		t.Errorf("project filter leaked: %+v", hits)
	}
	if hits, err = mart.SearchRunbooks(ctx, "colim", "", 5); err != nil || len(hits) == 0 {
		t.Errorf("trigram fallback missed partial token (err=%v hits=%v)", err, hits)
	}
}

func TestIntegrationRunbookPruneRespectsEmptyCorpus(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)

	cleanST := filepath.Join("testdata", "syncthing-clean.xml")
	// Seed the index from the fixture corpus.
	if _, err := Sync(ctx, SyncOptions{DSN: dsn, Machine: "m1",
		MetricsDir:      filepath.Join("testdata", "metrics"),
		RunbooksDir:     filepath.Join("testdata", "runbooks"),
		SyncthingConfig: cleanST}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// A machine with NO corpus must leave the shared index untouched.
	if _, err := Sync(ctx, SyncOptions{DSN: dsn, Machine: "m2",
		MetricsDir:      filepath.Join("testdata", "metrics"),
		RunbooksDir:     filepath.Join("testdata", "does-not-exist"),
		SyncthingConfig: cleanST}); err != nil {
		t.Fatalf("corpus-less sync: %v", err)
	}
	var n int
	if err := mart.conn.QueryRow(ctx, `SELECT count(*) FROM runbooks`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("corpus-less machine wiped the shared index (count=%d, err=%v)", n, err)
	}

	// A corpus that lost a file prunes exactly that row.
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "hearth"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "runbooks", "hearth", "colima-split-brain.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "hearth", "colima-split-brain.md"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := Sync(ctx, SyncOptions{DSN: dsn, Machine: "m1",
		MetricsDir: filepath.Join("testdata", "metrics"), RunbooksDir: tmp,
		SyncthingConfig: cleanST})
	if err != nil {
		t.Fatalf("prune sync: %v", err)
	}
	if rec.RunbooksPruned != 1 {
		t.Errorf("expected exactly 1 pruned row, got %d", rec.RunbooksPruned)
	}
	if err := mart.conn.QueryRow(ctx, `SELECT count(*) FROM runbooks`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("prune left wrong count (count=%d, err=%v)", n, err)
	}
}
