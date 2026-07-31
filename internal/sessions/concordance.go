package sessions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// connectTimeout bounds how long a sync waits for an unreachable concordance — an
// offline laptop must fail fast and leave the JSONL WAL to drain later, never
// hang a hook or a cron flush.
const connectTimeout = 5 * time.Second

// Concordance is the thin Postgres seam. It owns no decision logic — build.go and
// runbooks.go decide what rows exist; Concordance moves them.
type Concordance struct {
	conn *pgx.Conn
}

// ConnectConcordance opens a single connection to the operational concordance. The DSN
// SHOULD omit the password: pgx resolves ~/.pgpass (libpq-compatible), so the
// secret stays outside repos and config files.
func ConnectConcordance(ctx context.Context, dsn string) (*Concordance, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to concordance (JSONL WAL is untouched; re-run when reachable): %w", err)
	}
	slog.Debug("Successfully connected to the operational concordance.")
	return &Concordance{conn: conn}, nil
}

// Close releases the connection.
func (c *Concordance) Close(ctx context.Context) error { return c.conn.Close(ctx) }

// schemaHint decorates the two schema-drift errors an operator can actually
// hit, so both point at the canonical DDL instead of surfacing as a bare pgx
// error with no guidance.
//
//	42P01 undefined_table  — a fresh concordance that has never had the DDL applied
//	42703 undefined_column — an EXISTING concordance running an older DDL
//
// 42703 is the more likely of the two in practice: UpsertSessions references
// every column unconditionally, so the moment the schema grows a column, every
// machine pointed at a not-yet-migrated concordance fails here.
func schemaHint(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	const applyDDL = "apply scripts/concordance/schema.sql (cameronsjo/claude-configurations)"
	switch pgErr.Code {
	case "42P01":
		return fmt.Errorf("%w — concordance schema missing; %s", err, applyDDL)
	case "42703":
		return fmt.Errorf("%w — concordance schema is out of date%s; %s", err, missingColumn(pgErr), applyDDL)
	}
	return err
}

// missingColumn returns a parenthetical naming the undefined column, or "" when
// there is nothing to add.
//
// Postgres reports 42703 with ColumnName EMPTY — verified against postgres 18.4:
// the column is named in Message ("column \"schema_version\" of relation
// \"session\" does not exist") and nowhere else. Since schemaHint wraps the
// original error, that Message is already in the final string, so echoing it
// here produced "…out of date (missing column "x" … does not exist)". Add the
// parenthetical only when the structured field actually carries something the
// wrapped message does not.
func missingColumn(pgErr *pgconn.PgError) string {
	if pgErr.ColumnName == "" {
		return ""
	}
	return " (missing column " + pgErr.ColumnName + ")"
}

// Watermarks returns session_id -> last_message_id for the given ids — the
// incremental-sync cursor. Sessions whose local watermark matches are
// already-synced tails the ETL skips.
func (c *Concordance) Watermarks(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	rows, err := c.conn.Query(ctx,
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
func (c *Concordance) UpsertSessions(ctx context.Context, rows []SessionRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`
			INSERT INTO session (
				session_id, schema_version, harness, source_format,
				machine, project, git_branch, model,
				first_ts, last_ts,
				tokens_input, tokens_cache_create, tokens_cache_read, tokens_output,
				tokens_reasoning_output, tokens_total,
				cost_usd, estimated_cost_usd, pricing_source, pricing_verified_at,
				cost_source, committed, last_message_id, synced_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23, now())
			ON CONFLICT (session_id) DO UPDATE SET
				schema_version = EXCLUDED.schema_version,
				harness = EXCLUDED.harness,
				source_format = EXCLUDED.source_format,
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
				tokens_reasoning_output = EXCLUDED.tokens_reasoning_output,
				tokens_total = EXCLUDED.tokens_total,
				cost_usd = EXCLUDED.cost_usd,
				estimated_cost_usd = EXCLUDED.estimated_cost_usd,
				pricing_source = EXCLUDED.pricing_source,
				pricing_verified_at = EXCLUDED.pricing_verified_at,
				cost_source = EXCLUDED.cost_source,
				committed = EXCLUDED.committed,
				last_message_id = EXCLUDED.last_message_id,
				synced_at = now()`,
			r.SessionID, r.SchemaVersion, r.Harness, nullable(r.SourceFormat),
			r.Machine, nullable(r.Project), nullable(r.GitBranch), nullable(r.Model),
			r.FirstTs, r.LastTs,
			r.Tokens.Input, r.Tokens.CacheCreate, r.Tokens.CacheRead, r.Tokens.Output,
			r.Tokens.ReasoningOutput, r.Tokens.Total,
			r.CostUSD, r.EstimatedCostUSD, nullable(r.PricingSource), r.PricingVerifiedAt,
			nullable(r.CostSource), r.Committed, nullable(r.LastMessageID),
		)
	}
	if err := c.conn.SendBatch(ctx, batch).Close(); err != nil {
		return schemaHint(fmt.Errorf("upsert %d session rows: %w", len(rows), err))
	}
	return nil
}

