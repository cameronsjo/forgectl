# sweep Specification

## Purpose
`forgectl sweep` builds the chief-of-staff ledger: it joins GitHub PRs, worker brief directories, herdr agent status and the desk into one schema-2 JSON ledger of what waits on the operator, which `forgectl attention list` reads.

## Requirements

### Requirement: Unix only
`forgectl sweep` SHALL run only on a Unix system. On any other system it MUST exit 2 with a message saying it is not supported on this OS.

#### Scenario: Non-Unix host
- **WHEN** `forgectl sweep` runs on a non-Unix system
- **THEN** it exits 2 and reads and writes nothing

### Requirement: No built-in defaults for owner, since, briefs or reviewers
`sweep` SHALL take `owner`, `since`, `briefs` and `reviewers` from the `[sweep]` config table, with `--owner`, `--since` and `--briefs` overriding the first three. A key set by neither, or set to an unusable value (an owner that is not a GitHub name, a `since` not `YYYY-MM-DD`, a relative `briefs`, a reviewer outside `[a-z0-9-]`), MUST exit 2 before anything is read, naming every such key at once without echoing the value.

#### Scenario: Two keys missing
- **WHEN** `forgectl sweep` runs with neither config nor flags for owner and since
- **THEN** it exits 2
- **AND** one message names both `sweep.owner` and `sweep.since`

### Requirement: Ledger output and location
`sweep` SHALL write the ledger (`schemaVersion` 2) to `$XDG_STATE_HOME/cadence/sweep.json` (default `~/.local/state/cadence/sweep.json`) on every successful run. With `--json` it MUST also print the ledger to stdout; without it, it prints a one-line summary and the path written. Schema 2 MUST keep every schema-1 field so a v1 reader still works.

#### Scenario: JSON run
- **WHEN** `forgectl sweep --json` succeeds
- **THEN** stdout is the ledger with `schemaVersion` 2, `generatedAt`, the counts, `workers`, `prs`, `opinions` and `sources`
- **AND** the same ledger is in `sweep.json`
- **AND** the exit code is 0

### Requirement: Atomic, owner-only write
`sweep` SHALL write a fresh `O_EXCL` temporary file with mode `0600` beside `sweep.json` and rename it over the target, replacing a symlink rather than following it. It MUST create `$XDG_STATE_HOME/cadence` at `0700` when absent, and MUST refuse with exit 1 a directory that is a symlink, another user's, or writable by group or others.

#### Scenario: State directory is group-writable
- **WHEN** `$XDG_STATE_HOME/cadence` exists with group write permission
- **THEN** `sweep` refuses the write and exits 1

### Requirement: Failed GitHub read keeps the last ledger
If GitHub or the briefs directory cannot be read, `sweep` SHALL exit 1 before writing, so a partial ledger never replaces a good one. A herdr or desk failure MUST degrade the ledger instead, reported in `sources` (`herdr: unavailable`, `desk: absent` or `error`), and MUST NOT stop the run.

#### Scenario: herdr is down
- **WHEN** `herdr agent list` cannot be called
- **THEN** the ledger is still written with `sources.herdr` set to `unavailable`
- **AND** no worker carries a `pane` or `blockedBy`

#### Scenario: GitHub unreachable
- **WHEN** the GitHub query fails
- **THEN** `sweep` exits 1 and `sweep.json` is left unchanged

### Requirement: Workers from brief directories
`sweep` SHALL treat as workers the directories under `briefs` whose names start with `[attention] worker_prefix` (default `cos-`) and that hold a `brief.md`, `picks.md`, `go.md` or `report.md` modified at or after `since`. Each worker's `phase` MUST be the furthest of `launched`, `plan`, `go`, `report` by its documented evidence, and its `repos` come from the brief's `## Where to work` bullets.

#### Scenario: Plan complete
- **WHEN** a worker directory holds `brief.md` and a `picks.md` whose last line is `PICKS-COMPLETE`, and no `go.md`
- **THEN** that worker's `phase` is `plan`

### Requirement: Briefs are untrusted input
`sweep` SHALL refuse symlinked worker directories and files (a symlinked worker is skipped and reported on stderr), open files without blocking so a FIFO is refused, and cap reads (256 KiB of `brief.md`, the last 4 KiB of each phase file, 500 worker directories, 50 repositories per brief). PR titles and brief text MUST be stripped of control, bidi and invisible characters before they enter the ledger.

#### Scenario: Symlinked worker directory
- **WHEN** a `cos-` entry under `briefs` is a symlink
- **THEN** it contributes nothing to the ledger and stderr reports it was skipped

### Requirement: PR state from trusted review markers only
Each open PR SHALL be the first of `draft`, `blocked` (conflicts, failing checks, or a marker at head with `crit>0`), `marker-clean` (a `crit=0` marker at head, none with `crit>0`, green checks, mergeable) or `waiting`. A `cadence-review` marker MUST count only when posted by the authenticated `gh` login, its `head=` equals the PR's current head, and its name is in `[sweep] reviewers`. The ledger MUST never report a PR as `ready`.

#### Scenario: Marker from another account
- **WHEN** a PR's only `crit=0` marker was posted by an account other than the authenticated login
- **THEN** the PR is not `marker-clean`

#### Scenario: Marker for an older head
- **WHEN** a `crit=0` marker names a commit that is no longer the PR's head
- **THEN** the marker is ignored

### Requirement: Desk read without writing
`sweep` SHALL read the desk only through the read-only scan `attention list` uses: nothing in the desk is pinned, moved, run, skipped or written. A worker MUST get `blockedBy: desk` when a waiting desk item's signal pane equals the worker's pane, else `blockedBy: prompt` when its herdr status is `blocked`.

#### Scenario: Worker waits on a desk item
- **WHEN** a worker's pane queued a desk item that is still waiting
- **THEN** that worker's `blockedBy` is `desk`
- **AND** the desk directory is unchanged byte for byte

### Requirement: Opinions never stand in for a GO
Each opinion SHALL carry a stable `id`, a `tone` (`act`, `warn`, `note`, `good`, sorted in that order), text, and an `action` of `unblock`, `review` or `none`. A worker whose plan is ready with no GO MUST yield action `none`, never an approval. Opinion text MUST name only validated worker and repository names, never a PR title.

#### Scenario: Plan awaiting GO
- **WHEN** a worker's plan has been ready 10 minutes with no `go.md` and its agent is idle
- **THEN** its opinion has `action` `none`
