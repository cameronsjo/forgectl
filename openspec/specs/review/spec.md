# review Specification

## Purpose
`forgectl review` renders the cross-project work inventory (every open issue and PR across the configured owners, live from gh and optionally Gitea) with local reviewed marks, and `review releases` is a read-only release radar over the release-rhythm registry.

## Requirements

### Requirement: Live work inventory with reviewed dimming
`forgectl review` SHALL list every open issue and PR across `[review] owners` (or the authenticated login when unset or empty), live from gh, copying nothing. The only local state SHALL be the reviewed-marks file. Reviewed rows SHALL be dimmed, and an item with activity newer than its mark MUST read as unreviewed again.

#### Scenario: New activity un-dims an item
- **WHEN** an item marked reviewed receives an update after the mark
- **THEN** the next `forgectl review` shows it undimmed with `reviewed: false` under `--json`

### Requirement: Filter validation before any query
`review` SHALL accept `--kind issue|pr` and `--repo owner/name`. A `--kind` outside those values or a `--repo` not shaped `owner/name` MUST be refused with exit 2 (`usage_error` under `--json`) before any query runs.

#### Scenario: Malformed repo filter
- **WHEN** the operator runs `forgectl review --repo forgectl`
- **THEN** the command exits 2 and makes no gh call

### Requirement: JSON output shape
`review --json` SHALL emit a bare array of rows with keys `key`, `kind`, `repo`, `number`, `title`, `state`, `isDraft`, `labels`, `updatedAt`, `url`, `reviewed`; an empty result MUST be `[]` and `labels` MUST be `[]`, never `null`. Notes SHALL go to stderr.

#### Scenario: No open items
- **WHEN** the configured owners have no open issues or PRs
- **THEN** `review --json` writes `[]`

### Requirement: Bounded list output
`--limit N` SHALL keep the first N rows sorted by host, repo, number; `--limit 0` keeps every row. The human table MUST default to 100 rows with a `showing N of M` stderr hint when cut. With `--json`, any `--limit` SHALL emit `{truncated,total,shown,limit,hint,notes,items}` with `items` last and `notes` carrying the stderr notes; no `--limit` keeps the bare array. `--fields` SHALL project row keys; an unknown name MUST exit 1.

#### Scenario: Source-side truncation is visible on stdout
- **WHEN** a GitHub search is capped upstream and the operator runs `review --json --limit 0`
- **THEN** the object has `truncated: false` and its `notes` carries the "results may be truncated" note

### Requirement: Total failure is never an empty inventory
When every source fails, `review` MUST exit 1. Under `--json`, the per-source notes, each with its cause and fix, SHALL be folded into the one failure object's `error`; otherwise they print as lines above the error. One failing source out of several SHALL be a note with exit 0.

#### Scenario: gh not signed in
- **WHEN** the only source is GitHub and gh is not authenticated
- **THEN** `review --json` exits 1 and the failure object's `error` names `gh auth login` as the fix

### Requirement: Mark, unmark, and safe sync
`review mark <ref>` and `review unmark <ref>` SHALL set and clear a reviewed mark; `owner/repo#N` always means the configured GitHub host, and a host-qualified ref is accepted only for a host wired up as a source. `review sync` SHALL prune marks for items no longer open, but MUST refuse to prune when any query degraded or was truncated, MUST skip when the open set is empty, and MUST leave marks for hosts with no active source untouched.

#### Scenario: Partial open set
- **WHEN** one query degrades during `review sync`
- **THEN** sync prints that it refuses to prune and no mark is removed

#### Scenario: Empty open set
- **WHEN** the queries return no open items
- **THEN** sync skips pruning so the store is not wiped

### Requirement: GitHub host pin and token scrub
Every review gh call MUST be pinned to `[github] host` (default `github.com`), overriding an ambient `GH_HOST`. On a non-default host, `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN` and `GITHUB_ENTERPRISE_TOKEN` MUST be scrubbed so only gh's stored credential for that host is used.

