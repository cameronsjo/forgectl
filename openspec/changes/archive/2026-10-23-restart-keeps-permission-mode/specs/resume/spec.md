## MODIFIED Requirements

### Requirement: Restart stops only provably safe sessions
`forgectl resume restart --outdated` MUST signal a session only after re-checking that the pid is the same claude that registered it, the status is exactly `idle`, the herdr pane's foreground is that pid, and the input line is empty. It SHALL snapshot first, then relaunch `forgectl resume --keep-permission-mode <id>` in the same pane. Exit is 0 when all sessions resumed or were skipped, 1 on failure or timeout, 2 on bad usage.

#### Scenario: Busy session waits
- **WHEN** an outdated session is busy
- **THEN** restart re-checks it until `--timeout` and does not signal it while busy

#### Scenario: Missing selector
- **WHEN** the operator runs `forgectl resume restart` without `--outdated`
- **THEN** the command exits 2

#### Scenario: Restarted session keeps its mode
- **WHEN** a session in `acceptEdits` is restarted
- **THEN** the relaunch line carries `--keep-permission-mode` and the session resumes in `acceptEdits`

## ADDED Requirements

### Requirement: Resume can keep the session's permission mode
`forgectl resume --keep-permission-mode` SHALL start claude with the permission mode the session's transcript last recorded. When no mode is recorded, or the recorded one is refused, the posture's mode MUST apply and a note on stderr SHALL say why. Without the flag, resume MUST use the posture's mode.

#### Scenario: Mode recorded
- **WHEN** the transcript's last mode is `acceptEdits` and the flag is passed
- **THEN** resume starts claude with `--permission-mode acceptEdits`

#### Scenario: Manual resume
- **WHEN** the operator runs `forgectl resume <id>` without the flag
- **THEN** resume starts in the posture's mode

### Requirement: A kept permission mode is validated
A mode read from a transcript MUST exactly match one of Claude Code's known `--permission-mode` values before it reaches the argv. `bypassPermissions` MUST carry only when the posture could reach it: `allow_danger` on after the off-terminal clear, or the posture's own mode is `bypassPermissions`.

#### Scenario: Unknown value in the transcript
- **WHEN** the transcript's last mode is `plan --dangerously-skip-permissions`
- **THEN** resume starts in the posture's mode and its argv carries no transcript text

#### Scenario: Bypass off a terminal
- **WHEN** the transcript's last mode is `bypassPermissions` and stdout is not a terminal
- **THEN** resume starts in the posture's mode

### Requirement: Update watcher announces restarts
When the `[[resume.on_update]]` restart action restarts a session, it SHALL post a desktop notification naming the session and pane before the signal, and one summary notification counting each final state when the run ends. `resume restart` and the watcher log SHALL print a `summary` line.

#### Scenario: One session restarted
- **WHEN** the watcher restarts one idle outdated session
- **THEN** a "restarting a session" notification is posted before the stop and a summary reads `1 session(s): 1 resumed`
