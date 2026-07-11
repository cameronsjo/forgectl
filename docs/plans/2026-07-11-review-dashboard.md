# Plan: cross-project review dashboard (`forgectl review`)

## Provenance

- issue: #76 (part of #1; refs #3, #13, #55; composes with draft PR #43 and cameronsjo/auto-claude#4)
- planned: 2026-07-11, panel-reviewed (2 seats: consistency/underspecification + owner lens; findings folded in)
- recommended_model: **sonnet** for Phase A (this doc is the complete spec; no architecture judgment left open). Phase B needs the UI-requirements doc written first (a prose task), then the server slice is sonnet-drivable too. Escalate to opus only if the shared-search extraction turns out to fight `internal/pr`'s test suite in ways this doc didn't anticipate.
- human gates: plan approval (this PR leaving draft is NOT the gate — the gate is the maintainer's go-ahead on the plan; merge is the second gate).

## Context

One place to review the open work inventory — issues *and* PRs — across every owned repo:
~200 open issues across 13 repos, ~85 open PRs across 38 repos, ~20 of them draft `plan: …`
PRs awaiting drivers. No such surface exists today; `pr dash`/`pr prs` cover only
`@me`-involvement PRs and silently truncate (see Bug fix below).

**Governing posture:** state is referenced, never copied (the work-tracking layer contract,
ADR-0035 in the private meta-repo). Render live from `gh`; no ETL, no sync, no second store.
The only persisted view-state is the reviewed-marks file — that narrower rule is this plan's
own posture, stated here, not the ADR's.

**Compose story:** PR #43 owns the PR review *workflow*; this owns the cross-repo *read
surface*. auto-claude#4 (observatory) is the executor-fleet window (zero gh calls); this is
the work-inventory window (zero tmux calls). `review serve` follows the observatory's
security posture and its UI pipeline (requirements doc → external design session → static
assets land later).

**Stage decision:** the PoC ships **stage-blind** with a raw LABELS column — the live
`auto:*` labels carry the pipeline signal with zero derivation. A STAGE column + `--stage`
filter is a tracked fast-follow gated on the pipeline-convention spec freezing; its rule
table is hand-transcribed from the frozen spec into one Go table (`stage.go`, ordered,
first-match-wins, honest `unknown`) citing the spec version. Built once — no parallel-guess
table.

## Command surface

```
forgectl review                    unified issues+PRs: KIND / REPO / # / TITLE / LABELS / STATE (reviewed rows dimmed)
forgectl review --json             machine output; [] never null
forgectl review --kind issue|pr --repo <slug>
forgectl review mark <ref> | unmark <ref> | sync    (ref normalized to the host-qualified key)
forgectl review serve [--addr 127.0.0.1:4711] [--token <t>] [--cache-ttl 60s]     (Phase B)
```

`review` is a new top-level command group registered in `internal/cli/root.go` — it is not a
`pr` subcommand because it spans issues and (later) hosts beyond github.com.

## Phase A — PoC: issues fan-out + unified aggregation + CLI

### New package `internal/review/` (flat, mirrors `internal/pr`)

**`review.go`** — the domain type and helpers:

```go
type Kind string // "issue" | "pr"

type Item struct {
    Kind      Kind
    Host      string   // "github.com" for Phase A; the Gitea seam carries its own
    Owner     string
    Repo      string
    Number    int
    Title     string
    Author    string
    State     string   // lowercased gh state
    IsDraft   bool     // PRs only
    Labels    []string // label names, order as returned
    UpdatedAt time.Time
    URL       string
}

func (i Item) Slug() string { return i.Owner + "/" + i.Repo }
// Key is the host-qualified identity: "github.com/owner/repo#N".
// Phase-C-ready: a Gitea item keys as "git.example.com/owner/repo#N".
// NOTE: issues and PRs share GitHub's number space per repo, so Key alone
// is unique without Kind.
func (i Item) Key() string
```

Deterministic sort: `(Host, Slug, Number)` — same tiebreak-for-stability discipline as
`sortPRs`.

**`source.go`** — the source seam (this is the Gitea seam) + aggregation:

```go
type Source interface {
    Name() string
    Items(ctx context.Context) ([]Item, []string, error) // items, notes, fatal err
}

// Aggregate fans sources in, dedupes by Key (last write wins is fine — sources
// don't overlap in Phase A), degrades a failed source to a note, sorts.
func Aggregate(ctx context.Context, sources ...Source) ([]Item, []string, error)
```

Concurrency model mirrors `pr.PRs`: buffered channel sized to the source count, fixed
receive loop, a failed source contributes a note never an error. `Aggregate` errors only
when EVERY source fails (all-degraded = no data to render; mirror the spirit of
`pr reviewed sync`'s refuse-on-partial).

**`github.go`** — the GitHub source over `exec.Runner`:

- Construction: `NewGitHub(run exec.Runner, owners []string)`.
- Queries per owner: `gh search issues --owner <o> --state open --json <fields> --limit 500`
  and `gh search prs --owner <o> --state open --json <fields> --limit 500`. Owner values come
  from config (validated against the same anchored owner charset as `internal/pr` — config is
  low-trust input to an argv).
- **Deliberately `--owner`-scoped, NOT `@me`:** this is the owner-wide inventory view;
  `pr dash`/`pr prs` keep their involvement scoping.
- Issue JSON fields: `number,title,url,author,updatedAt,state,labels,repository`. PR fields:
  the existing `prSearchFields` + `labels`.
- Truncation sentinel: when one query returns exactly its `--limit` rows, append a note
  (`"issues(<owner>): results may be truncated at 500"`) — never silently cap.
- Parsing: hostile-input discipline identical to `parseSearchPRs` — validate owner/repo
  charset per row, skip-and-log invalid rows, never fatal.

### Shared search extraction + bug fix (in `internal/pr`)

Pull the `gh search prs` invocation/parse into one helper both surfaces call:

```go
// search.go (new file in internal/pr)
// SearchOpts scopes one gh search query. Exactly one of WhoFlag or Owner is set.
type SearchOpts struct {
    WhoFlag string // "--author" | "--assignee" | "--review-requested" (value @me)
    Owner   string // --owner <owner> scoping (review surface)
    Limit   int    // 0 → DefaultSearchLimit
}
func SearchPRs(ctx context.Context, run exec.Runner, opts SearchOpts) ([]PR, error)
```

`Client.searchPRs` becomes a thin call through `SearchPRs` with `WhoFlag` set. **The bug
fix:** today's `searchPRs` passes no `--limit`, so gh's default (30) silently truncates
`pr prs`/`pr dash` whenever a query exceeds 30 rows. `DefaultSearchLimit = 200` for the
`@me` queries (involvement rarely exceeds this; the sentinel note catches it when it does):
rows == limit → the caller appends a truncation note. `internal/review/github.go` reuses
the same helper for its PR leg (mapping `PR` → `Item`; labels ride a widened field set —
adding `labels` to `prSearchFields` is additive and `pr prs`/`pr dash` simply ignore it).
The issues leg lives in `internal/review` (different JSON shape, no `isDraft`).

### Reviewed-marks reuse (in `internal/pr/reviewed.go`)

Add string-key methods alongside the Ref-key ones (additive, no behavior change):

```go
func (s *ReviewedStore) MarkKey(key string) error
func (s *ReviewedStore) UnmarkKey(key string) error
func (s *ReviewedStore) IsReviewedKey(key string, latestActivity time.Time) bool
func (s *ReviewedStore) SyncKeys(openKeys []string) error
```

The existing Ref methods delegate to these with `ref.String()`. The review surface stores
under the host-qualified `Item.Key()` in its own file — same timestamp≥UpdatedAt
auto-un-dim semantics, zero new logic.

### Config (in `internal/config/config.go`)

```toml
[review]              # forgectl review — cross-project work inventory
owners = ["cameronsjo"]   # gh search --owner scope; default ["cameronsjo"]
```

`ReviewConfig{Owners []string}` + `IsZero()`, wired into `Config`. Default applied in the
CLI layer when the section is absent. `ReviewReviewedPath()` beside `PrReviewedPath()` →
`review-reviewed.json` (separate file: different key shape, different lifecycle).

### CLI (in `internal/cli/`)

- `review.go` — `newReviewCmd(cfg config.Config)` parent: builds the GitHub source over
  `exec.OSRunner{}`, resolves owners (config else default), registers in `root.go`.
- `review_list.go` — the bare `forgectl review` RunE + `--json` + `--kind` + `--repo`
  filters. Table: `KIND / REPO / # / TITLE / LABELS / STATE`; labels cell is
  comma-joined and `sanitizeCell`-scrubbed like every other gh-supplied string; dimming
  applied per whole line AFTER tabwriter flush (post-flush dim discipline); count summary
  to stderr. `--json` emits `[]` never null, each row carrying `key`, `kind`, `repo`,
  `number`, `title`, `state`, `isDraft`, `labels`, `updatedAt`, `url`, `reviewed`.
- `review_mark.go` — `mark <ref>` / `unmark <ref>` / `sync`. `<ref>` accepts
  `owner/repo#N` (normalized to the github.com-qualified key via the exported validator)
  or a full `https://github.com/owner/repo/{issues,pull}/N` URL. `sync` prunes against the
  full aggregated open set, refusing on partial data and on an empty set (same guards as
  `pr reviewed sync`, same rationale comments).
- Ref parsing: export a small `ParseWorkRef` in `internal/review` reusing `internal/pr`'s
  anchored validation via an exported `pr.RefFromParts(owner, repo, num string) (Ref, error)`
  (rename of the private `refFrom`, kept as the one anchored validator — both paths stay
  on the same charset guards).
- Test seams: `newReviewCmdForClient(...)`-style constructors taking the source + store
  path explicitly, mirroring `newPrPrsCmdForClient`.

### Tests (house style: FakeRunner, temp-dir stores, table tests)

- `internal/review/github_test.go` — argv construction per owner (both legs), hostile-row
  skipping, truncation sentinel at rows==limit, label parsing.
- `internal/review/source_test.go` — Aggregate dedupe/degrade/sort; all-sources-failed error.
- `internal/pr/discover_test.go` — extend: `--limit` present on every `@me` query;
  extraction keeps existing argv otherwise identical.
- `internal/pr/reviewed_test.go` — string-key methods share semantics with Ref methods.
- `internal/cli/review_list_test.go` / `review_mark_test.go` — table rendering + dim
  discipline, `--json` shape ([] never null), mark/unmark/sync flows over a temp store.

## Phase B — web face (after Phase A merges)

- `internal/review/server.go` — stdlib `net/http` only: `GET /api/review`,
  `POST/DELETE /api/reviewed/{key}`, `GET /` static; in-memory TTL cache (default 60s,
  `?refresh=1` busts) so a browser refresh never hot-polls `gh search` (30 req/min budget).
- `internal/review/static/` via `go:embed` — minimal functional page first (fetch, filter,
  mark-toggle; `textContent`-only rendering — titles are hostile input). The designed
  Artificer-themed frontend lands later as a static-assets-only PR, produced from
  `docs/design/review-dashboard-ui-requirements.md` (to be written; observatory precedent).
- `internal/cli/review_serve.go` — **forgectl's first HTTP surface**, so first-surface
  scrutiny: loopback-only default (`127.0.0.1:4711`); refuse to start on a non-loopback
  `--addr` without `--token`; Host-header allowlist; token required on mutating endpoints
  regardless (the only mutable state is reviewed-marks; deny-by-default anyway).

## Phase C — Gitea source (seam already built)

`internal/review/gitea.go` implementing `Source` via the tea CLI (pattern:
`internal/projects/gitea.go`); `[review] gitea = true`-style config toggle. Session-lane
and fleet convergence ride the same seam.

## Risks

1. **gh search budget:** 30 req/min; a full load is 2 queries × owners (default 1 owner =
   2 calls) — trivially fine. Multi-owner configs stay ≤ 8 calls.
2. **Stage ambiguity:** resolved by decision — stage-blind PoC; STAGE builds once from the
   frozen convention spec (fast-follow issue carries the transcription + version cite).
3. **Linkage fidelity:** body-parsed Closes refs miss UI-created links; a GraphQL
   `closingIssuesReferences` batch upgrade is the fast-follow path, not built now.
4. **`gh search issues` also matches PRs on some gh versions** unless filtered; verify the
   installed gh's behavior and, if needed, add `--include-prs=false`-equivalent filtering
   or post-filter rows whose URL contains `/pull/` out of the issues leg (dedupe by Key
   already prevents double-render; the guard keeps Kind honest).

## Verification

- `forgectl review --json | jq length` roughly matches spot-checked
  `gh search issues --owner cameronsjo --state open` + `gh search prs` counts.
- A known `plan: …` draft PR shows its `auto:*` labels in the LABELS column.
- `review mark owner/repo#N` dims the row; new upstream activity un-dims it; CLI and
  (later) web marks land under the same host-qualified key.
- `pr prs` output grows past 30 rows where it was silently truncating.
- `go build ./... && go vet ./... && go test ./...` green.
