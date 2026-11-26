# attention Specification

## Purpose
`forgectl attention` keeps one machine-wide queue of everything waiting on the operator: `list` merges waiting desk items, open desk asks, the last sweep ledger and a stuck drain, ordered by delay cost, and reads every source without writing.

## Requirements

### Requirement: Unix only
`forgectl attention` SHALL run only on a Unix system. On any other system each verb MUST exit 2 with a message saying it is not supported on this OS.

#### Scenario: Non-Unix host
- **WHEN** `forgectl attention list` runs on a non-Unix system
- **THEN** it exits 2

### Requirement: Pane comes from herdr, never a record
A row's `pane` SHALL be a pane `herdr agent list` reports. When two or more panes claim the same session, a desk or sweep row naming one of them MUST carry no `session`.

#### Scenario: Duplicate session claim
- **WHEN** two herdr panes report the same session id and one of them queued a desk item
- **THEN** that desk row carries no `session`

### Requirement: Desk and sweep read without writing
`list` SHALL open the desk read-only (creating, pinning, moving and chmodding nothing) and turn only waiting items into rows; nothing in `attention` runs, skips or approves a desk item. It SHALL add the blocked workers and `marker-clean` PRs from `sweep.json` schema 2; a sweep file that is not a regular file, is over 4 MiB, is not JSON or not schema 2 MUST count as one skipped entry.

#### Scenario: Malformed sweep file
- **WHEN** `sweep.json` holds a schema-1 ledger
- **THEN** `list` adds no sweep rows and counts one skipped entry

### Requirement: Open asks are rows
`attention list` SHALL add one row of kind `ask` for each open ask in the desk's asks log, at delay cost 4 and tier `count`, with the ask id as `tag` and its bucket as `bucket`. An answered ask MUST NOT appear, and a snoozed ask MUST NOT appear until its snooze ends. `sources.asks` SHALL report the log as `ok`, `absent` or `error`.

#### Scenario: Snoozed ask
- **WHEN** an ask is snoozed for two hours
- **THEN** `list` shows no row for it until the two hours have passed, and then shows it again

#### Scenario: Answered ask
- **WHEN** an ask has a `done` response
- **THEN** `list` shows no row for it

### Requirement: Asks read without writing
`list` SHALL read the asks log without taking a lock and without creating, writing or changing the mode of anything in the asks directory.

#### Scenario: Broad asks directory
- **WHEN** the asks directory has mode `0755`
- **THEN** `list` shows its asks and the directory keeps mode `0755`

### Requirement: Ask text is untrusted
An ask row's text SHALL be cleaned like every other row text. A log line that is not a well-formed record MUST be skipped and counted in `skipped`, and an ask whose id is not `ask-` and 8 hex digits MUST give no row.

#### Scenario: Escape sequence in a title
- **WHEN** an ask's title holds an escape sequence and a bidi override
- **THEN** the text view prints the title without them

### Requirement: Asks on another host
When `FORGECTL_DESK_ASKS_HOST` is set and neither `DESK_DIR` nor `CLAUDE_DESK_DIR` is, `list` SHALL NOT read any asks log and SHALL set `sources.asks` to `remote`.

#### Scenario: Remote asks
- **WHEN** `FORGECTL_DESK_ASKS_HOST` names a host and no desk directory variable is set
- **THEN** `list` shows no ask rows and `sources.asks` is `remote`

### Requirement: Queue order
`forgectl attention list` SHALL order rows by delay cost: 1 a blocked worker or a stuck drain, 2 a waiting desk item, 3 a `marker-clean` PR, 4 an open desk ask; ties go oldest first. `--json` MUST print `{"rows": [...], "skipped": N, "sources": {...}}` with `sources` reporting `desk`, `asks`, `herdr` and `sweep`.

#### Scenario: Desk item and ask
- **WHEN** the queue holds a waiting desk item and an open ask
- **THEN** the desk row is listed before the ask row

### Requirement: A blocked worker joins its desk row
A desk item SHALL absorb a worker the sweep reports `blockedBy: desk` from the same pane, and the merged row SHALL take the worker's cost.

#### Scenario: Worker blocked on the desk
- **WHEN** a worker blocked on the desk runs in the pane that queued a waiting item
- **THEN** `list` shows one desk row at cost 1 naming the worker

### Requirement: List exit codes
`attention list` SHALL exit 0 on success and 2 when a flag or the `[attention]` config is refused. It MUST NOT fail on a source it cannot read: it reports the other sources and says why in `sources`.

#### Scenario: Bad worker prefix
- **WHEN** `[attention] worker_prefix = "-x"` is configured
- **THEN** `attention list` exits 2 and names the key
