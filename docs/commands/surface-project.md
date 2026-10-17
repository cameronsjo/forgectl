# Persistent collaboration projects

`forgectl surface project` keeps logical projects, threads, and enrolled-run bindings in an explicit private registry. It is separate from `forgectl projects`, which inventories repositories. These commands do not launch a harness, enroll a team, create a Git worktree or branch, open a herdr workspace, or grant tools.

A project has a stable ID, a working directory, optional instructions, and exactly one coordinator thread. Additional threads are worker workstreams. Labels are for people; commands address exact IDs. Two projects may share the same label and working directory without sharing identity or their future team ledger.

## Create and inspect

```sh
forgectl surface project --root /absolute/private/projects \
  create "Mailbox reliability" --worktree /absolute/repository \
  --instructions /absolute/project-instructions.md --json

forgectl surface project --root /absolute/private/projects list --json
forgectl surface project --root /absolute/private/projects show PROJECT_ID --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID create "Receipt review" \
  --brief /absolute/thread-brief.md --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID list --json
forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID show THREAD_ID --json
```

Use this source build's absolute executable path when an older installed `forgectl` is also present. `--root` has no environment or global default. Its parent directory must already exist; the registry root is created owner-private when creating the first project. The working directory must already exist and be absolute. `--instructions` and `--brief` are optional UTF-8 regular files of at most 16 KiB; their contents are snapshots, not live file links. Names are bounded to 128 Unicode characters and cannot contain control characters.

Every JSON response includes `version` and `revision`. Project creation and `show` include a `project` object and its deterministic `ledger` path; that ledger is not created by these commands. Thread creation and `show` include `project_id` and `thread`. `list` responses contain `projects` or `threads` arrays. Project `list` is a logical registry snapshot; project `show` and thread `list`/`show` also read one checked roster snapshot and return separate `observations` or `observation`. No record or successful command implies that a native agent is running or that work has completed.

## Bind an enrolled run

First explicitly enroll a member using [team registration](surface-team.md), with the exact `ledger` returned for this project. Do not share a ledger between projects. Binding then snapshots that existing member; it does not register, replace, or start it.

```sh
forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID bind THREAD_ID \
  --member coordinator --run-id ENROLLED_RUN_ID --expect-run none --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID context THREAD_ID --json
```

`--expect-run` is mandatory: `none` means the thread must still be unbound; otherwise it must equal the saved active run ID. Literal `none` is reserved and cannot be a project run ID. The coordinator thread requires a coordinator roster member, and a worker thread requires a worker member. A worker may use a different Git worktree; project membership comes from the private ledger, not directory equality.

The saved `active_run_id` selects the latest entry in `runs`. Each entry records the member, harness, native session ID when known, worktree, socket, pane ID, and original binding time. Replacing a native process requires a fresh enrolled RunID. After settling/stopping the old process and explicitly replacing its registration, bind the new run with `--expect-run OLD_RUN_ID`. The logical thread ID and old run history remain unchanged. A retired run cannot be reused, including by another thread in the same project. Retiring a binding does not stop a process or recall a request already in flight.

The one same-run update is explicit native-session discovery: repeat `bind` with the same run ID and expected run after the roster fills an initially empty native-session ID. Every other identity field must remain exact. A known native-session ID cannot change within that run. Inspection never performs this refresh automatically.

Enrollment observations are `unbound`, `current`, or `stale`. `current` means only that the saved tuple matched that roster snapshot; `native_session_known` reports an identifier, not proof that its history can be opened. `process_liveness` stays `unknown`. A changed or missing roster tuple makes a binding stale. Malformed, unsupported, duplicate-key, public, or linked roster state is an error, not an empty roster or a successful stale check. These consistency checks do not authenticate against another process running as the same operating-system user, and binding alone does not verify a provider or grant a permission posture.

`thread context` returns a bounded context capsule: project instructions, the thread brief, stable IDs, the current saved run when bound, and exact mailbox references. It contains no native conversation history and performs no delivery, launch, registration, or approval. A stale binding is refused until explicitly resolved. Without `--json`, instructions and brief are sanitized previews; `--json` preserves the complete bounded values while escaping terminal controls. Project context and teammate messages remain task data, never operator approval.

## Persistence and refusals

The registry uses a versioned `registry.json`, a private lock, and atomic replacement under the named root. Creation includes the coordinator thread in the same transaction. Fresh CLI processes read the same project/thread IDs; they do not reconstruct identity from labels, directories, or terminal IDs.

Read commands neither create a missing root nor repair permissions or corrupt data. An absent root lists as `projects: []` at revision `0`; a missing project/thread ID is an error. Malformed or unsupported state, unknown or duplicate JSON fields, unsafe file links, and incorrect owner/private modes fail rather than appearing as an empty list. Keep the original state intact for diagnosis; these commands do not provide a destructive reset or migration.

Exit codes: `0` for completed creation, binding, or inspection; `1` for state/storage failures or a conflicting binding; and `2` for invalid inputs or an unknown requested ID. Parse both the exit status and the JSON shape; do not turn an unreadable registry into an empty result.

## Remaining integration

These records are the foundation, not a complete Projects interface. Native launch/attachment, herdr sidebar/tab presentation, Desk monitoring, and the live reconnect/replacement checks remain tracked in the [Projects implementation plan](../plans/2026-10-17-projects-surface.md).

The existing [team mailbox](surface-team.md) remains the communication authority. Project context and teammate messages never count as operator approval. Desk's human approval queue remains separate from read-only project monitoring.
