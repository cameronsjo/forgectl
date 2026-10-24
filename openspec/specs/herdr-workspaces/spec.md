# herdr-workspaces Specification

## Purpose
`forgectl herdr organize` groups the tabs of a running herdr session into workspaces by configured glob rules and orders tabs and workspaces, reporting the plan by default and applying it only on request.

## Requirements

### Requirement: Organize preconditions
`forgectl herdr organize` MUST exit 2 before any herdr call when it is not running in a herdr pane (`HERDR_ENV=1` and a `HERDR_SOCKET_PATH` naming a live socket), when no `[[herdr.organize.rule]]` is configured, or when the `[herdr.organize]` config is invalid. When several of these fail it SHALL report them together in one error.

#### Scenario: Outside herdr with no rules
- **WHEN** the command runs outside a herdr pane with no rules configured
- **THEN** it exits 2 and the error names both problems

#### Scenario: Legacy rules file present
- **WHEN** no rules are configured and `~/.config/herdr-organize/rules.toml` exists
- **THEN** the exit-2 error says forgectl no longer reads that file and how to move its keys into `config.toml`

### Requirement: Configuration validation
The `[herdr.organize]` section SHALL take `default`, `workspace_order`, and `[[herdr.organize.rule]]` entries with `glob` and `workspace`. `default` MUST be set once a rule exists; a label listed twice in `workspace_order`, a glob that does not compile, or any label in `default`, a rule, or `workspace_order` that exceeds 64 bytes, starts with `-`, or carries control, bidi, or invisible characters MUST be a config error (exit 2).

#### Scenario: Duplicate order label
- **WHEN** `workspace_order = ["forge", "forge"]`
- **THEN** the command exits 2 naming the duplicate

### Requirement: Rule matching
Each tab SHALL go to the workspace of the first rule whose glob matches `"<cwd> :: <title>"` for one of its panes, first pane first. Globs follow shell rules where `*` also matches `/`, and matching is case-sensitive. A tab no rule matches goes to `default`. A tab is identified by its first pane's terminal id; a tab whose first pane has none is skipped with a warning and left in place.

#### Scenario: Unmatched tab
- **WHEN** no rule glob matches any pane of a tab
- **THEN** the report lists it as `unmatched` and plans it into the `default` workspace

### Requirement: Deterministic ordering
Within a workspace, tabs SHALL be ordered by wing, then repo, then cwd, then first-pane terminal id, where wing and repo are the first two path parts under `$PROJECTS_DIR` (else `~/Projects`), a `.claude/worktrees/` path sorts with its repo, and a cwd outside the root sorts last. Workspaces MUST follow `workspace_order` left to right, with unlisted workspaces keeping their relative order after the listed ones.

#### Scenario: Same cwd across runs
- **WHEN** two tabs share a cwd
- **THEN** they are ordered by terminal id, giving the same order on every run

### Requirement: Report is read-only by default
Without `--apply`, the command SHALL only read the session and print a report of `move`, `blocked`, `order`, and `unmatched` lines (or `organized: N tabs in M workspaces; nothing to do`) and MUST NOT change the session. It works on stock herdr. `--explain` adds which rule caught each tab and each unmatched tab's match key.

#### Scenario: Dry report
- **WHEN** the user runs `forgectl herdr organize`
- **THEN** no tab or workspace moves and the exit is 0, even if it predicts herdr will block a move

### Requirement: Nothing is closed or renamed
The command MUST NOT close or rename any tab, pane, or workspace. Applying SHALL only move tabs, create a missing workspace for a rule's label, and reorder tabs and workspaces.

#### Scenario: Apply leaves titles alone
- **WHEN** `--apply` completes
- **THEN** every tab keeps its title and no tab is closed

### Requirement: Apply gates before any change
`--apply` MUST refuse with exit 2 before any change when the build has no file lock (non-Unix) or the herdr CLI lacks the fork's `tab move`. It SHALL serialize runs with a lock at `<config dir>/herdr-organize.lock`, print a waiting notice when another run holds it, and plan from a session read taken after acquiring the lock. It asks no confirmation.

#### Scenario: Stock herdr
- **WHEN** `--apply` runs against upstream herdr without `tab move`
- **THEN** the command exits 2 and nothing moves

### Requirement: Apply sequence and focus restore
`--apply` SHALL move each tab (locating it again by terminal id before each move), then order workspaces, then tabs within each, then restore focus: each workspace's active tab first, the caller's tab last. A move that would empty its workspace MUST be reported as `blocked` rather than attempted. When every step succeeded and only the focus restore fails, the command SHALL print `warning: could not restore focus: <why>` and exit 0.

#### Scenario: Last tab in a workspace
- **WHEN** a planned move would leave its source workspace with no tabs
- **THEN** the move is reported `blocked` and not made

#### Scenario: Focus restore fails after a clean run
- **WHEN** every move lands and herdr refuses the final `tab focus`
- **THEN** the command prints a warning naming the focus failure and exits 0

### Requirement: Failure handling without rollback
When herdr declines a move, `--apply` SHALL continue with stages that do not depend on it and exit 1 naming each refused move. Any other herdr failure MUST stop the run, restore focus, report what applied and what did not run, and exit 1. Nothing is rolled back; re-running recomputes the plan from the live session.

#### Scenario: Mid-run failure
- **WHEN** a herdr call fails after two of five moves
- **THEN** the output names the failed move, says `applied: 2 of 5 moves` with what did not run, restores focus, and exits 1

### Requirement: JSON output
With `--json`, the command SHALL write one object `{"plan","result"}` to stdout and the human report to stderr. `plan` carries `moves`, `layout`, `unmatched`, and `warnings`; `result` carries `applied`, `blocked`, `not_run`, `warnings`, and `error`, including on exit 1. `result.warnings` MUST be an array, empty when there is nothing to report.

#### Scenario: Apply failure under JSON
- **WHEN** `--apply --json` exits 1
- **THEN** stdout still holds the object with `result.error` set

#### Scenario: Focus warning under JSON
- **WHEN** `--apply --json` finishes every move and the focus restore fails
- **THEN** the command exits 0 and `result.warnings` names the focus failure