#### Scenario: Enterprise host configured
- **WHEN** `[github] host = "github.example.com"` and the shell exports `GH_TOKEN`
- **THEN** review's gh calls target that host without `GH_TOKEN` in their environment

### Requirement: Invalid review config fails loudly
When the config file fails to decode, `[github] host` is invalid, `[review.gitea]` is enabled with an empty or invalid host, or the Gitea host equals the GitHub host, every `review` verb MUST fail immediately with `review: invalid config` before touching the reviewed store or running gh. Flags MUST NOT mask this error.

#### Scenario: Gitea enabled with no host
- **WHEN** `[review.gitea] enabled = true` has no `host`
- **THEN** `forgectl review --json` fails with the config error rather than an unknown-flag error

### Requirement: Read-only release radar
`review releases` SHALL read the registry (`--registry`, else `$FORGECTL_RELEASE_REGISTRY`, else `~/Projects/cadence-ecosystem/docs/release-rhythm.yaml`) and query GitHub read-only, pinned to `[github] host`, for each `release-pr`, `testflight`, and `manual-cut` repo; `continuous` and `dormant` repos SHALL be listed in a footer. A registry over 256 KiB, with a repeated or merge key, or a mapping over 64 keys MUST be refused.

#### Scenario: Oversized registry
- **WHEN** the registry file exceeds 256 KiB
- **THEN** the radar refuses to read it

### Requirement: Release row states and stall rules
Each tracked repo SHALL be `ok`, `paused` (toggle not `on`), `stalled`, or `unknown` (any failed read, named in the row). A repo SHALL be `stalled` when its toggle is on and the entrypoint has not run in 26h, its last 2 scheduled runs repeat a non-healthy reason, a run reports `half-shipped`, an endpoint trails a release older than 24h, its gate or lint copy hash is missing or differs, or the `no-release-pr` or `release-workflow-stuck` rule fires.

#### Scenario: Gate copy drift
- **WHEN** a repo's `.github/scripts/ship-gate.sh` hash differs from the canonical hash
- **THEN** that repo's row is `stalled`

#### Scenario: No canonical gate hash
- **WHEN** no `--gate-sha256`, `$FORGECTL_GATE_SHA256`, or canonical script beside the registry is available
- **THEN** every `release-pr` row reads `unknown`

#### Scenario: Lint copy drift
- **WHEN** a repo's `.github/scripts/lint-release-env.sh` hash differs from the canonical hash
- **THEN** that repo's row is `stalled` with `lint_copy` `drift`, and the stall names `lint-release-env.sh`

#### Scenario: Lint copy missing
- **WHEN** a `release-pr` repo has no `.github/scripts/lint-release-env.sh` on its branch
- **THEN** that repo's row is `stalled` with `lint_copy` `missing`, as a missing gate copy is

#### Scenario: No canonical lint hash
- **WHEN** no `--lint-sha256`, `$FORGECTL_LINT_SHA256`, or `../scripts/release/lint-release-env.sh` beside the registry is available
- **THEN** every `release-pr` row reads `unknown`

### Requirement: Releasable commit detection
A commit SHALL be releasable when its subject type is `feat`, `fix`, or `perf`, its type or scope carries `!`, or its body has a `BREAKING CHANGE:` footer. `no-release-pr` MUST fire for a `release-pr` repo only when a releasable unreleased commit older than 24h exists with no open release PR. When the compare read is short of the branch's ahead count and shows nothing releasable, the row MUST be `unknown`.

#### Scenario: Only chores unreleased
- **WHEN** a repo's unreleased commits are all `chore` and `docs`
- **THEN** no `no-release-pr` stall fires

### Requirement: Fail-on-stall exit contract
`review releases --json` SHALL write a machine-readable report including each row's toggle source (`variable` or `runs`). With `--fail-on-stall`, the command MUST exit 1 when any row is `stalled` or `unknown`, so a read failure never passes as healthy; without it, the radar SHALL exit 0 after printing.

#### Scenario: Unknown row under fail-on-stall
- **WHEN** one repo's runs read returns HTTP 403 and `--fail-on-stall` is set
- **THEN** that row is `unknown` naming the failed read and the command exits 1
