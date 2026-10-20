## Purpose
The agent contract makes every forgectl verb drivable without a TTY: honest exit codes from one table, a stable `--json` stdout and stderr shape, stream discipline, and no interaction when no person is present.

## ADDED Requirements

### Requirement: No interaction without a terminal
A verb MUST NOT open a picker, prompt, or confirmation unless stdin and stdout are both terminals. When a choice would be needed and no terminal is present, the verb SHALL print the candidate set to stdout, one per line, and exit non-zero without acting on any candidate.

#### Scenario: Picker verb run from a pipe
- **WHEN** `forgectl pr pick` finds two open PRs and stdout is not a terminal
- **THEN** both candidates are written to stdout, one per line
- **AND** the command exits 1 with an error saying a selection is required, and no review is started

### Requirement: Bare invocation never succeeds silently off a terminal
Bare `forgectl` SHALL open the hub only when stdin and stdout are terminals. Off a terminal it MUST print the usage text to stderr and exit non-zero, so a script never reads silence as success. Under `TERM=dumb` it MUST refuse to draw, point at `forgectl menu`, and exit 2.

#### Scenario: Bare invoke from a script
- **WHEN** `forgectl` runs with no arguments and stdout is a pipe
- **THEN** usage is written to stderr and the exit status is non-zero

#### Scenario: Bare invoke on a dumb terminal
- **WHEN** `forgectl` runs with no arguments on a TTY with `TERM=dumb`
- **THEN** it exits 2 and the message points at `forgectl menu`

### Requirement: One exit-code table
Every verb SHALL map its outcome to one table: 0 ok (including already done and an empty result), 1 failed, 2 usage (nothing attempted, fixable by the caller), 3 unauthorized, 4 refused by a safety rule. Codes 5 and 6 are reserved and MUST NOT be emitted. An error with no class MUST exit 1.

#### Scenario: Unknown flag
- **WHEN** a verb outside the documented exceptions is run with an unknown flag or the wrong number of arguments
- **THEN** it exits 2

#### Scenario: Unknown verb
- **WHEN** `forgectl zzbogus` is run
- **THEN** it exits 2, including when `--help` is also passed

### Requirement: Usage exceptions keep exit 1
Where exit 2 already means something else in a verb, usage errors MUST exit 1 instead: every `tasks` subverb, `env check`, `k8s`, `resume snapshot` and `surface event`. `resume snapshot` and `surface event` are Claude Code hook verbs and MUST NOT exit 2 for a bad flag, an unresolvable `$HOME`, or an unparseable config.

#### Scenario: Bad flag on a hook verb
- **WHEN** `forgectl resume snapshot --bogus` is run
- **THEN** the exit status is not 2

### Requirement: Cancelled prompt exits 130
Backing out of a forgectl picker or confirm (Esc, Ctrl+C, or No at a Yes/No confirm) SHALL print `cancelled` and exit 130, never 0 and never 1. An abort MUST write `cancelled` to stderr; a No at a confirm writes it to stdout. A cancel MUST NOT be rendered as a styled error.

#### Scenario: Esc at a picker
- **WHEN** the operator presses Esc in the `resume` picker
- **THEN** `cancelled` is written to stderr and the process exits 130

#### Scenario: Esc at the bare hub
- **WHEN** the operator closes the bare `forgectl` hub with Esc
- **THEN** the process exits 0

### Requirement: JSON verdict on stdout keeps its exit code
A `--json` verb that reports a problem as its result SHALL write the full JSON verdict to stdout, exit with its documented non-zero code, and write no error object or human error frame to stderr.

#### Scenario: Doctor finds a failure under --json
- **WHEN** `forgectl doctor --json` finds a failing check
- **THEN** the full report is on stdout, nothing error-shaped is on stderr, and the exit status is 1

### Requirement: JSON failure object on stderr
When a `--json` verb fails before emitting its verdict, stdout MUST stay empty and stderr MUST carry exactly one JSON object on one line, `{"error","code","path"}`, with all three keys present. `code` SHALL be `usage_error` when nothing was attempted and `failed` otherwise; `path` is `""` unless one resolved file is to blame.

#### Scenario: Bad flag with --json
- **WHEN** `forgectl status --json --bogus` is run
- **THEN** stdout is empty and the last line of stderr is one object with `code` `usage_error`
- **AND** the exit status is 2

#### Scenario: Bad flag before --json
- **WHEN** `forgectl status --bogus --json` is run
- **THEN** the failure is still reported as the JSON object, even though parsing stopped before `--json`

### Requirement: --json as another flag's value does not count
When the token `--json` is consumed as the value of a flag that takes a value, the failure SHALL be reported as human text, not as the JSON object. A `--json` after a `--` terminator MUST NOT count either.

#### Scenario: --json as a host value
- **WHEN** `forgectl projects list --host --json --bogus` is run
- **THEN** the error is the human message, because `--json` is the value of `--host`

### Requirement: --json never changes the exit code
The same failure SHALL exit with the same status with or without `--json`.

#### Scenario: Same failure, both modes
- **WHEN** a verb fails once with `--json` and once without
- **THEN** both runs exit with the same status

### Requirement: Pre-verb failures follow the contract
Failures raised before any verb runs (an unset `$HOME`, a relative `$XDG_CONFIG_HOME`, or a `config.toml` that does not parse or cannot be read) SHALL exit 2, except for the usage-exception verbs. Without `--json` they MUST print one plain `forgectl: <message>` line; when the target verb declares `--json` and it was passed, stderr MUST carry that verb's one failure object instead.

#### Scenario: Unset HOME with --json
- **WHEN** `forgectl status --json` runs with `$HOME` unset
- **THEN** stderr carries one failure object with code `failed`, stdout is empty, and the exit status is 2

### Requirement: Stream discipline and colour
Payload SHALL go to stdout and diagnostics to stderr. Styled output MUST carry no ANSI escapes when stdout is not a terminal or when `NO_COLOR` is set to any non-empty value, and `NO_COLOR` MUST override `CLICOLOR_FORCE`, including for help and error frames.

#### Scenario: Help into a pipe under NO_COLOR
- **WHEN** `NO_COLOR=yes CLICOLOR_FORCE=1 forgectl --help | cat` is run
- **THEN** the output contains no escape sequences
