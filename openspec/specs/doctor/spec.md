# doctor Specification

## Purpose
`forgectl doctor` is the ecosystem health check: it runs independent checks over claude, the terminal tools, gh auth, config, the local bench, the workflow trust store and forgectl's own currency, and reports each with a state and a fix.

## Requirements

### Requirement: Independent checks
`forgectl doctor` SHALL run every check independently, so one failing check MUST NOT hide or skip another. It SHALL reuse each domain's own check rather than reimplementing it, and it MUST NOT take positional arguments.

#### Scenario: gh missing and config broken
- **WHEN** gh is not on PATH and `config.toml` does not parse
- **THEN** both the `gh` and the `config` checks are reported as `fail`, alongside every other check

### Requirement: Closed check state vocabulary
Each check SHALL report a `state` of `ok`, `warn`, `fail`, or `skip`. `skip` MUST mean an optional integration that is not configured on this machine, never a failure. A `warn` or `fail` check SHOULD carry a remediation hint.

#### Scenario: Bench not configured
- **WHEN** no hearth directory is configured
- **THEN** the `hearth` check is `skip`, not `fail`

### Requirement: Exit status reflects failures only
`doctor` SHALL exit 0 when no check is `fail` (`warn` and `skip` exit 0) and exit 1 when at least one check is `fail`. Without `--json`, a failing run MUST end with a line naming the failed checks.

#### Scenario: One failed check
- **WHEN** only the `gh` check fails
- **THEN** the exit status is 1 and the closing line reads `doctor found 1 problem: gh (each row's hint names the fix)`

#### Scenario: Warnings only
- **WHEN** tmux is missing (a `warn`) and nothing fails
- **THEN** the exit status is 0

### Requirement: JSON report survives a failure
`doctor --json` SHALL write `{"checks":[{"name","state","detail","hint"}],"healthy":bool}` to stdout, omitting `detail` and `hint` when empty. When a check fails it MUST still write the full report to stdout and exit 1, with no error object on stderr.

#### Scenario: Failing check under --json
- **WHEN** `forgectl doctor --json` runs and the `chronicle` check fails
- **THEN** stdout holds the full report with `"healthy": false` and the exit status is 1

### Requirement: Runs on a broken config
`doctor` SHALL run when `config.toml` does not parse or cannot be read, and MUST report that as a `fail` on its `config` check rather than refusing to start. With no config file, the `config` check MUST be `skip` with a hint to run `forgectl init`; a file that exists and validates MUST be `ok`.

#### Scenario: No config file
- **WHEN** no `config.toml` exists
- **THEN** the `config` check is `skip` with a hint naming `forgectl init`

### Requirement: Config check validates semantics
The `config` check SHALL strictly decode the file and run each section's own validation, so a file that parses but holds a value a verb would refuse (including a bad `[theme]`) MUST report `fail`.

#### Scenario: Unknown theme preset
- **WHEN** `[theme] preset = "neon"` is set
- **THEN** the `config` check is `fail` and its detail names the bad value

### Requirement: Trust store is optional
A machine with no workflow trust anchor and no trust store SHALL report the `trust store` check as `skip`. A present but insecure anchor, or a store whose anchor is gone, MUST be `fail`.

#### Scenario: Blessed workflows never set up
- **WHEN** neither a trust anchor nor a trust store exists
- **THEN** the `trust store` check is `skip` with a hint naming `forgectl workflow trust init`

### Requirement: Doctor has no side effects
`doctor` MUST only read: the `log path` check SHALL stat the log directory and never create it, and no check SHALL install, upgrade, or write anything.

#### Scenario: Log directory absent
- **WHEN** the resolved log directory does not exist
- **THEN** the `log path` check is `warn` and the directory is still absent afterwards

### Requirement: Captured output is escaped
Check details and hints can carry text captured from external commands. The human report MUST escape newlines, carriage returns and terminal control sequences in them, so a check line cannot be forged or overwritten.

#### Scenario: Hostile brew output
- **WHEN** a check's detail contains a newline followed by a fake `ok` line
- **THEN** the human report shows it escaped on the same line, not as a second check line

### Requirement: Source builds are not tracked for currency
The `forgectl version` check SHALL be `skip` for a source build, and otherwise SHALL report `ok` when forgectl is current against the Homebrew tap, or `warn` when brew is missing, the check fails, or an upgrade is available.

#### Scenario: Outdated install
- **WHEN** the Homebrew tap has a newer forgectl
- **THEN** the `forgectl version` check is `warn` with a hint naming `forgectl upgrade`
