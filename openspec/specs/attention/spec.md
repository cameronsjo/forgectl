# attention Specification

## Purpose
`forgectl attention` keeps one machine-wide queue of everything waiting on the operator: Claude Code sessions publish their `What's next:` items with `put`, and `list` merges them with waiting desk items and the last sweep ledger, ordered by delay cost.

## Requirements

### Requirement: Unix only
`forgectl attention` SHALL run only on a Unix system. On any other system each verb MUST exit 2 with a message saying it is not supported on this OS.

#### Scenario: Non-Unix host
- **WHEN** `forgectl attention list` runs on a non-Unix system
- **THEN** it exits 2

### Requirement: Publish a reply's items
`forgectl attention put --session ID --turn ID` SHALL read a reply from stdin, parse the last `What's next:` block outside fenced code, keep at most 20 question and action items, and write them as that session's record. It MUST print `items=`, `seen=` and `read=`. stdin MUST NOT be a terminal (exit 2), a reply over 16 MiB MUST be refused, and a reply over 64 KiB MUST be parsed from its last 64 KiB.

#### Scenario: Reply with two items
- **WHEN** a reply ending in a `What's next:` list with one `**Question 1**:` and one `**Action 2 (Claude)**:` line is piped to `put`
- **THEN** the session's record holds one `q` item and one `a` item
- **AND** `put` exits 0

#### Scenario: Terminal on stdin
- **WHEN** `put` runs with stdin attached to a terminal
- **THEN** it exits 2 and writes nothing

### Requirement: put --json echo
With `--json`, a successful `put` SHALL print exactly one object `{session, turn, items, seen, read}` on stdout, with every key always present (`actor` and `rec` as `""` when absent, `items` as `[]` when the reply had no block). A refused `put --json` MUST print nothing on stdout and one `{"error","code","path"}` object on stderr, with the same exit code as without `--json`.

#### Scenario: Reply with no block
- **WHEN** `put --json` receives a reply with no `What's next:` heading
- **THEN** stdout is one JSON object whose `items` is `[]`

### Requirement: Item text is cleaned
Every item text SHALL be one line of at most 200 characters with control, bidi and invisible characters removed. A `Recommended: X` value MUST be cut from the text and mapped to `rec` as `now`, `later`, `park` or `forget` (`go` and `yes` read as `now`, `defer` as `later`, `leave` as `forget`); an unmapped value stays in the text and sets no `rec`.

#### Scenario: Recommended value
- **WHEN** an item line ends in `Recommended: defer`
- **THEN** the stored item's `rec` is `later` and its text no longer holds `Recommended: defer`

### Requirement: Private spool
Records SHALL live in `$XDG_STATE_HOME/cadence/attention` (default `~/.local/state/cadence/attention`), one `<session>.json` file at mode `0600` per session, where the session id MUST match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`. `put` MUST create the directory at `0700` and MUST refuse, with exit 1, one that is a symlink, another user's, or has any permission bit beyond `0700`. Writes MUST go through an `O_EXCL` temporary file renamed over the record.

#### Scenario: Spool directory too open
- **WHEN** the spool directory has mode `0755`
- **THEN** `put` exits 1 and leaves the directory as it is

### Requirement: The spool is untrusted on read
On every read, `list` SHALL re-validate every record field, refuse unknown fields, require the record to name the session its file is named for, and clean every text again. A record that fails MUST be skipped and counted in `skipped`, and its content MUST never be printed. `origin` is a display label only and MUST NOT gate any action.

#### Scenario: Record names another session
- **WHEN** `s1.json` holds a record whose session is `s2`
- **THEN** `list` skips it, counts it in `skipped`, and prints none of its text

### Requirement: Delay-cost order
`forgectl attention list` SHALL order rows by delay cost: 1 a blocked worker, 2 a waiting desk item, 3 a `marker-clean` PR, 4 an `Action (Cameron)`, 5 a question, 6 an action Claude offers; ties go oldest first. `--json` MUST print `{"rows": [...], "skipped": N, "sources": {...}}` with `sources` reporting `spool`, `desk`, `herdr` and `sweep`.

#### Scenario: Desk item and question
- **WHEN** the queue holds a waiting desk item and a session's question
- **THEN** the desk row is listed before the question row

### Requirement: One row per wait
A desk item SHALL absorb a worker the sweep reports `blockedBy: desk` from the same pane, and a session's `Action (Cameron)` led by `run`, `bash`, `brew`, `restart` or `sudo` when that session's herdr-listed pane queued a waiting item during or after that turn. A run-type action with no such item MUST carry `notOnDesk: true`. A record's own `--pane` MUST NOT cause a merge.

#### Scenario: Run action not staged
- **WHEN** a session lists `**Action 1 (Cameron)**: run the migration` and its pane queued no desk item
- **THEN** that row carries `notOnDesk: true`

### Requirement: Pane comes from herdr, never a record
A row's `pane` SHALL be the pane `herdr agent list` reports for its session, never one read from a record. When two or more panes claim the same session, that session MUST count as not in herdr: no `pane`, `origin` `unverified` (or `worker`), and no desk merges.

#### Scenario: Duplicate session claim
- **WHEN** two herdr panes report the same session id
- **THEN** that session's rows carry no `pane`

### Requirement: Desk and sweep read without writing
`list` SHALL open the desk read-only (creating, pinning, moving and chmodding nothing) and turn only waiting items into rows; nothing in `attention` runs, skips or approves a desk item. It SHALL add the blocked workers and `marker-clean` PRs from `sweep.json` schema 2; a sweep file that is not a regular file, is over 4 MiB, is not JSON or not schema 2 MUST count as one skipped entry.

#### Scenario: Malformed sweep file
- **WHEN** `sweep.json` holds a schema-1 ledger
- **THEN** `list` adds no sweep rows and counts one skipped entry

### Requirement: Pruning
`list`, and `put` after it writes, SHALL prune the spool: a record with a pane on the caller's herdr socket stays while herdr lists that pane and goes when it does not; any other record goes once older than `[attention] ttl_hours` (default 24, range 1 to 720). `put` MUST never prune the record it just wrote, and a housekeeping failure after the write MUST NOT change its exit 0.

#### Scenario: Record outlives its TTL
- **WHEN** a record with no pane is older than `ttl_hours`
- **THEN** the next `list` removes it

### Requirement: Exit codes
`attention` SHALL exit 0 on success; 1 when `put` or `rm` finds the spool refused or unwritable; 2 when a flag or the `[attention]` config is refused or `put`'s stdin is a terminal, with nothing written. `list` MUST NOT fail on a refused spool: it reports desk and sweep rows, sets `sources.spool` to `error`, and names the reason on stderr.

#### Scenario: ttl out of range
- **WHEN** `[attention] ttl_hours = 0` is configured
- **THEN** `attention list` exits 2 and names the key
