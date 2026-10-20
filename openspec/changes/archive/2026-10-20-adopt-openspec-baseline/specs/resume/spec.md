## Purpose
`forgectl resume` finds a recent Claude Code session in any repository, moves into its working directory, restores its snapshotted tasks, and execs claude to continue or fork it.

## ADDED Requirements

### Requirement: Cross-repo session discovery
`forgectl resume [filter]` SHALL list recent sessions from Claude Code's own history and transcripts across every repository, matching the filter against repo, name, cwd, and id. A single match MUST resume directly after printing the matched row on stderr; several matches on a terminal SHALL open a picker of the newest 25.

#### Scenario: Unique filter
- **WHEN** exactly one session matches `forgectl resume forgectl`
- **THEN** stderr names the matched session and claude is exec'd in its recorded cwd

### Requirement: Headless ambiguity lists candidates with exit 1
When more than one session matches and `--dry-run` is set or stdin or stdout is not a terminal, resume MUST NOT open a picker. It SHALL print one candidate row per line on stdout and exit 1. A filter with no match MUST exit 1 with empty stdout.

#### Scenario: Ambiguous filter in a pipe
- **WHEN** a filter matches three sessions and stdout is piped
- **THEN** three candidate rows are written to stdout and the exit code is 1

#### Scenario: No match
- **WHEN** a filter matches no session
- **THEN** stdout is empty and the exit code is 1

### Requirement: Running sessions are refused unless forked
Resuming a session whose claude process is still live MUST be refused with exit 2, naming its pid, because two processes on one transcript corrupt it. `--fork` SHALL branch a new session off the transcript instead, whether or not the original is running.

#### Scenario: Live session
- **WHEN** the operator resumes a session that is running as pid 4242 without `--fork`
- **THEN** the command exits 2 and the message names pid 4242

#### Scenario: Fork a live session
- **WHEN** the operator passes `--fork` for a running session
- **THEN** claude is exec'd to fork a new session from that transcript

### Requirement: Dry run resolves without executing
`forgectl resume --dry-run` MUST print the resolved cwd and claude argv, exec nothing, never prompt, and exit with the code the real run would give, including the exit 2 refusals.

#### Scenario: Dry run of a live session
- **WHEN** `--dry-run` targets a running session
- **THEN** the argv is printed and the exit code is 2

### Requirement: Resume applies the target project's posture
The resumed session SHALL take the `[launch]` profile resolved for the session's own directory and the same injected launch environment as `forgectl launch`. With stdout not a terminal it MUST withhold `--allow-dangerously-skip-permissions`.

#### Scenario: Resume into another repo
- **WHEN** a session in a repo whose project block sets `model = "sonnet"` is resumed from elsewhere
- **THEN** the claude argv carries that repo's model

### Requirement: Bad launch proxy profile refuses with exit 2
When `[proxy] launch_profile` names an undefined or empty profile, resume MUST refuse with exit 2 before restoring any task, and this check SHALL run before `--dry-run` returns.

#### Scenario: Undefined launch profile
- **WHEN** `launch_profile = "missing"` and the operator runs `forgectl resume --dry-run x`
- **THEN** the command exits 2

### Requirement: Machine-readable listing
`forgectl resume ls --json` SHALL emit a JSON array on stdout with per-session `id`, `cwd`, `last_active`, `live`, and `tasks`, and optional `name`, `name_source`, `repo`, `branch`, `last_prompt`, `version`, and `pid`. Every subcommand MUST return rather than exec.

#### Scenario: Pre-check liveness
- **WHEN** a script runs `forgectl resume ls --json`
- **THEN** each element carries `live` and, for a live session, `pid`

### Requirement: Snapshot never fails a turn
`forgectl resume snapshot` SHALL capture each live session's `/rename` name and task bodies into a per-session JSON record in forgectl's config directory, and MUST always exit 0. Repeated snapshots MUST merge by task id and never drop previously captured tasks.

