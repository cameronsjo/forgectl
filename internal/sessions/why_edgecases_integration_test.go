package sessions

// Edge-case coverage for WhySessions/LastSession NOT exercised by
// TestIntegrationWhyAndLast in integration_test.go (which covers the happy
// path, project-filter exactness, the session_id-less degradation, and
// newest-first ordering across two sessions). This file adds: an empty query
// string, a whitespace-only query string, absurd --limit values (0 and
// negative), and a non-ASCII (CJK + emoji) query through the tsquery path.
// Same FORGECTL_TEST_MART_DSN gate and testdata fixtures as the sibling file.

import (
	"context"
	"testing"
	"time"
)

// seedWhyFixture inserts one session with two authored runbooks — a plain
// ASCII one and a CJK+emoji one — so the edge-case tests below have known
// rows to query against without depending on the shared testdata corpus.
func seedWhyFixture(t *testing.T, ctx context.Context, mart *Mart) {
	t.Helper()
	if _, err := mart.conn.Exec(ctx, `
		INSERT INTO session (session_id, machine, project, git_branch, last_ts, synced_at)
		VALUES ('edge-1', 'it-machine', 'edgeproj', 'main', '2026-07-11T09:00:00Z', now())`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := mart.conn.Exec(ctx, `
		INSERT INTO runbooks (session_id, project, title, type, path, full_text, machine) VALUES
			('edge-1', 'edgeproj', 'ASCII plan', 'plan', 'edgeproj/ascii.md', 'a plain ascii runbook about widgets', 'it-machine'),
			('edge-1', 'edgeproj', 'CJK plan', 'plan', 'edgeproj/cjk.md', '咖啡 workflow discussion with an emoji 🔥 test', 'it-machine')`); err != nil {
		t.Fatalf("seed runbooks: %v", err)
	}
}

// An empty topic is rejected at the mart boundary. Without the guard it would
// collapse to an empty tsquery, fall through to the trigram fallback, and
// ILIKE '%' || ” || '%' would dump the entire session_id-linked corpus up to
// --limit — an unset shell variable (`forgectl sessions why ""`) becoming a
// corpus dump. WhySessions now refuses a blank query.
func TestIntegrationWhyEmptyTopicRejected(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)
	seedWhyFixture(t, ctx, mart)

	if _, err := mart.WhySessions(ctx, "", "", 10); err == nil {
		t.Fatal("empty topic must be rejected, got nil error (a blank query must not dump the corpus)")
	}
}

// A whitespace-only topic trims to empty, so the blank-query guard rejects it
// too — the guard is on the trimmed query, not just the literal empty string.
// (Before the guard this took the trigram fallback and matched nothing, since
// no fixture contains the literal whitespace run.)
func TestIntegrationWhyWhitespaceTopicRejected(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)
	seedWhyFixture(t, ctx, mart)

	if _, err := mart.WhySessions(ctx, "   ", "", 10); err == nil {
		t.Fatal("whitespace-only topic must be rejected, got nil error (it trims to blank)")
	}
}

// A negative --limit is not sanitized before reaching Postgres — LIMIT $N
// with N<0 is a Postgres-level error ("LIMIT must not be negative"), not a
// panic or a silently-empty result. Pin that WhySessions surfaces it as a Go
// error the CLI layer can report, rather than crashing or hanging.
func TestIntegrationWhyNegativeLimitErrors(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)
	seedWhyFixture(t, ctx, mart)

	if _, err := mart.WhySessions(ctx, "widgets", "", -1); err == nil {
		t.Error("negative limit should surface a Postgres error, got nil")
	}
}

// A zero --limit is valid SQL (LIMIT 0) and must degrade to a clean empty
// result on BOTH the full-text and trigram-fallback paths — not an error,
// not a panic, even though a match exists.
func TestIntegrationWhyZeroLimitReturnsEmpty(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)
	seedWhyFixture(t, ctx, mart)

	hits, err := mart.WhySessions(ctx, "widgets", "", 0)
	if err != nil {
		t.Fatalf("zero limit should not error: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("zero limit should return no hits even with a match available, got %+v", hits)
	}
}

// Non-ASCII topics (CJK ideographs, an emoji) must survive websearch_to_tsquery
// and match through the ranked full-text path, not silently fall through to
// the trigram fallback (or worse, error). The 'english' config still forms
// lexemes for non-English scripts via the default parser, so this pins that
// unicode.IsLetter-shaped input is a first-class query, not just something
// sanitizeTerm has to render safely on the way back out.
func TestIntegrationWhyUnicodeTopic(t *testing.T) {
	dsn := martDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mart := prepMart(t, ctx, dsn)
	seedWhyFixture(t, ctx, mart)

	hits, err := mart.WhySessions(ctx, "咖啡", "", 10)
	if err != nil {
		t.Fatalf("CJK topic: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "edgeproj/cjk.md" {
		t.Fatalf("CJK topic should rank-match the CJK runbook, got %+v", hits)
	}

	hits, err = mart.WhySessions(ctx, "🔥", "", 10)
	if err != nil {
		t.Fatalf("emoji topic: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "edgeproj/cjk.md" {
		t.Fatalf("emoji topic should rank-match the CJK+emoji runbook, got %+v", hits)
	}
}
