# Persistent collaboration projects

`forgectl surface project` keeps logical projects, threads, and enrolled-run bindings in an explicit private registry. It is separate from `forgectl projects`, which inventories repositories. Creation, binding, context, and inspection do not launch a harness, enroll a team, create a Git worktree or branch, or grant tools. Explicit `open` and `thread focus` commands control verified herdr presentation.

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

Registry JSON responses include `version` and `revision`. Project creation and `show` include a `project` object and its deterministic `ledger` path; that ledger is not created by these commands. Thread creation and `show` include `project_id` and `thread`. `list` responses contain `projects` or `threads` arrays. Project `list` is a logical registry snapshot; project `show` and thread `list`/`show` also read one checked roster snapshot and return separate `observations` or `observation`. The timeline has a separate evidence-snapshot shape described below. No record or successful command implies that a native agent is running or that work has completed.

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

## Read-only timeline

```sh
forgectl surface project --root /absolute/private/projects timeline PROJECT_ID --json
forgectl surface project --root /absolute/private/projects \
  timeline PROJECT_ID --thread THREAD_ID --live
```

The timeline reuses Desk's renderer without opening its approval queue. With no `--thread`, it shows all logical thread states and the project's bounded mailbox transport evidence. A thread view includes only messages whose saved sender or recipient run ID matches that thread's binding history. Legacy messages without run IDs remain project-level evidence; names and timestamps never invent a thread attribution.

`waiting`, `transport_accepted`, `delivery_failed`, and `delivery_unknown` describe delivery, not completed model turns or verified artifacts. Explicit native observations and artifact checks appear as separate event categories. Enrollment and native-session identity remain separate from process liveness, which is explicitly `unknown`. Message bodies, delivery diagnostics, native histories, and project instructions do not enter the projection. A completed native turn is not a successful overall task; an artifact digest match is not a correctness or authorship judgment.

JSON includes `version`, `project_id`, optional `thread_id`, and Desk's `view` with sanitized events, unknown liveness, `partial`, `held`, and dropped-record counts. It is not an atomic revision of both registry and mailbox. Malformed, over-cap, or unterminated input remains visible; the command prints the partial report and exits `1`. Reads are bounded to 8 MiB of mailbox data, 1 MiB per record, and 50,000 messages. `--live` requires a terminal and cannot be combined with `--json`. Neither snapshot nor live mode registers, launches, focuses, repairs state, or approves work.

## Observe native turns and check artifact bytes

```sh
forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID observe THREAD_ID --run-id CURRENT_RUN_ID --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID verify-artifact THREAD_ID --run-id CURRENT_RUN_ID \
  --path result.txt --sha256 EXPECTED_LOWERCASE_SHA256 --json
```

`observe` queries the exact current bound native session without sending a prompt, resuming history, or launching a process. Codex uses its private Unix app-server endpoint and metadata-only thread/turn reads. Pi uses its enrolled inbox extension; Hermes uses its enrolled supervisor. Other harnesses explicitly refuse this capability. A missing native-session ID, stale run, wrong native identity, or binding change during the query records nothing.

Current provider/model metadata is returned separately from a terminal-turn result. A selected model does not establish which model produced a historical turn. Metadata alone never adds durable completion evidence. Native terminal outcomes are `completed`, `failed`, `incomplete`, or `unknown`, bound to the original project/thread/run/native-session/turn identities. Pi uses a persisted native entry ID, not its resettable turn counter. An in-progress turn is not completion evidence. Process liveness remains unknown even when the query succeeds.

`verify-artifact` hashes a regular file beneath that run's bound worktree, then rechecks the exact enrollment before recording its relative path, size, and matching digest. It rejects traversal, outside paths, symlinks, hardlinks, oversized files, a changed path/file identity, and mismatched hashes. The check is capped at 32 MiB. This proves byte identity at verification time only; correctness, authorship, and continued freshness require separate checks. No native model result is inferred from the file.

The registry retains at most 256 evidence records per project. Exact repeated evidence is a no-op; a conflicting result for the same native turn or artifact path within the same run is refused. Replacement retains old evidence without attributing it to the new run. Timeline events expose exact evidence IDs and `currently_enrolled`, distinguishing historical observations from the current enrollment. The timeline itself never polls native endpoints or performs artifact checks.

## Explicit herdr presentation

```sh
forgectl surface project --root /absolute/private/projects \
  open PROJECT_ID --session projects-test --herdr-binary /absolute/herdr --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID open THREAD_ID --view timeline \
  --session projects-test --herdr-binary /absolute/herdr --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID open THREAD_ID --view native \
  --session projects-test --herdr-binary /absolute/herdr \
  --codex-binary /absolute/codex --expect-provider openai \
  --expect-model EXACT_EXISTING_MODEL --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID focus THREAD_ID \
  --session projects-test --herdr-binary /absolute/herdr --json

forgectl surface project --root /absolute/private/projects \
  thread --project PROJECT_ID close-view THREAD_ID --operation OPERATION_ID \
  --session projects-test --herdr-binary /absolute/herdr --json
```

`open` creates a project-owned workspace whose initial tab runs this executable's read-only live timeline, then focuses it. It records a `pending` operation before creation and the complete verified reference afterward. Reopening focuses that saved workspace; it does not infer ownership from a name or working directory, restart a server, or duplicate a missing/stale workspace. Both the session and absolute herdr binary are explicit; ambient session defaults are not used.

