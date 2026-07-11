-- TEST FIXTURE — mirror of the canonical mart DDL.
-- Canonical: cameronsjo/claude-configurations scripts/sessions-mart/schema.sql
-- If integration tests fail on a schema mismatch, re-copy from canonical.
-- Cadence cross-machine operational mart — canonical DDL.
-- Plan: docs/plans/2026-07-10-cadence-persistence-observability.md (D3).
--
-- Two tables only: an operational `session` index and a derived `runbooks`
-- full-text index. Deliberately DISJOINT from the Session Observatory schema
-- (scripts/sessions/schema.sql): no analytical/judge tables here — those stay
-- loopback-local in hearth's sessions-db by charter. Observatory rows are
-- never migrated in.
--
-- Idempotent: safe to re-apply (CREATE ... IF NOT EXISTS throughout).
-- Apply: psql "$MART_DSN" -f schema.sql
--
-- pg_trgm ships in postgres:17-alpine. CREATE EXTENSION needs CREATE on the
-- DATABASE (superuser in practice) — the `mart` app role does NOT hold that.
-- The line below works for `mart` only because initdb/01-init.sh already
-- created the extension as superuser, so IF NOT EXISTS no-ops before any
-- permission check. On the reuse-an-existing-server path, a superuser must
-- run `CREATE EXTENSION IF NOT EXISTS pg_trgm;` in the mart database FIRST
-- (see README Phase 2 step 1) or this apply fails permission-denied.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Operational session index. Field shapes borrowed from the Observatory's
-- `session` table where they overlap; `machine` is provenance (a column,
-- NEVER part of the upsert key — session_id is a globally-unique UUID).
CREATE TABLE IF NOT EXISTS session (
    session_id          text PRIMARY KEY,
    machine             text NOT NULL,
    project             text,
    git_branch          text,
    model               text,
    first_ts            timestamptz,
    last_ts             timestamptz,
    tokens_input        bigint,
    tokens_cache_create bigint,
    tokens_cache_read   bigint,
    tokens_output       bigint,
    cost_usd            numeric,
    -- Which ledger priced the row: 'commits.jsonl' (ADR-0017 canonical
    -- root-session aggregation) or 'sessions.jsonl' (SessionEnd total).
    -- NULL = unpriced (a session with no cost row anywhere).
    cost_source         text
        CHECK (cost_source IN ('commits.jsonl', 'sessions.jsonl')),
    committed           boolean,
    -- Incremental-sync cursor/watermark: the last transcript message id the
    -- ETL has seen for this session. Lets a sync skip already-synced tails.
    -- A watermark only — never a dedup key.
    last_message_id     text,
    synced_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS session_machine_ts ON session (machine, first_ts);
CREATE INDEX IF NOT EXISTS session_project_ts ON session (project, first_ts);

-- Derived, rebuildable full-text index over the synced runbook markdown
-- corpus (~/.claude/cadence/runbooks/). Markdown is the source of truth (D4);
-- this table is droppable and regenerable, never a second source of truth.
-- `path` (relative to the corpus root) is the upsert key. session_id is a
-- soft reference — deliberately no FK, so a runbook indexes cleanly even when
-- its authoring session has not synced yet.
CREATE TABLE IF NOT EXISTS runbooks (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id  text,
    project     text,
    slug        text,
    title       text,
    type        text,
    path        text NOT NULL UNIQUE,
    full_text   text NOT NULL,
    machine     text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- The 512 KiB cap guards the tsvector 1 MiB hard limit for pathological
    -- documents. The sibling trigram index below spans the FULL text — the
    -- two search paths deliberately cover different spans past the cap
    -- (ranked search truncates; substring fallback doesn't). Runbooks are
    -- small markdown; the cap exists for safety, not as a working range.
    search      tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english', left(full_text, 524288)), 'B')
    ) STORED
);
CREATE INDEX IF NOT EXISTS runbooks_search_gin ON runbooks USING GIN (search);
CREATE INDEX IF NOT EXISTS runbooks_full_text_trgm ON runbooks USING GIN (full_text gin_trgm_ops);
CREATE INDEX IF NOT EXISTS runbooks_project ON runbooks (project);
