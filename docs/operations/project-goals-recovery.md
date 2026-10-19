# Recovery runbook: repository goals

This is a read-first, non-destructive procedure for a retained
`forgectl projects goal` run. Do not replay work, kill a process from a stored
PID, delete a run directory, or weaken its isolation boundary.

## 1. Identify the retained run

Use the same absolute executable and registry root that started the goal. The
registry root is owner-private; do not substitute an ambient global install:

```sh
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  list --json
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  status GOAL_ID --json
```

The list response is:

```json
{"version":1,"states":[{"id":"goal-...","phase":"..."}]}
```

Status returns the version-1 state object. Preserve its `phase`, `summary`,
`run_dir`, `worktree`, `branch`, `supervisor_active`, `cleanup_complete`,
`cancel_requested`, `question`, `questions`, `checks`, `result`, and `error`
fields. A stale heartbeat on a nonterminal goal is reported as
`phase: "unknown"`; terminal phases, including `canceled`, are not rewritten.
That is not proof that the supervisor or native descendants are gone, and the
read does not restart or replay anything.

The retained run directory is `run_dir`, normally
`/path/to/private/goal-registry/goals/<goal-id>`. It can contain `goal.json`,
`state.json`, `runtime.json`, `supervisor.log`, `events.jsonl`,
`source-before.json`, `result.patch`, `check-01.txt` and later check outputs,
`agents/`, and `native-control/`. The linked implementation worktree is the
separate `worktree` path, normally a sibling
`/path/to/private/goal-registry-worktrees/<goal-id>`. Early failure or
cancellation may occur before any particular artifact is created.

## 2. Answer or cancel only a responsive run

If status is `needs_operator`, copy the exact unanswered `question.id` and
provide a bounded literal answer. The control receipt shape is:

```json
{
  "version": 1,
  "goal_id": "goal-...",
  "sequence": 1,
  "action": "respond",
  "question_id": "question-...",
  "answer": "...",
  "created_at": "..."
}
```

```sh
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  respond GOAL_ID --question QUESTION_ID \
  --answer-file /path/to/answer.txt --json
```

`--answer-file` must be a bounded regular non-symlink file of at most 8 KiB;
use `--answer-file -` for literal stdin. An answer can clarify the existing
goal but cannot expand its allowed paths, tools, providers, or isolation.

To request cancellation:

```sh
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  cancel GOAL_ID --json
```

The cancellation receipt has the same shape with `action: "cancel"` and no
question or answer. Both commands require an active responsive supervisor;
terminal or `unknown` goals are refused. A queued cancellation is not
completion. Confirm it with `status --json` only when `phase` is
`"canceled"` and `cleanup_complete` is `true`.
A clean cancellation may still retain the exact state `error` value
`"context canceled"`; `phase: "canceled"` with `cleanup_complete: true` is the
cleanup boundary, not an instruction to erase that diagnostic.

In the TUI, `q` or Ctrl-C detaches from the screen; it does not cancel the
supervisor. Cancellation is a separate confirmed action. Detaching is safe
only as a display operation: the run continues in the background.

## 3. Read retained artifacts

For `patch` and `check`, the artifact command verifies recorded SHA-256 bytes
before returning them. `json` serializes retained state and has no separate
artifact-blob digest. Use the exact flags; `--index` is zero-based:

```sh
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  artifact GOAL_ID --kind json --json
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  artifact GOAL_ID --kind patch
/path/to/forgectl/forgectl projects goal --root /path/to/private/goal-registry \
  artifact GOAL_ID --kind check --index 0 --json
```

The JSON wrapper is:

```json
{"version":1,"kind":"patch|check|json","goal_id":"goal-...","content":"..."}
```

`patch` reads `result.patch`; `check --index 0` reads `check-01.txt` and
`--index 1` reads `check-02.txt`; `json` serializes the retained state.
Missing artifacts are expected when the run stopped before that stage. A patch
or check digest mismatch is evidence tampering or corruption, not a reason to
copy or regenerate the artifact.

For a result, inspect `result.patch_sha256`, `changed_files`,
`review_passed`, `checks_passed`, `source_unchanged`, and `human_accepted`.
For each check, inspect its recorded `argv`, `exit_code`, `passed`, output
digest, `patch_before`, and `patch_after`. A passing check must have exit code
zero and leave the reviewed patch unchanged.

Audit the source checkout and linked worktree separately with read-only Git
commands:

```sh
git -C /path/to/repository status --short --untracked-files=all
git -C /path/to/repository rev-parse HEAD
git -C /path/to/linked-worktree status --short --untracked-files=all
git -C /path/to/linked-worktree diff --check
```

Compare the source HEAD to the approved `spec.base_revision`, and compare
`source-before.json` and status evidence before treating
`source_unchanged: true` as meaningful. Do not assume that a clean linked
worktree proves that the original checkout was untouched.

