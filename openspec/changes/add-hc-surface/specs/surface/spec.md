## MODIFIED Requirements

### Requirement: Explicit backend
`surface launch [target]` MUST require `--surface` naming `tmux`, `cmux`, `herdr`, or `hc`; there is no default and no detection. A bare target name SHALL resolve beneath the projects root by exact match and an ambiguous name MUST be refused; a path may be anywhere. `--harness` overrides the profile's harness, and `--name` overrides the display name.

#### Scenario: Missing backend
- **WHEN** the user runs `forgectl surface launch .` without `--surface`
- **THEN** the launch is refused and no surface is created

#### Scenario: Ambiguous project name
- **WHEN** a bare target name matches more than one project
- **THEN** the launch is refused rather than guessing

### Requirement: Worker launch
`--worktree <branch>` SHALL require `--surface herdr` or `--surface hc`, and `--name`. It SHALL create a git worktree at `<repo>/.claude/worktrees/<name>` with repository hooks disabled, give the worker its own herdr workspace or hc terminal labeled with forgectl's ownership marker, and record a ledger row. A new branch MUST start at the GitHub default branch head when origin is on github.com, else at `HEAD`; a failing GitHub call fails the launch. An existing branch is checked out as is.

#### Scenario: Worktree without herdr
- **WHEN** `--worktree` is given with `--surface tmux`
- **THEN** the launch is refused

#### Scenario: hc worker
- **WHEN** `--worktree feat/x --name x` is given with `--surface hc`
- **THEN** the worker runs in an hc terminal named `x` and the hc ledger for the repo and session holds its row

## ADDED Requirements

### Requirement: hc backend
`--surface hc` SHALL start the trampoline with `hc run --bg --outlive-parent --no-notify`, naming the terminal after the display name and labeling it `forgectl.surface=<ownership marker>`. A display name outside hc's name rules MUST be refused before anything is created. forgectl MUST NOT stop or restart an hc core.

#### Scenario: Name hc refuses
- **WHEN** `surface launch --surface hc --name T7` runs
- **THEN** the launch is refused before any worktree, ledger row, or terminal exists

### Requirement: hc terminal ownership
A reference to an hc terminal SHALL record its address, its creation time, and a fingerprint of the hc core's id. Close, probe, read, and brief MUST act only on a terminal whose `forgectl.surface` label, creation time, and core fingerprint all match the reference.

#### Scenario: Address reused
- **WHEN** an hc worker's terminal was removed and a new terminal took its address
- **THEN** `surface close` refuses with an identity mismatch and kills nothing

### Requirement: hc worker verbs
`list`, `read`, `ready`, `wait`, `brief`, and `close` SHALL take `--surface herdr|hc`, defaulting to `herdr`, and read that backend's ledger. Under hc, readiness MUST come from hc's reported status (`idle` or `done` is ready, `blocked` is blocked), never from screen text. `surface tag` MUST refuse an hc worker.

#### Scenario: Blocked hc worker
- **WHEN** an hc worker reports status `blocked`
- **THEN** `surface ready` exits 1 naming the blocked status

### Requirement: hc briefs and close
Under hc, `surface brief` MUST refuse unless the worker's status is `idle` or `done`, MUST NOT pass `--force` to `hc send`, and SHALL record the brief's marker before sending. `surface close` SHALL run `hc kill` and then `hc rm`.

#### Scenario: Brief to a blocked worker
- **WHEN** an hc worker reports status `blocked`
- **THEN** `surface brief` reports `refused` and types nothing

#### Scenario: Close removes the terminal
- **WHEN** `surface close x --surface hc` runs on a live hc worker
- **THEN** its terminal is killed and removed, and `hc get` no longer finds it

### Requirement: Worker follows the launcher's mode
`surface launch --worktree --inherit-mode <mode>` SHALL take the launching session's Claude Code permission mode as a ceiling: no claude worker MUST be looser than it, whatever `[launch.worker]` or the matched project block sets. It MUST NOT raise a worker above the mode its config gives. An unknown mode, or the flag without `--worktree`, MUST be refused.

#### Scenario: Auto coordinator
- **WHEN** a coordinator in auto launches a worker with `--inherit-mode auto` and `[launch.worker] permission_mode = "auto"`
- **THEN** the worker starts in auto

#### Scenario: A claim does not raise
- **WHEN** the launch passes `--inherit-mode auto` and `[launch.worker]` sets no permission mode
- **THEN** the worker starts in the built-in `acceptEdits`

#### Scenario: Plan launcher caps an explicit worker mode
- **WHEN** `[launch.worker] permission_mode = "auto"` and the launch passes `--inherit-mode plan`
- **THEN** the worker starts in plan

#### Scenario: Bypass launcher
- **WHEN** the launch passes `--inherit-mode bypassPermissions` and `[launch.worker] permission_mode = "auto"`
- **THEN** the worker starts in auto, never looser
