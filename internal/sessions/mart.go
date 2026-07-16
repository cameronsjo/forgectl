package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// connectTimeout bounds how long a sync waits for an unreachable mart — an
// offline laptop must fail fast and leave the JSONL WAL to drain later, never
// hang a hook or a cron flush.
const connectTimeout = 5 * time.Second

// Mart is the thin Postgres seam. It owns no decision logic — build.go and
// runbooks.go decide what rows exist; Mart moves them.
type Mart struct {
	conn *pgx.Conn
}

// ConnectMart opens a single connection to the operational mart. The DSN
// SHOULD omit the password: pgx resolves ~/.pgpass (libpq-compatible), so the
// secret stays outside repos and config files.
func ConnectMart(ctx context.Context, dsn string) (*Mart, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to mart (JSONL WAL is untouched; re-run when reachable): %w", err)
	}
	slog.Debug("Successfully connected to the operational mart.")
	return &Mart{conn: conn}, nil
}

// Close releases the connection.
func (m *Mart) Close(ctx context.Context) error { return m.conn.Close(ctx) }

// schemaHint decorates undefined-table errors (SQLSTATE 42P01) so a fresh
// mart points the operator at the canonical DDL instead of a bare SQL error.
func schemaHint(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return fmt.Errorf("%w — mart schema missing; apply scripts/sessions-mart/schema.sql (cameronsjo/claude-configurations)", err)
	}
	return err
}

// Watermarks returns session_id -> last_message_id for the given ids — the
// incremental-sync cursor. Sessions whose local watermark matches are
// already-synced tails the ETL skips.
func (m *Mart) Watermarks(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	rows, err := m.conn.Query(ctx,
		`SELECT session_id, coalesce(last_message_id, '') FROM session WHERE session_id = ANY($1)`, ids)
	if err != nil {
		return nil, schemaHint(fmt.Errorf("query watermarks: %w", err))
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		var id, wm string
		if err := rows.Scan(&id, &wm); err != nil {
			return nil, fmt.Errorf("scan watermark row: %w", err)
		}
		out[id] = wm
	}
	return out, rows.Err()
}

// UpsertSessions writes the operational index rows, keyed on session_id alone
// (machine is provenance, never part of the key). Batched in one implicit
// transaction: a killed connection mid-flush rolls back cleanly and the next
// run drains the same WAL.
func (m *Mart) UpsertSessions(ctx context.Context, rows []SessionRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`
			INSERT INTO session (
				session_id, machine, project, git_branch, model,
				first_ts, last_ts,
				tokens_input, tokens_cache_create, tokens_cache_read, tokens_output,
				cost_usd, cost_source, committed, last_message_id, synced_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15, now())
			ON CONFLICT (session_id) DO UPDATE SET
				machine = EXCLUDED.machine,
				project = EXCLUDED.project,
				git_branch = EXCLUDED.git_branch,
				model = EXCLUDED.model,
				first_ts = EXCLUDED.first_ts,
				last_ts = EXCLUDED.last_ts,
				tokens_input = EXCLUDED.tokens_input,
				tokens_cache_create = EXCLUDED.tokens_cache_create,
				tokens_cache_read = EXCLUDED.tokens_cache_read,
				tokens_output = EXCLUDED.tokens_output,
				cost_usd = EXCLUDED.cost_usd,
				cost_source = EXCLUDED.cost_source,
				committed = EXCLUDED.committed,
				last_message_id = EXCLUDED.last_message_id,
				synced_at = now()`,
			r.SessionID, r.Machine, nullable(r.Project), nullable(r.GitBranch), nullable(r.Model),
			r.FirstTs, r.LastTs,
			r.Tokens.Input, r.Tokens.CacheCreate, r.Tokens.CacheRead, r.Tokens.Output,
			r.CostUSD, nullable(r.CostSource), r.Committed, nullable(r.LastMessageID),
		)
	}
	if err := m.conn.SendBatch(ctx, batch).Close(); err != nil {
		return schemaHint(fmt.Errorf("upsert %d session rows: %w", len(rows), err))
	}
	return nil
}

// PresentIDs reports which of the given session ids exist in the mart — the
// post-flush reconcile that turns a silently-skipped session into a loud
// MISSING line on the receipt.
func (m *Mart) PresentIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	rows, err := m.conn.Query(ctx,
		`SELECT session_id FROM session WHERE session_id = ANY($1)`, ids)
	if err != nil {
		return nil, schemaHint(fmt.Errorf("reconcile present ids: %w", err))
	}
	defer rows.Close()
	out := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan present id: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// UpsertRunbooks rebuilds the derived full-text index from the scanned corpus