## 4. Diagnose startup and cleanup boundaries

Separate metadata failure from runtime-helper failure:

- **Prepare failure** is metadata-only. With the exact approved socket, inspect
  Docker Engine API 1.51, Linux OS/architecture, cgroup v2, memory/PID limits,
  built-in seccomp, and the exact immutable image ID, architecture, volumes,
  and allowed environment defaults. Use the explicit socket, never a tag or
  ambient Docker context:

  ```sh
  docker --host unix:///path/to/docker.sock \
    image inspect IMAGE_ID
  ```

- **Start helper failure** occurs after metadata checks. Start invokes the fixed
  Linux `/usr/local/bin/forgectl` goal-container helper and then proves the
  role's mount/UID boundary as a non-root caller before native model work. A
  helper, mount, runtime-cwd, `.git`, or non-root/policy proof failure is not
  fixed by inspecting the image tag or falling back to host commands.

Every mounted worktree, role-runtime, Git metadata, and result-patch path used
by container mode must be shared with the Linux VM. The registry/control
directory, broker sockets, and Docker socket are not mounted into the
container. Host `stat` is not sufficient. Container mode uses Linux binaries
and argv semantics; the helper image supplies only Linux `forgectl`, Git,
Python 3, and a POSIX shell. Checks needing another toolchain require a
separately trusted image build. The host toolchain and host-command fallback
are not available. Each tool or check operation is bounded by 120 seconds or
the remaining goal deadline, whichever is sooner; networking is disabled, so
approved check argv must be compatible with the Linux helper image and work
without network access.

### Process-group receipts

In the retained run directory, legacy goals without an explicit `team` keep
one control directory per native Codex role at `native-control/<role>/`.
Explicit native teams in process-group mode also keep an execution-host
directory at `native-control/<role>-execution/`. Do not require that extra
directory for a legacy Codex-trio run. Each native control directory that the
selected mode creates requires this pair for a clean shutdown:

- `guardian-startup.json` with `status: "started"`, valid `native_pid`,
  `process_group`, `socket`, and `started_at`.
- `guardian-receipt.json` with `status: "stopped"`, the same process identity,
  `socket_gone: true`, `group_gone: true`, and no `error`.

Missing, mismatched, `failed`, or `unknown` receipts are not clean-shutdown
evidence. `socket_gone` proves only that the socket path was removed; the group
receipt is the process-cleanup evidence. A stored PID or process-group ID is
historical evidence only: IDs can be reused, so never kill a later process
from a receipt or state file.

### Container execution receipts

Container mode keeps one receipt sequence per short-lived tool/check operation
under `native-control/<role>-execution/`, rather than the native guardian pair.
A final `removed` receipt must match the approved immutable image and full
container ID, report a stopped container with PID zero, and confirm removal.
`unknown`, missing, mismatched, or unpaired receipts mean cleanup is
unconfirmed. Do not recreate or replay that operation; preserve the receipts
for operator investigation.

The scope is deliberate: native **MODEL** processes remain outside the Linux
container and get process-group cleanup only. **TOOL** and check operations get
fresh containers whose Linux PID namespace contains all their descendants.
That stronger tool boundary does not cover daemonized native-model descendants.

## 5. Unknown or missing evidence

If `status` reports `unknown`, if `supervisor_active` is false without a clean
terminal state, or if a receipt is missing, stop automated recovery. Do not
start the same goal again, resend an answer, issue a second cancellation, or
invent a resume/retry command: this CLI provides none. Do not kill stored PIDs,
remove containers by guessed names, delete sockets, erase `state.json`, or
remove `native-control/` to make the state look clean. `unknown` means the
operation or cleanup may have happened without proof of its final state.

Retain the run directory, worktree, logs, source snapshot, result patch, check
outputs, and receipts. Use only read-only `list`, `status`, and `artifact`
commands while collecting evidence. Escalate unresolved live-process or
container identity to an operator who can inspect the current host/runtime
using the receipt's full owned identity; the runbook does not prescribe a
blind kill or a weaker isolation mode.

## 6. Retention and redaction

Goal state remains after `ready`, failure, and confirmed cancellation. Any
branch, linked worktree, receipt, patch, or check output already created is
retained; an early stop may have fewer artifacts. There is no cleanup or
delete command in this lifecycle. Do not remove retained state as part of
recovery, and do not assume a successful cleanup receipt makes the evidence
disposable.

When sharing diagnostics, report only the goal ID, phase, summary/error
category, relevant digest, receipt phase/booleans, and command exit status.
Redact private repository, registry, runtime, socket, and worktree paths;
native model prompts and responses; usernames; access tokens; auth headers;
environment values; and any secret-bearing stderr. Never paste `goal.json`,
`runtime.json`, native transcripts, or full logs without reviewing and
redacting them first. Keep exact paths, IDs, and receipts in the private
retained run for the operator who owns it.
