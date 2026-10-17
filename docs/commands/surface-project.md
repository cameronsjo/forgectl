# Persistent collaboration projects

`forgectl surface project` keeps logical projects and threads in an explicit private registry. It is separate from `forgectl projects`, which inventories repositories. The first slice only creates and inspects durable context: it does not launch a harness, enroll a team, create a Git worktree or branch, open a herdr workspace, or grant tools.

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

Every JSON response includes `version` and `revision`. Project creation and `show` include a `project` object and its deterministic `ledger` path; that ledger is not created by these commands. Thread creation and `show` include `project_id` and `thread`. `list` responses contain `projects` or `threads` arrays. No record or successful command implies that a native agent is running or that work has completed.

## Persistence and refusals

The registry uses a versioned `registry.json`, a private lock, and atomic replacement under the named root. Creation includes the coordinator thread in the same transaction. Fresh CLI processes read the same project/thread IDs; they do not reconstruct identity from labels, directories, or terminal IDs.

Read commands neither create a missing root nor repair permissions or corrupt data. An absent root lists as `projects: []` at revision `0`; a missing project/thread ID is an error. Malformed or unsupported state, unknown or duplicate JSON fields, unsafe file links, and incorrect owner/private modes fail rather than appearing as an empty list. Keep the original state intact for diagnosis; these commands do not provide a destructive reset or migration.

Exit codes: `0` for completed creation/inspection, `1` for state/storage failures, and `2` for invalid inputs or an unknown requested ID. Parse both the exit status and the JSON shape; do not turn an unreadable registry into an empty result.

## Remaining integration

These records are the foundation, not a complete Projects interface. Run binding, native launch/attachment, herdr sidebar/tab presentation, Desk monitoring, and the live reconnect/replacement checks remain tracked in the [Projects implementation plan](../plans/2026-10-17-projects-surface.md).

The existing [team mailbox](surface-team.md) remains the communication authority. Project context and teammate messages never count as operator approval. Desk's human approval queue remains separate from read-only project monitoring.
