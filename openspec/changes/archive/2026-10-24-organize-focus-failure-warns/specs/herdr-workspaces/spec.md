## MODIFIED Requirements

### Requirement: Apply sequence and focus restore
`--apply` SHALL move each tab (locating it again by terminal id before each move), then order workspaces, then tabs within each, then restore focus: each workspace's active tab first, the caller's tab last. A move that would empty its workspace MUST be reported as `blocked` rather than attempted. When every step succeeded and only the focus restore fails, the command SHALL print `warning: could not restore focus: <why>` and exit 0.

#### Scenario: Last tab in a workspace
- **WHEN** a planned move would leave its source workspace with no tabs
- **THEN** the move is reported `blocked` and not made

#### Scenario: Focus restore fails after a clean run
- **WHEN** every move lands and herdr refuses the final `tab focus`
- **THEN** the command prints a warning naming the focus failure and exits 0

### Requirement: JSON output
With `--json`, the command SHALL write one object `{"plan","result"}` to stdout and the human report to stderr. `plan` carries `moves`, `layout`, `unmatched`, and `warnings`; `result` carries `applied`, `blocked`, `not_run`, `warnings`, and `error`, including on exit 1. `result.warnings` MUST be an array, empty when there is nothing to report.

#### Scenario: Apply failure under JSON
- **WHEN** `--apply --json` exits 1
- **THEN** stdout still holds the object with `result.error` set

#### Scenario: Focus warning under JSON
- **WHEN** `--apply --json` finishes every move and the focus restore fails
- **THEN** the command exits 0 and `result.warnings` names the focus failure