// (plan D4): upsert every scanned file keyed on path, then prune rows whose
// file no longer exists in the corpus. The prune runs ONLY when the scan
// found at least one file — an absent or empty corpus on this machine must
// not wipe an index another machine populated.
func (m *Mart) UpsertRunbooks(ctx context.Context, rows []RunbookRow) (deleted int64, err error) {
	if len(rows) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	paths := make([]string, 0, len(rows))
	for _, r := range rows {
		paths = append(paths, r.Path)
		batch.Queue(`
			INSERT INTO runbooks (session_id, project, slug, title, type, path, full_text, machine, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())
			ON CONFLICT (path) DO UPDATE SET
				session_id = EXCLUDED.session_id,
				project = EXCLUDED.project,
				slug = EXCLUDED.slug,
				title = EXCLUDED.title,
				type = EXCLUDED.type,
				full_text = EXCLUDED.full_text,
				machine = EXCLUDED.machine,
				updated_at = now()`,
			nullable(r.SessionID), nullable(r.Project), nullable(r.Slug), nullable(r.Title),
			nullable(r.Type), r.Path, r.FullText, r.Machine,
		)
	}
	if err := m.conn.SendBatch(ctx, batch).Close(); err != nil {
		return 0, schemaHint(fmt.Errorf("upsert %d runbook rows: %w", len(rows), err))
	}
	tag, err := m.conn.Exec(ctx, `DELETE FROM runbooks WHERE path <> ALL($1)`, paths)
	if err != nil {
		return 0, schemaHint(fmt.Errorf("prune vanished runbooks: %w", err))
	}
	return tag.RowsAffected(), nil
}

// SearchHit is one full-text match from the mart's runbook index.
type SearchHit struct {
	Path    string
	Title   string
	Project string
	Type    string
	Machine string
	Rank    float32
	Snippet string
}

// SearchRunbooks runs a websearch-syntax full-text query over the index,
// falling back to a trigram ILIKE scan when the tsquery matches nothing.
// The fallback treats the whole query as ONE literal substring — it rescues
// a partial or typo'd single token (the pg_trgm GIN index carries it), not a
// multi-word phrase that happens to be split across the document.
func (m *Mart) SearchRunbooks(ctx context.Context, query, project string, limit int) ([]SearchHit, error) {
	hits, err := m.scanHits(ctx, `
		SELECT path, coalesce(title,''), coalesce(project,''), coalesce(type,''), machine,
		       ts_rank(search, q) AS rank,
		       ts_headline('english', full_text, q,
		                   'MaxWords=20, MinWords=8, StartSel=<<, StopSel=>>') AS snippet
		FROM runbooks, websearch_to_tsquery('english', $1) AS q
		WHERE search @@ q AND ($2 = '' OR project = $2)
		ORDER BY rank DESC
		LIMIT $3`, query, project, limit)
	if err != nil || len(hits) > 0 {
		return hits, err
	}
	slog.Debug("Full-text query matched nothing, falling back to trigram scan.", "query", query)
	return m.scanHits(ctx, `
		SELECT path, coalesce(title,''), coalesce(project,''), coalesce(type,''), machine,
		       0::float4 AS rank,
		       left(full_text, 160) AS snippet
		FROM runbooks
		WHERE full_text ILIKE '%' || $1 || '%' AND ($2 = '' OR project = $2)
		ORDER BY updated_at DESC
		LIMIT $3`, query, project, limit)
}

func (m *Mart) scanHits(ctx context.Context, sql string, args ...any) ([]SearchHit, error) {
	rows, err := m.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, schemaHint(fmt.Errorf("search runbooks: %w", err))
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Path, &h.Title, &h.Project, &h.Type, &h.Machine, &h.Rank, &h.Snippet); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// WhyHit is one predecessor session attributed to a topic through a runbook it
// authored — the "why did you do X" answer for one session. Because the mart
// ingests no per-file edit history, the attribution is narrative: a runbook
// (field report, handoff, plan) whose text matches the query AND whose
// frontmatter session_id names this session. Title stands in for intent, the
// snippet for key decisions, and Path is the local corpus link — the mart
// carries no dedicated intent, decisions, or URL fields.
type WhyHit struct {
	SessionID string
	Project   string
	Model     string
	LastTs    *time.Time
	Committed bool
	// The highest-ranked matching runbook this session authored.
	Title   string
	Type    string
	Path    string
	Snippet string
}

