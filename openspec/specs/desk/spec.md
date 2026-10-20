# desk Specification

## Purpose
The desk is an operator queue for scripts and batch manifests an agent stages but will not run itself: the agent queues an item with a fixed sha256, and a person approves exactly those bytes to run once.

## Requirements

### Requirement: Unix only
Every `forgectl desk` verb SHALL run only on a Unix system. On any other system each verb MUST exit 2 with a message saying the desk is not supported on this OS.

#### Scenario: Non-Unix host
- **WHEN** any `forgectl desk` verb runs on a non-Unix system
- **THEN** it exits 2
- **AND** stderr says `forgectl desk is not supported on this OS`

### Requirement: Queue an item with a fixed hash
`forgectl desk add <file|->` SHALL require non-empty one-line `--what` and `--why`, insert `# WHAT:` and `# WHY:` (and `# TTY: yes` with `--tty`) header lines, then fix the item's sha256 and print `name=`, `kind=` and `sha256=` lines (or `{name, kind, sha256, path, warnings, duplicate, signal}` with `--json`). The kind MUST come from the extension: `.sh` is a script, `.manifest` is a batch.

#### Scenario: Queue a script
- **WHEN** an agent runs `forgectl desk add ./fix.sh --what "Merge PR 1201" --why "You own merges"`
- **THEN** the item is written to `pending/` under the next free `NN-` number
- **AND** stdout carries its `name=`, `kind=script` and the full `sha256=`
- **AND** the exit code is 0

#### Scenario: Unsafe WHAT text
- **WHEN** `--what` or `--why` is missing, empty, or holds a control or bidi character
- **THEN** `add` exits 2 and queues nothing

#### Scenario: Unusable input file
- **WHEN** `<file>` is a FIFO or device, a manifest that cannot run, or no item number is free
- **THEN** `add` exits 1 without reading a FIFO or device and queues nothing

### Requirement: Retry-safe add
`desk add` SHALL be safe to retry: when an item of the same kind with the same sha256 is already waiting in `pending/`, it MUST queue nothing, print that item with `duplicate=true` (`"duplicate": true` in JSON) and exit 0, unless `--allow-duplicate` is given. Only a waiting item counts as a duplicate.

#### Scenario: Same file added twice
- **WHEN** the same file is added twice with the same `--what` and `--why` while the first is still waiting
- **THEN** the second call queues nothing, reports the first item with `duplicate=true`, and exits 0

### Requirement: Only unchanged bytes run
An item's sha256 SHALL be fixed once, at `add` or the first time a desk sees a hand-dropped item. On approval the desk MUST read the bytes once, check the full hash (never a prefix), and run exactly those verified bytes. A pending item whose bytes changed MUST be moved to `skipped/` with `skip_reason: changed` and never run or re-armed.

#### Scenario: Script edited after queueing
- **WHEN** a pending item's file is modified after it was queued
- **THEN** the desk does not run it and moves it to `skipped/` as `changed`
- **AND** the record keeps the queued hash and the hash it found

### Requirement: Approval acts only on what is on screen
The dashboard SHALL show the selected item's first 12 sha256 hex characters with its WHAT and WHY, and `y` MUST run nothing while the window is too small to show them. `a` MUST ask first, list every item it will run with its full sha256, and run only after every page of that list has been shown. TTY and changed items MUST be excluded from `a`.

#### Scenario: Rescan changes the selection
- **WHEN** a rescan, not a keypress, puts a different waiting item under the cursor
- **THEN** the next `y` refuses once instead of running the new item

### Requirement: Run at most once
A run SHALL start by renaming the item from `pending/` into `running/`, so two desks on one directory cannot both run it. An item whose number already has files in `done/` MUST be moved to `skipped/` as `name-reused` without writing over the earlier run's record.

#### Scenario: Two desks claim one item
- **WHEN** two dashboards on the same desk directory approve the same item at once
- **THEN** exactly one runs it and the other does nothing

### Requirement: Safe file handling
Pending items SHALL be opened without following symlinks, and anything other than a regular file with one link MUST be refused. Desk readers MUST refuse non-regular files without blocking on a FIFO. The desk directory and its `pending/`, `running/`, `done/`, `skipped/` subdirectories MUST be created `0700` with files `0600`.

#### Scenario: Symlink planted in pending
- **WHEN** a symlink or a hard-linked file appears in `pending/`
- **THEN** the desk refuses it and does not run it

### Requirement: Terminal-safe rendering
All item names, WHAT and WHY lines, script lines, log tails, event lines and notification text SHALL pass through the terminal-safe filter before display, so an item cannot move the cursor, set the window title or hide text. A meta `sha256` that is not 64 lowercase hex characters MUST make the item `refused` and MUST never be printed.

#### Scenario: Malformed meta hash
- **WHEN** an item's meta file holds a `sha256` that is not 64 lowercase hex characters
- **THEN** `desk status` shows the item as `refused` and does not print that value

### Requirement: Run environment
Every item, including TTY items and batch steps, SHALL start in the user's home directory, and its environment MUST omit bash startup hooks (`BASH_ENV`, `SHELLOPTS`, `BASHOPTS`, `CDPATH`, `GLOBIGNORE`, `PS4`, `POSIXLY_CORRECT`, `BASH_COMPAT`, `EXECIGNORE`, `TMOUT`, `BASH_XTRACEFD`, `FUNCNEST`, `GLOBSORT`, and every `BASH_FUNC_*`). Detached items run with stdin from `/dev/null` and survive the dashboard quitting.