// PresentIDs reports which of the given session ids exist in the concordance — the
// post-flush reconcile that turns a silently-skipped session into a loud
// MISSING line on the receipt.
func (c *Concordance) PresentIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	rows, err := c.conn.Query(ctx,
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
func (c *Concordance) UpsertRunbooks(ctx context.Context, rows []RunbookRow) (deleted int64, err error) {
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
	if err := c.conn.SendBatch(ctx, batch).Close(); err != nil {
		return 0, schemaHint(fmt.Errorf("upsert %d runbook rows: %w", len(rows), err))
	}
	tag, err := c.conn.Exec(ctx, `DELETE FROM runbooks WHERE path <> ALL($1)`, paths)
	if err != nil {
		return 0, schemaHint(fmt.Errorf("prune vanished runbooks: %w", err))
	}
	return tag.RowsAffected(), nil
}

// SearchHit is one full-text match from the concordance's runbook index.
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
func (c *Concordance) SearchRunbooks(ctx context.Context, query, project string, limit int) ([]SearchHit, error) {
	hits, err := c.scanHits(ctx, `
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
	return c.scanHits(ctx, `
		SELECT path, coalesce(title,''), coalesce(project,''), coalesce(type,''), machine,
		       0::float4 AS rank,
		       left(full_text, 160) AS snippet
		FROM runbooks
		WHERE full_text ILIKE '%' || $1 || '%' AND ($2 = '' OR project = $2)
		ORDER BY updated_at DESC
		LIMIT $3`, query, project, limit)
}

func (c *Concordance) scanHits(ctx context.Context, sql string, args ...any) ([]SearchHit, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
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
// authored — the "why did you do X" answer for one session. Because the concordance
// ingests no per-file edit history, the attribution is narrative: a runbook
// (field report, handoff, plan) whose text matches the query AND whose
// frontmatter session_id names this session. Title stands in for intent, the
// snippet for key decisions, and Path is the local corpus link — the concordance
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
func (c *Concordance) WhySessions(ctx context.Context, query, project string, limit int) ([]WhyHit, error) {
	slog.Debug("Preparing to query sessions for narrative match", "query", query, "project", project, "limit", limit)
	// A blank query is meaningless for both paths: an empty tsquery matches
	// nothing, and the ILIKE fallback would collapse to '%%' and dump the entire
	// session-linked corpus (an unset shell variable becoming a corpus dump).
	// Refuse it at the boundary so no caller can trip that.
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("why query must not be blank")
	}
	// The snippet parameters (ts_headline MaxWords/MinWords/selectors, the
	// 160-char trigram-fallback preview) mirror SearchRunbooks so `why` and
	// `search` render a match identically.
	hits, err := c.scanWhyHits(ctx, `
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
			ORDER BY r.session_id, rank DESC, r.updated_at DESC NULLS LAST, r.path ASC
		) ranked
		ORDER BY last_ts DESC NULLS LAST, session_id ASC
		LIMIT $3`, query, project, limit)
	if err != nil || len(hits) > 0 {
		return hits, err
	}
	slog.Debug("Why full-text query matched nothing, falling back to trigram scan.", "query", query)
	return c.scanWhyHits(ctx, `
		SELECT session_id, project, model, last_ts, committed, title, type, path, snippet FROM (
			SELECT DISTINCT ON (r.session_id)
				s.session_id, coalesce(s.project,'') AS project, coalesce(s.model,'') AS model,
				s.last_ts, coalesce(s.committed,false) AS committed,
				coalesce(r.title,'') AS title, coalesce(r.type,'') AS type, r.path,
				left(r.full_text, 160) AS snippet
			FROM runbooks r
			JOIN session s ON s.session_id = r.session_id
			WHERE r.full_text ILIKE '%' || $1 || '%' ESCAPE '\' AND ($2 = '' OR s.project = $2)
			ORDER BY r.session_id, r.updated_at DESC NULLS LAST, r.path ASC
		) ranked
		ORDER BY last_ts DESC NULLS LAST, session_id ASC
		LIMIT $3`, likeEscape(query), project, limit)
}

// likeEscape backslash-escapes the LIKE/ILIKE metacharacters (%, _, and the
// escape char itself) so a caller-supplied string matches as a literal
// substring under an `ESCAPE '\'` clause — otherwise a bare `%` dumps the whole
// corpus and `foo_bar` matches `fooXbar`, contradicting the documented
// literal-substring behavior of the `why` fallback.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (c *Concordance) scanWhyHits(ctx context.Context, sql string, args ...any) ([]WhyHit, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
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
// behind". There is no explicit outro/lifecycle flag in the concordance: Committed
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
// sessions in the concordance — a clean miss the caller reports without erroring.
func (c *Concordance) LastSession(ctx context.Context, repo string) (*SessionSummary, error) {
	slog.Debug("Preparing to query last session", "repo", repo)
	var s SessionSummary
	err := c.conn.QueryRow(ctx, `
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
	arts, err := c.sessionArtifacts(ctx, s.SessionID)
	if err != nil {
		return nil, err
	}
	s.Artifacts = arts
	return &s, nil
}

// sessionArtifacts lists the runbooks a session authored (linked by
// session_id), ordered for a stable receipt.
func (c *Concordance) sessionArtifacts(ctx context.Context, sessionID string) ([]Artifact, error) {
	rows, err := c.conn.Query(ctx, `
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