// WhySessions returns the most recent sessions whose authored runbooks match
// the query, newest first (by last_ts). A runbook links to a session through
// its session_id frontmatter (an INNER join): a runbook with no session_id
// can't name a session to ask "why", so it never appears here — it stays
// findable via SearchRunbooks. One row per session; the highest-ranked
// matching runbook represents it. The tsquery path handles topics; a trigram
// ILIKE fallback rescues a literal path or partial token the english parser
// mangles (dots, slashes), which is what lets a `<path>` argument match at all.
func (m *Mart) WhySessions(ctx context.Context, query, project string, limit int) ([]WhyHit, error) {
	slog.Debug("Preparing to query sessions for narrative match", "query", query, "project", project, "limit", limit)
	// The snippet parameters (ts_headline MaxWords/MinWords/selectors, the
	// 160-char trigram-fallback preview) mirror SearchRunbooks so `why` and
	// `search` render a match identically.
	hits, err := m.scanWhyHits(ctx, `
		SELECT session_id, project, model, last_ts, committed, title, type, path, snippet FROM (
			SELECT DISTINCT ON (r.session_id)
				s.session_id, coalesce(s.project,'') AS project, coalesce(s.model,'') AS model,
				s.last_ts, coalesce(s.committed,false) AS committed,
				coalesce(r.title,'') AS title, coalesce(r.type,'') AS type, r.path,
				ts_rank(r.search, q) AS rank,
				ts_headline('english', r.full_text, q,
					'MaxWords=20, MinWords=8, StartSel=<<, StopSel=>>') AS snippet
			FROM runbooks r
			JOIN session s ON s.session_id = r.session_id,
			     websearch_to_tsquery('english', $1) AS q
			WHERE r.search @@ q AND ($2 = '' OR s.project = $2)
			ORDER BY r.session_id, rank DESC
		) ranked
		ORDER BY last_ts DESC NULLS LAST
		LIMIT $3`, query, project, limit)
	if err != nil || len(hits) > 0 {
		return hits, err
	}
	slog.Debug("Why full-text query matched nothing, falling back to trigram scan.", "query", query)
	return m.scanWhyHits(ctx, `
		SELECT session_id, project, model, last_ts, committed, title, type, path, snippet FROM (
			SELECT DISTINCT ON (r.session_id)
				s.session_id, coalesce(s.project,'') AS project, coalesce(s.model,'') AS model,
				s.last_ts, coalesce(s.committed,false) AS committed,
				coalesce(r.title,'') AS title, coalesce(r.type,'') AS type, r.path,
				left(r.full_text, 160) AS snippet
			FROM runbooks r
			JOIN session s ON s.session_id = r.session_id
			WHERE r.full_text ILIKE '%' || $1 || '%' AND ($2 = '' OR s.project = $2)
			ORDER BY r.session_id, r.updated_at DESC
		) ranked
		ORDER BY last_ts DESC NULLS LAST
		LIMIT $3`, query, project, limit)
}

func (m *Mart) scanWhyHits(ctx context.Context, sql string, args ...any) ([]WhyHit, error) {
	rows, err := m.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, schemaHint(fmt.Errorf("query why sessions: %w", err))
	}
	defer rows.Close()
	var hits []WhyHit
	for rows.Next() {
		var h WhyHit
		if err := rows.Scan(&h.SessionID, &h.Project, &h.Model, &h.LastTs, &h.Committed,
			&h.Title, &h.Type, &h.Path, &h.Snippet); err != nil {
			return nil, fmt.Errorf("scan why hit: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// Artifact is one runbook a session authored — a narrative it left behind.
type Artifact struct {
	Type  string
	Title string
	Path  string
}

// SessionSummary is a session-table row plus the runbook artifacts it authored
// — the answer to "the most recent session in this repo and what it left
// behind". There is no explicit outro/lifecycle flag in the mart: Committed
// reports whether the session produced commits, and Artifacts is the set of
// runbooks (by type) it authored — a handoff or field-report among them is the
// closest signal of a clean sign-off.
type SessionSummary struct {
	SessionID string
	Project   string
	GitBranch string
	Model     string
	Machine   string
	FirstTs   *time.Time
	LastTs    *time.Time
	Committed bool
	Artifacts []Artifact
}

// LastSession returns the most recent session in a repo (by last_ts) and the
// runbook artifacts it authored. The repo match is exact against `project`
// (mirroring search's project filter). Returns (nil, nil) when the repo has no
// sessions in the mart — a clean miss the caller reports without erroring.
func (m *Mart) LastSession(ctx context.Context, repo string) (*SessionSummary, error) {
	slog.Debug("Preparing to query last session", "repo", repo)
	var s SessionSummary
	err := m.conn.QueryRow(ctx, `
		SELECT session_id, coalesce(project,''), coalesce(git_branch,''),
		       coalesce(model,''), machine, first_ts, last_ts, coalesce(committed,false)
		FROM session
		WHERE project = $1
		ORDER BY last_ts DESC NULLS LAST
		LIMIT 1`, repo).
		Scan(&s.SessionID, &s.Project, &s.GitBranch, &s.Model, &s.Machine,
			&s.FirstTs, &s.LastTs, &s.Committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, schemaHint(fmt.Errorf("query last session for %q: %w", repo, err))
	}
	arts, err := m.sessionArtifacts(ctx, s.SessionID)
	if err != nil {
		return nil, err
	}
	s.Artifacts = arts
	return &s, nil
}

// sessionArtifacts lists the runbooks a session authored (linked by
// session_id), ordered for a stable receipt.
func (m *Mart) sessionArtifacts(ctx context.Context, sessionID string) ([]Artifact, error) {
	rows, err := m.conn.Query(ctx, `
		SELECT coalesce(type,''), coalesce(title,''), path
		FROM runbooks WHERE session_id = $1
		ORDER BY type, path`, sessionID)
	if err != nil {
		return nil, schemaHint(fmt.Errorf("query session artifacts: %w", err))
	}
	defer rows.Close()
	var arts []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Type, &a.Title, &a.Path); err != nil {
			return nil, fmt.Errorf("scan artifact row: %w", err)
		}
		arts = append(arts, a)
	}
	return arts, rows.Err()
}

// nullable maps "" to SQL NULL so empty optional strings don't masquerade as
// real values in the index.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