References bind generated ownership markers to the server incarnation, endpoint, workspace, tab, pane, and stable terminal. Human-readable label prefixes are not ownership. Changed or duplicate markers, moved/replaced terminals, or server turnover are refused. An ambiguous creation remains `unknown` rather than allowing an automatic retry. Inspect `show --json` for `surface_operations`; do not delete the registry or manually alter phases to force a retry. Automatic recovery/retirement of unresolved operations is not provided by this slice.

After successful creation or focus, the bridge reapplies bounded human-readable project/thread names to herdr's workspace/tab labels as `Human name · fc-surface-<tag>`. The generated marker remains an exact, independently checked suffix; bare legacy markers remain valid. Missing, malformed, changed, or duplicated markers are refused even when human prefixes differ. The workspace's `forgectl_project` token and root-pane display names remain cosmetic metadata for optional sidebar grouping, not ownership evidence. Display names are sanitized and capped at 80 characters; the registry keeps the full name. Ownership syntax is reserved in the display prefix. Cosmetic failure produces a `warnings` entry, not permission to create another workspace/tab. These commands do not edit global herdr configuration.

`thread open --view timeline` explicitly creates a read-only thread timeline tab inside the saved project workspace. It requires current enrollment, journals intent before the tab mutation, and keeps the logical thread and native run separate from the view's operation ID. Repeating the command focuses its existing verified view. Unsupported views fail rather than silently choosing a different native interface or starting another history writer.

`thread open --view native` starts a Codex remote client for an already enrolled persistent session on its private app-server socket. Codex first checks the expected provider/model without mutation, then prepares the exact session with a native resume request containing no overrides. This preparation is not read-only. Only the supported OpenAI provider, user-reviewed `never`/`on-request` approvals, and network-disabled read-only/workspace-write policies are accepted; custom permission profiles and unknown policies are refused. The client receives the observed policy, working directory, and exact workspace/write roots through a typed launch request, not arbitrary harness arguments. It uses the existing private surface handshake and binary-provenance checks with a fresh credential-free client home, never a copied auth file. This connects a UI to an existing session; it does not create a coding worker or change generic worker-policy checks. Provider metadata identifies the selected provider; it does not independently attest the server's network route.

Successful native creation proves authenticated client execution started in the recorded tab, not that attachment finished or that the native policy remains unchanged. The report states that limitation explicitly. Reopening only focuses the existing client; it does not resume again. Pi RPC and Hermes ACP enrollments currently require explicit timeline views because a safe concurrent native client has not been established. Closing a remote-client tab does not stop the enrolled app server or remove its native history. The isolated client state remains in its private temporary directory.

Codex's native client checks project trust on the remote app server. If that server has no explicit trust decision, the native trust prompt remains visible for the operator; the bridge does not approve it or change trust. An explicitly untrusted server-side workspace keeps project-local configuration disabled and does not require granting trust merely to attach.

`thread close-view` closes only the exact saved presentation operation, never the project workspace or enrolled native server. It may retire an old run's view after enrollment replacement, but cannot focus that stale run. Closure has its own `pending`/`closed`/`already_gone`/`unknown` evidence and preserves the original creation reference. Proven closure permits reopening a view of the same current RunID; closing a UI is not native process replacement. An unresolved close blocks another view. Legacy operations without a recorded presentation kind cannot be closed automatically. These are surface-identity checks, not native-process termination evidence.

Presentation reports include the registry revision, exact IDs, `focused`, and any operation/workspace identity. `verified` describes the saved surface, not proof that its viewer or a native harness is still running. If creation is recorded but focus fails, the command reports `focused: false` and exits `1`; a later `open` attempts only focus. Thread focus requires a current enrollment and a verified, non-retired tab for that exact active run. It never silently creates a tab, resumes native history, or launches another writer.

## Persistence and refusals

The registry uses a versioned `registry.json`, a private lock, and atomic replacement under the named root. Creation includes the coordinator thread in the same transaction. Fresh CLI processes read the same project/thread IDs; they do not reconstruct identity from labels, directories, or terminal IDs.

Read commands neither create a missing root nor repair permissions or corrupt data. An absent root lists as `projects: []` at revision `0`; a missing project/thread ID is an error. Malformed or unsupported state, unknown or duplicate JSON fields, unsafe file links, and incorrect owner/private modes fail rather than appearing as an empty list. Keep the original state intact for diagnosis; these commands do not provide a destructive reset or migration.

Exit codes: `0` for completed creation, binding, or inspection; `1` for state/storage failures or a conflicting binding; and `2` for invalid inputs or an unknown requested ID. Parse both the exit status and the JSON shape; do not turn an unreadable registry into an empty result.

## Remaining integration

These commands remain a source-build integration, not a global installation. Native Codex attachment, UI reconnect, receiver restart, and coordinator replacement have live receipts in the [Projects implementation plan](../plans/2026-10-17-projects-surface.md). Pi RPC and Hermes ACP use read-only thread timelines rather than concurrent native interfaces.

Hermes shell-based replies remain subject to its native command policy. A full accepted run does not guarantee every message payload can pass that policy: a subsequent source-code reply was denied and correctly produced no success receipt. The denial is not bypassed. A message-only native channel remains a pending design decision, not an implemented capability.

The existing [team mailbox](surface-team.md) remains the communication authority. Project context and teammate messages never count as operator approval. Desk's human approval queue remains separate from read-only project monitoring.
