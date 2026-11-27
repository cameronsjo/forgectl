## ADDED Requirements

### Requirement: Watch lists
`events watch --session ID <pr>...` SHALL add each PR to the session's watch list and `events unwatch --session ID [<pr>...]` SHALL remove the named PRs, or every PR when none is named. A PR SHALL be named as `owner/repo#N` or a `https://github.com/owner/repo/pull/N` URL. A session id MUST be 1 to 128 letters, digits, `.`, `_` or `-`, not starting with `.`. A watch that would put a session past 50 PRs MUST be refused with nothing changed.

#### Scenario: Path in a session id
- **WHEN** an agent passes `--session ../x`
- **THEN** nothing is recorded and the exit code is 2

### Requirement: Poll records typed events
`events poll` SHALL read every watched PR from GitHub and, for each change since its last snapshot, record one event of kind `pr.pushed`, `pr.checks`, `pr.review`, `pr.ready`, `pr.citl`, `pr.merged` or `pr.closed`. A PR that cannot be read SHALL be skipped and keep its last snapshot.

#### Scenario: Merged
- **WHEN** a watched PR is merged
- **THEN** one `pr.merged` event is recorded and the PR comes off every watch list

### Requirement: First snapshot is not news
A PR's first snapshot SHALL record no event.

#### Scenario: New watch
- **WHEN** a session watches an open PR with green checks and the poller reads it for the first time
- **THEN** no event is recorded

### Requirement: Check events
A check rollup SHALL make a `pr.checks` event only when it is `SUCCESS`, `FAILURE` or `ERROR` and differs from the last one or the head moved.

#### Scenario: Checks fail
- **WHEN** a watched PR's rollup goes from `PENDING` to `FAILURE` at the same head
- **THEN** one `pr.checks` event with `checks: FAILURE` is recorded

### Requirement: Review events
A `pr.review` event SHALL be made when more cadence-review markers from the viewer's own reviews count at the head than before, carrying the summed `crit` and `imp`.

#### Scenario: Marker from another login
- **WHEN** a marker at the head is posted by a login other than the viewer's
- **THEN** no review event is recorded

### Requirement: Events carry typed fields only
An event SHALL carry only a seq, a time, a kind, the repository, the PR number, a full head SHA, a check rollup and review counts. Every field MUST be validated before a line is written and again after it is read; a line that fails is skipped and counted. No PR title, body, label text other than the CITL flag, comment or reviewer name SHALL be stored.

#### Scenario: Damaged line
- **WHEN** a feed line holds an unknown kind
- **THEN** `events feed --json` leaves it out and counts it in `skipped`

### Requirement: Feeds per session
Each event SHALL be appended to the machine log and to the feed of every session watching its PR, with the same seq. `events feed --session ID --since N` SHALL print the session's events with a seq above N, oldest first; `--json` SHALL print `{"events", "last", "skipped"}`.

#### Scenario: Two sessions watch one PR
- **WHEN** two sessions watch a PR whose checks finish
- **THEN** both feeds hold the same `pr.checks` event

### Requirement: Snapshot reader
`events state <pr>` SHALL print the PR's last snapshot: state, draft, head, check rollup, review counts at head, CITL, the repository's allowed merge methods and `polled_at`. A PR with no snapshot MUST exit 3 with nothing on stdout.

#### Scenario: Never polled
- **WHEN** `events state o/r#9` names a PR the poller never read
- **THEN** the exit code is 3

### Requirement: Private store
The store SHALL live in `$XDG_STATE_HOME/forgectl/events`, else `~/.local/state/forgectl/events`, as a directory of mode 0700 pinned without following symlinks. Its files SHALL be mode 0600, opened without following a symlink. Writers SHALL hold an exclusive flock and readers a shared one. A reader of a store that does not exist SHALL see no events and no snapshots and create nothing.

#### Scenario: Reader before the first poll
- **WHEN** `events feed` runs before anything was ever watched
- **THEN** it prints no events, exits 0, and creates no directory
