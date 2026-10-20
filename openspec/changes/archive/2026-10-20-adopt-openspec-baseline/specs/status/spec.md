## Purpose
`forgectl status` is a read-only overview across the workbench: local git state, PRs, reclaimable build directories and bench health, each section contained so one slow or broken source never hides the others.

## ADDED Requirements

### Requirement: Read-only overview of four sections
`forgectl status` SHALL report four sections, `git`, `prs`, `clean` and `bench`, by reusing the read paths of `projects list`, `pr dash`, the `clean` dry run and `bench status`. It MUST change nothing, and its only network traffic SHALL be the `prs` section's `gh` searches and the `bench` section's health probes.

#### Scenario: Clean dry run only
- **WHEN** `forgectl status` runs
- **THEN** no build directory is removed and only the reclaimable total is reported

### Requirement: Per-section failure containment
Each section SHALL run concurrently under its own deadline set by `--timeout` (default 20s). A section whose source errors, panics, or misses the deadline MUST be reported as `failed` while the other sections still report. A source that returns after its deadline MUST be treated as failed even when it returned data.

#### Scenario: One slow section
- **WHEN** the `clean` source runs past `--timeout 5s`
- **THEN** `clean` is `failed` with error `timed out after 5s`
- **AND** `git`, `prs` and `bench` report their own results

#### Scenario: Source panics
- **WHEN** a section's source panics
- **THEN** that section is `failed` with a categorical error, and the command does not crash

### Requirement: Exit status ignores section health by default
`status` SHALL exit 0 whatever the sections report. With `--strict` it SHALL write the full report and then exit 1 when any section is not `ok`; under `--json` that exit MUST add nothing to stderr.

#### Scenario: Strict with a degraded section
- **WHEN** `forgectl status --json --strict` runs and `prs` is `degraded`
- **THEN** the full report is on stdout, stderr has no error object, and the exit status is 1

#### Scenario: Not strict
- **WHEN** `forgectl status` runs and a section failed
- **THEN** the exit status is 0

### Requirement: Stable --json section shape
`status --json` SHALL emit an object whose keys `git`, `prs`, `clean` and `bench` are always present. Each section MUST carry `state` (`ok`, `degraded` or `failed`), `error` (`""` unless failed), `notes` (`[]` unless degraded) and `data` (null only when failed). `state` says whether the source could be read, not whether what it found is healthy.

#### Scenario: Bench component down
- **WHEN** the bench source answers and reports chronicle unavailable
- **THEN** the `bench` section's `state` is `ok` and its `data` says chronicle is unavailable

### Requirement: Untrusted text is escaped and capped
Every section error and note SHALL be escaped for the terminal and capped at 200 characters, in both the JSON and the human view. Names, titles, paths and reasons in the human view and the cockpit MUST also be escaped.

#### Scenario: Error with a control sequence
- **WHEN** a source error contains an ANSI escape
- **THEN** the section's `error` shows it escaped, not as a raw control byte

### Requirement: Bounded output with --limit
`--limit N` SHALL keep at most N rows of `git.projects`, `prs.active_reviews`, `prs.awaiting_you` and `prs.your_open`, and under `--json` SHALL add a top-level `bound` object (`limit`, `truncated`, `cut`, `hint`) that names each list cut. `--limit 0` MUST keep every row and still write `bound`. Without `--limit` there MUST be no `bound` key. The `git` totals always count every project.

#### Scenario: Cut project list
- **WHEN** 138 projects exist and `forgectl status --json --limit 20` runs
- **THEN** `git.projects` holds 20 rows, sorted attention-first
- **AND** `bound.cut` holds `{list: "git.projects", total: 138, shown: 20}`

#### Scenario: Limit zero
- **WHEN** `forgectl status --json --limit 0` runs
- **THEN** `bound` is `{"limit": 0, "truncated": false, "cut": []}` and nothing is cut

### Requirement: Flag refusals before any source runs
`status` MUST refuse, before any section runs and with exit 1, a `--timeout` of zero or less, a negative `--limit` (code `usage_error` under `--json`), `--limit` with `--tui`, and `--tui` with `--json` or `--strict`.

#### Scenario: Zero timeout
- **WHEN** `forgectl status --timeout 0s` is run
- **THEN** it exits 1 and no section source runs

#### Scenario: Negative limit under --json
- **WHEN** `forgectl status --json --limit -1` is run
- **THEN** stdout is empty, stderr holds one object with code `usage_error`, and the exit status is 1

### Requirement: Human view leads with a verdict
Without `--json`, the first line SHALL say how many problems need attention, naming the affected sections and a reason, or `nothing needs you`. Sections sharing a cause MUST count as one problem. Each section line leads with its state glyph, and lists are capped at 10 projects and 5 PRs unless `--limit` is given.

#### Scenario: Missing projects folder
- **WHEN** the projects root does not exist, so `git` and `clean` both fail
- **THEN** the verdict reads `1 problem: git, clean (no projects folder)`

### Requirement: Cockpit needs a real terminal
`status --tui` SHALL open a full-screen cockpit only when stdin and stdout are terminals; otherwise it MUST exit 1 and point at `--json`. Under `TERM=dumb` it MUST exit 1 and point at plain `status`. The cockpit MUST only read; its one action, `enter` on a PR row, quits the cockpit and then runs `forgectl pr <ref>`.

#### Scenario: Cockpit from a pipe
- **WHEN** `forgectl status --tui` runs with stdout piped
- **THEN** it exits 1 with a message pointing at `status --json`

### Requirement: Cockpit refresh limits
In the cockpit, only `git` SHALL refresh on its own, every 60 seconds, with no network call. `prs`, `clean` and `bench` MUST refresh only on `r` or `R`. A section MUST refresh at most once every 15 seconds, run at most one refresh at a time, and refuse another refresh until a timed-out source has actually returned.

#### Scenario: Refresh pressed twice
- **WHEN** `r` is pressed on `prs` twice within 15 seconds
- **THEN** the second press is refused with a wait message and no second `gh` search runs
