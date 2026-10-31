## ADDED Requirements

### Requirement: hc brief re-check at send
`surface brief --surface hc` SHALL hand the status and turn it checked to the send, and the send MUST re-read the worker and type nothing unless the worker is still forgectl's, live, at the checked turn, and `idle` or `done`. The send SHALL pass `--if idle` when the checked or the re-read status is `idle`.

#### Scenario: Turn started after the check
- **WHEN** a worker checked `done` at turn 3 reports turn 4 at the send
- **THEN** nothing is typed and the brief reports `refused`

#### Scenario: Still done
- **WHEN** a worker checked `done` still reports `done` at the checked turn
- **THEN** the brief is sent without `--if idle`

### Requirement: hc brief Escape spacing
An hc brief SHALL wait until one second has passed since the worker's last typed brief was recorded before its checks and Escape. When the Escape's delivery is unknown, or the Escape went out and the ledger record failed, the refusal SHALL return no sooner than one second after it.

#### Scenario: Retry right after a brief
- **WHEN** a brief follows a typed brief recorded 300 ms earlier
- **THEN** it waits 700 ms before reading the worker

### Requirement: hc ledger-only forget
`surface forget <name> --surface hc` SHALL remove the worker's ledger row, and nothing else, only when the recorded address on the current core holds no terminal with the recorded creation time. When the core id differs from the launch's, the row's core selection digest MUST also equal the current one. It MUST NOT kill, remove, or type into any terminal, and MUST NOT run git. Any other `--surface` is a usage error.

#### Scenario: Wiped core
- **WHEN** the core under the worker's session reports a new id
- **THEN** the row is removed, the worktree and branch are left, and the exit code is 0

#### Scenario: Worker still there
- **WHEN** the recorded address holds a terminal with the recorded creation time, whatever its label
- **THEN** the row is unchanged, the reason names `surface close`, and the exit code is 1

#### Scenario: New core id, worker still there
- **WHEN** the core id differs under the launch's selection and the recorded address still holds a terminal with the recorded creation time
- **THEN** the row is unchanged and the reason names `hc kill`

#### Scenario: Another core selection
- **WHEN** the core id differs and the row records no core selection digest, or one that differs from the current environment's
- **THEN** the row is unchanged, the reason names `surface close`, and the exit code is 1

### Requirement: hc forget refusals
`surface forget` MUST refuse, changing nothing, when hc cannot be read, when the reference names another `HC_SESSION` selection, for a row with no recorded terminal, at stage `closed`, in a launch less than ten minutes old, and when the row changed since it was read.

#### Scenario: hc unreadable
- **WHEN** `hc server status` fails
- **THEN** the row is unchanged and the exit code is 1

### Requirement: Worker posture in the ledger
A worker launch SHALL record its resolved posture in the ledger row before the worker starts: each field its harness takes, its value, and its source (`default`, `launch.worker`, `repo-profile`, or `launcher`). `surface list --json` SHALL print it as `posture`. A row without it SHALL still read.

#### Scenario: Launcher mode caps the worker
- **WHEN** `--inherit-mode plan` caps a claude worker at `plan`
- **THEN** the row's posture has `permission_mode` `plan` with source `launcher`

### Requirement: hc worker in the herdr-app window
`surface launch --worktree --surface hc --herdr-space <id>` SHALL label the worker's terminal `herdr.space=<id>` at `hc run`. Without the flag no such label is set. An id outside 1-64 letters, digits, `-` and `_`, the flag on a herdr worker, and the flag without `--worktree` MUST be refused as usage errors before anything is created.

#### Scenario: Shown in a space
- **WHEN** an hc worker is launched with `--herdr-space sp1`
- **THEN** `hc run` carries `--label herdr.space=sp1`

#### Scenario: Herdr worker
- **WHEN** `--herdr-space` is given with `--surface herdr`
- **THEN** the launch is refused with exit 2