#### Scenario: Desk started from a project directory
- **WHEN** an item runs from a dashboard started in `/some/project` with `BASH_ENV` set
- **THEN** the item's working directory is the home directory
- **AND** `BASH_ENV` is not in its environment

### Requirement: Read-only inspection verbs
`status`, `runs`, `show`, `watch`, `plan` and `--frame` SHALL open the desk read-only: nothing is created, moved, chmodded or written. A directory holding none of the four protocol subdirectories MUST NOT be turned into a desk; `status` prints `note: desk not found at <path>` on stderr and the empty queue (`found: false` in JSON) with exit 0.

#### Scenario: Typo in DESK_DIR
- **WHEN** `forgectl desk status` runs with `DESK_DIR` naming an empty directory
- **THEN** it prints the not-found note and an empty queue and exits 0
- **AND** no subdirectory is created

### Requirement: Item states
`desk status` SHALL report each item as one of `waiting`, `held`, `refused`, `running`, `lost`, `done` or `skipped`. A waiting item older than 24 hours MUST be flagged `stale`. A running item whose owner is gone with no `RUN-END`, or a claim with no recorded owner within 60 seconds, MUST show as `lost`. An item whose `# AFTER:` dependencies have not all finished with exit 0 MUST be `held` and cannot be claimed.

#### Scenario: Dependency not yet finished
- **WHEN** an item was added with `--after 03-enable` and `03-enable` has not finished with exit 0
- **THEN** status shows it `held` with the reason
- **AND** approving it is refused

### Requirement: Watch a run for agents
`forgectl desk watch <name>` SHALL print the item's event lines as they arrive and exit 0 on `RUN-END rc=0`; 1 on another rc, `RUN-LOST`, a skipped item or no such item; 2 on usage; 75 when `--deadline` passes first; 130 on SIGINT or SIGTERM; and 141 when stdout closes. On 75 and 130 the last line MUST be `resume=forgectl desk watch NAME --skip N ...`. With no `--deadline` and no terminal on stdin it MUST stop after 540 seconds.

#### Scenario: Deadline reached while pending
- **WHEN** an agent runs `forgectl desk watch 17-fix --deadline 60` and nobody approves the item within 60 seconds
- **THEN** it exits 75
- **AND** the last line is a `resume=` command with `--skip N` naming the lines already printed

### Requirement: Skip and clear lost runs
`forgectl desk skip <name> --reason TEXT` SHALL move a waiting item to `skipped/` (`reason=operator`) or a lost run out of `running/` (`reason=lost`), recording the text as `skip_note` with `skipped_by: cli`. A live run MUST be refused with exit 1. Skipping an already skipped item MUST change nothing, report `already=true`, and exit 0. A reason that is empty, over 200 characters, or holds control or bidi characters MUST exit 2.

#### Scenario: Retry a skip
- **WHEN** `desk skip 17-fix --reason "superseded"` runs on an item already in `skipped/`
- **THEN** it prints the originally recorded reason and note with `already=true`
- **AND** exits 0 without changing the item

### Requirement: Nothing is deleted on its own
`forgectl desk prune` SHALL be the only command that deletes an item's files, and only protocol files of `done/` and `skipped/` items older than `--days` (default 30). It MUST never touch `pending/`, `running/`, unknown files, symlinks or the desk root, and `--dry-run` MUST delete nothing. `--days` below 1 MUST exit 2; a desk that is a symlink or another user's MUST be refused with exit 1.

#### Scenario: Dry-run prune
- **WHEN** `forgectl desk prune --days 30 --dry-run` runs
- **THEN** it prints `would_prune=<n>` and one line per item a real prune would remove
- **AND** deletes nothing

### Requirement: Batch manifests
A `.manifest` item SHALL hold one step per line as `<step> [after=a,b] [timeout=S] [private] -- <command>`, run in dependency waves at most 4 at a time. A timed-out step MUST receive SIGTERM to its process group, SIGKILL 5 seconds later, and rc 124. A failed step MUST skip its dependents. A `private` step's output MUST be kept out of the combined log, and its `STEP_OUT` values of 6 or more characters replaced in later output with `<redacted:OUT_<step>_<key>>`.

#### Scenario: Step fails
- **WHEN** step `build` exits non-zero in a batch where `stage` has `after=build`
- **THEN** `stage` is skipped
- **AND** the batch ends with rc 1

### Requirement: Operator signal on add
When an item is queued, `add` SHALL notify the operator inside a herdr pane (a notification plus the pane's blocked state) and on macOS (a desktop notification), each switchable off under `[desk]` `notify_herdr` and `notify_macos`. A signal that fails MUST be reported as a `warning: operator signal failed:` line and MUST NOT fail the add.

#### Scenario: herdr notification fails
- **WHEN** `desk add` queues an item and the herdr notification call fails
- **THEN** the item is still queued and the exit code is 0
- **AND** stderr carries a `warning: operator signal failed:` line naming the setting that turns it off