#### Scenario: Stop hook snapshot
- **WHEN** `forgectl resume snapshot --quiet` runs and a write fails
- **THEN** the command exits 0 with nothing on stdout
- **AND** the failure is reported on stderr

#### Scenario: Fewer live tasks
- **WHEN** a later snapshot sees fewer tasks than an earlier one
- **THEN** the earlier tasks remain in the record

### Requirement: Task restore never overwrites
On resume, restoring snapshotted tasks MUST NOT overwrite a task file that already exists for the session and MUST raise but never lower `.highwatermark`. A `--fork` SHALL report snapshotted tasks rather than restore them.

#### Scenario: Restore twice
- **WHEN** the same session is resumed twice
- **THEN** the second restore changes nothing on disk

### Requirement: Snapshot store pruning is bounded
The snapshot store SHALL self-prune at most once a day, retiring records whose Claude Code transcript is gone, and MUST refuse a pass that would retire most of the store. `FORGECTL_RESUME_NO_PRUNE` set to any non-empty value MUST disable pruning.

#### Scenario: Pruning disabled
- **WHEN** `FORGECTL_RESUME_NO_PRUNE=1` is set
- **THEN** no snapshot record is removed

### Requirement: Outdated sessions report is read-only
`forgectl resume outdated` MUST list live sessions whose recorded claude version is numerically older than the installed one, without signalling anything. It SHALL mark unparseable versions `version_unparseable`, exit 0 whether or not anything is outdated, exit non-zero only when the installed version cannot be determined, and emit `[]` under `--json` when none are outdated.

#### Scenario: Numeric comparison
- **WHEN** a session runs 2.1.99 and 2.1.100 is installed
- **THEN** the session is listed as outdated

### Requirement: Restart stops only provably safe sessions
`forgectl resume restart --outdated` MUST signal a session only after re-checking that the pid is the same claude that registered it, the status is exactly `idle`, the herdr pane's foreground is that pid, and the input line is empty. It SHALL snapshot first, then relaunch `forgectl resume <id>` in the same pane. Exit is 0 when all sessions resumed or were skipped, 1 on failure or timeout, 2 on bad usage.

#### Scenario: Busy session waits
- **WHEN** an outdated session is busy
- **THEN** restart re-checks it until `--timeout` and does not signal it while busy

#### Scenario: Missing selector
- **WHEN** the operator runs `forgectl resume restart` without `--outdated`
- **THEN** the command exits 2

### Requirement: Update hooks fire once per settled version
`forgectl resume hooks run` SHALL run `[[resume.on_update]]` entries only when the installed claude version has changed and settled; the first run records a baseline and fires nothing. Command hooks MUST run their argv with no shell and receive versions only through `FORGECTL_HARNESS`, `FORGECTL_OLD_VERSION`, `FORGECTL_NEW_VERSION`. Only `harness = "claude"` is accepted.

#### Scenario: First run
- **WHEN** `resume hooks run` runs with no recorded state
- **THEN** it records the current version and fires no hook

#### Scenario: Unsupported harness
- **WHEN** an entry sets `harness = "codex"`
- **THEN** it is refused as not supported yet

### Requirement: Hook audit trail redacts arguments
Each hook run MUST append one JSON line to `resume-hooks/runs.jsonl` recording only the command program's base name, never its arguments, and MUST keep stderr only for a failed command, truncated to 240 characters with the hook's argument values scrubbed. The files SHALL be owner-only.

#### Scenario: Webhook URL in argv
- **WHEN** a command hook's argv contains a webhook URL and it succeeds
- **THEN** the audit record names only the program base name and stores no output

### Requirement: Picker cancellation exits 130
Cancelling the interactive picker with Esc or Ctrl+C MUST print `cancelled` and exit 130.

#### Scenario: Esc in the picker
- **WHEN** the operator presses Esc in the resume picker
- **THEN** `cancelled` is printed and the exit code is 130
