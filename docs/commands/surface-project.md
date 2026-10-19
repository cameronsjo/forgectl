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

## Agents activity overview

```sh
forgectl surface project --root /absolute/private/projects agents PROJECT_ID --json
forgectl surface project --root /absolute/private/projects agents PROJECT_ID --live
forgectl surface project --root /absolute/private/projects \
  agents PROJECT_ID --live --herdr-session projects-test \
  --herdr-binary /absolute/herdr
```

The Agents overview has one row per logical thread, harness/current-model subtitles, and native activity intervals on a shared rolling time axis. It runs inside a terminal tab, preserving herdr's existing sidebar and tabs. Press `?` for keyboard help in normal or narrow terminals; press `?` again or Escape to dismiss it. The help lists all controls: Up/Down or `k`/`j` select a thread, Enter focuses it, `e` opens its evidence details, `+`/`=` and `-`/`_` change the visible window from 5 to 60 minutes, and `q` or Ctrl-C exits. Narrow terminals retain the identity/status list. `--no-icons` selects ASCII markers.

Unbound threads show their coordinator/worker role and `Not connected`, rather than missing-model diagnostics. When every thread is unbound, an explanation replaces the empty time axis. Roomy terminals add margins and gaps between rows; short terminals keep a compact layout so spacing does not hide threads. Live status uses plain-language labels such as `Working`, `Activity unknown`, and `current session only`; exact source, reason, and coverage fields remain available in printed/JSON output. Enter or `e` on an unbound thread explains that no session is connected without attempting a surface action.

When both explicit herdr options are provided, Enter focuses the selected thread's existing verified view and `e` opens or focuses its detailed evidence timeline. Actions recheck the selected run and surface identity; they never silently select a replacement run or guess a native resume command. Without those options the live view remains read-only and its focus/open actions are unavailable. `--live` requires a terminal and cannot be combined with `--json`. Printed and JSON output read one snapshot and perform no surface actions.

Bars represent observed native activity, not task correctness or mailbox delivery. Codex supplies bounded native turn history; Pi and Hermes supply intervals observed by the current extension/supervisor process. Their `current-process` coverage and `since` time make unavailable earlier history explicit. Harness/model subtitles show the current selection, not the producer of every old interval; printed/JSON metadata also includes the current provider. Historical model metadata is shown only when captured with that interval. Unsupported or unreachable sources remain unknown. Failed polls freeze open intervals at their last observation rather than extending them as if still active; finished observations remain visible. Old persisted turn evidence without start/end times is a point, never an invented duration. No prompt, generated task summary, or reasoning text is used as a label.

Collection is read-only and rechecks exact enrollment after native inspection. Snapshots are bounded to 64 rows, 512 total intervals/points, and 2 MiB of encoded output, with four concurrent native reads, two seconds per read, and a five-second native-refresh deadline. JSON includes total/dropped counts, source coverage, and explicit partial/reason fields. An incomplete one-shot snapshot is still printed but exits nonzero; inspect its partial fields rather than treating it as complete project coverage.

The existing explicit `thread observe` mutation can persist a settled interval only when its native turn ID and outcome match. Both actual endpoints are required; currently selected model metadata is never substituted for interval-captured metadata. Repeated observations preserve known values and the original observation time; missing metadata can be enriched once, but conflicting times/outcomes/known metadata are refused. The Agents view itself never writes these observations or reconstructs lost process history from mailbox receipts.

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

In Hermes native-reply mode, missing or invalid reply proof retains an `unknown` settled outcome and closed activity interval, even if the raw prompt returned `end_turn`. Denied or unsupported native operations also cannot become completed evidence. A deliberately reply-free acknowledgement may therefore remain unknown; that does not imply its input was undelivered. Mailbox receipt, native prompt termination, validated reply publication, and verified artifact remain separate facts.

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

`open` creates a project-owned workspace whose initial tab runs this executable's Agents overview, then focuses it. It records a `pending` operation before creation and the complete verified reference afterward. Reopening focuses that saved workspace; it does not replace an existing viewer, infer ownership from a name or working directory, restart a server, or duplicate a missing/stale workspace. Both the session and absolute herdr binary are explicit; ambient session defaults are not used. Existing project workspaces keep their original viewer; run `agents --live` directly in an operator-selected terminal to use the new overview without changing them.

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

## Self-service repository goals

`forgectl projects goals` is the compiled, interactive self-service flow for one
bounded change in an existing local Git checkout. Run it from the repository you
want to change in a terminal:

```sh
cd /path/to/repository
forgectl projects goals
```

This is separate from both the persistent project-identity commands above and
the attended pilot below. It creates its own retained project, mailbox, native
sessions, branch and linked worktree; it does not reuse a project merely because
the label or repository path matches.

Start on one form: choose the repository and a **Bug fix**, **Feature**, **Docs**
or **Custom** preset, then describe the goal. Enter inserts a newline in the
goal; Tab and Shift+Tab move between fields and visible buttons. Enter or Space
activates a focused button. There is no Ctrl+J/Ctrl+S step-by-step wizard.

Presets suggest scope, acceptance criteria and verification commands without
replacing your goal text. Bug fix and Feature inspect known local manifests for
Go, Rust or JavaScript test commands; they do not execute detection commands,
run repository scripts or install dependencies during setup. Docs suggests
`docs` and root README paths with a whitespace check. When no test runner is
detected, the form explicitly says that only a whitespace check is suggested.
That is not a claim that behavior tests passed. Custom keeps explicit edits.

**Advanced** expands on the same form for title, allowed paths, acceptance
criteria, checks, model, role assignments and duration. The title otherwise comes from the
goal. Allowed paths are repository-relative literal files or directory prefixes,
not globs; `.` opts in to repository-wide source changes while Git metadata,
agent metadata and `.env` files remain excluded. Enter checks as JSON argument
arrays, for example:

```json
[["/usr/bin/git","diff","--check"]]
```

Each inner array is the exact executable and argv used for verification. There
is no shell parsing, so pipes, redirects, variable expansion and a single command
string do not work. The duration is 1–30 minutes and is not a token or spending
cap. Review the compact approval summary before Start: the actual repository,
exact base revision, source-dirty warning, model, scope, acceptance criteria and
exact commands. Preparation does not start a model. Start is a separate,
explicit confirmation; changing a field or preset invalidates the prepared
approval instead of silently launching the old values.

**Team** is separate from the task preset. **Codex trio** retains the original
all-Codex workflow. **Mixed native team** assigns Codex to coordination, Pi to
implementation and Claude Code to independent review. **Custom team** exposes
each role in Advanced as `harness,provider,model`; complete values also appear in
approval. Changing a role, model, provider or team invalidates preparation.

Each native role has its own credential-free execution host. Implementation
commands start in the isolated worktree; coordinator and reviewer commands start
in their private runtime directories and can inspect source using absolute paths.
File-tool paths remain worktree-relative. The supervisor separately grants the
reviewer execution host the exact approved check scope after frozen-patch review;
model tool calls never receive that wider scope. Implementation tools, its native
model process and its execution host stop before the patch is frozen.
Each dedicated guardian sets an owner-private file-creation mask before starting
its native child, including lazily created Pi transcripts; the caller's mask and
existing retained files are not changed.

Codex models that require code-mode tool calling use the installed native
code-mode host as a JavaScript dispatcher for those same five goal tools. It
does not expose Node, filesystem/network APIs or native shell tools. Disabling
the dispatcher is not a substitute for disabling I/O tools: a code-mode-only
model cannot call the approved tools without it.

Native teams remain experimental. Inspect retained review, checks and cleanup
evidence before accepting a result. With the default process-group isolation,
they use the existing Codex filesystem/network sandbox and owned-process-group
cleanup. Preparation explicitly warns that daemonized children may survive
cancellation; this is not all-descendant VM/container isolation. An unavailable
tool sandbox refuses the run before model work; do not disable isolation, change
global permissions or substitute another provider to bypass it. The legacy
Codex trio remains a separate execution path.

### Optional Linux container tool isolation

The `isolation` field is an opt-in boundary for tool and verification execution;
it does not replace native model-process cleanup. Select **Linux container** in
the setup form's Isolation field, then expand **Advanced** and provide the
exact **Container socket** and **Container image** values. Container mode
requires an explicit native team, even when every role is Codex, so no legacy
tool path can bypass the boundary. The default **Process-group cleanup** choice
preserves old goal specs and their JSON bytes.

The JSON shape is:

```json
"isolation": {
  "mode": "container",
  "socket": "/path/to/docker.sock",
  "image": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
```

The socket must be an absolute, clean local Unix socket path of at most 103
bytes. The image must be an existing local Linux image identified by its exact
64-character lowercase SHA-256 ID; replace the example digest with the value
from the trusted local engine. The engine must already be a trusted local Linux
Docker Engine exposing API v1.51 with cgroup v2, memory/PID limits and built-in
seccomp. Preparation inspects the engine, image architecture and image defaults
before reading source HEAD; it does not execute the helper or prove mount/UID
access. Start creates each role's execution host, runs the fixed
`/usr/local/bin/forgectl` helper and performs the mount/UID boundary probe before
starting that role's native model. A failed helper or probe refuses model work.
The flow never pulls or builds an image, starts a VM or daemon, or falls back to
host commands.

The repository includes a minimal helper image recipe. From its checkout, an
operator may build a local candidate and inspect its immutable ID:

```sh
docker build -f Dockerfile.goal-tools -t forgectl-goal-tools:local .
docker image inspect --format '{{.Id}}' forgectl-goal-tools:local
```

The recipe supplies Linux `forgectl`, Git, Python 3 and a POSIX shell. Checks
that need another toolchain require a separately trusted image build; the host
toolchain is never substituted. On macOS with Colima, every absolute worktree,
role-runtime and Git path must be in a directory shared with the Linux VM. Do
not assume `/tmp` (often `/private/tmp`) is shared; a host `stat` succeeding is
not enough, because the Start-time helper and boundary probe must prove the
engine can read and write the approved paths as the requested non-root UID/GID.

Each tool, file helper and approved check gets one fresh leased container. The
container preserves approved host absolute paths so Linux argv and Git worktree
semantics remain meaningful. It mounts only the role's worktree (read-only
unless that operation is explicitly writable), its private runtime directory,
the exact Git directories and worktree `.git` entry read-only, and the separate
`result.patch` read-only when present. The source checkout, credentials,
control/receipt directory, broker sockets and Docker socket are not mounted.
Mounts are private and non-recursive; overlapping, symlinked or unsupported
paths refuse the operation.

The effective policy is Linux-only, no network, read-only root filesystem, a
bounded private `/tmp`, private PID/cgroup/IPC namespaces, no published ports or
devices, no-new-privileges, default seccomp, dropped capabilities and bounded
CPU, memory and PID counts. A fixed privileged init only changes the workload to
the caller's nonzero UID/GID with no supplementary groups; the actual workload
is non-root and capability-free. Linux PID-namespace teardown covers every tool
descendant, including double-forked or `setsid` processes. Native model
processes remain outside this container and retain separate process-group
cleanup, so daemonized native descendants are not covered by this stronger tool
boundary.

The host keeps an attached stdin lease with bounded heartbeats. EOF, a stalled
heartbeat, cancellation, deadline or supervisor loss stops the fixed init and
therefore the whole tool container; a failed lease is not replayed. Every
operation records a fresh nonce, requested/effective policy, image and full
container ID under `native-control/<role>-execution/`, with `creating`,
`created`, `starting`, `running`, `stopping`, `stopped`, `removed` or `unknown`
receipts. Cleanup is successful only after the verified owned container is
stopped and removal is confirmed. `unknown` means cleanup is unconfirmed,
poisons that execution host and blocks a clean ready/canceled result; never
recreate an uncertain operation.

Unit tests and a successful preflight do not establish a completed mixed-team
container goal. Inspect the retained native responses, checks, source snapshot,
patch and container receipts before accepting a result.

With no `--root`, goals use the owner-private platform state directory
`$XDG_STATE_HOME/forgectl/projects`, falling back to
`$HOME/.local/state/forgectl/projects`. To keep the registry and generated
worktrees under another private location, use an absolute, clean path:

```sh
forgectl projects goals --root /path/to/private/goal-registry
```

The dashboard polls retained goals. Open a goal to stay in one run workspace:
progress and status, inline questions, and labeled sections for the patch, all
check receipts and JSON details. Enter adds a newline to an answer; move to the
Send button to submit it explicitly. Reading evidence does not require an
Escape chain through separate screens. Cancellation asks for confirmation in
the same workspace and defaults to keeping the run active.

Use `1`–`4` for Overview, Diff, Checks and JSON; `a` or Tab enters a pending
answer, and `c` opens cancellation. Page Up/Down scrolls the form or evidence.
Outside an editor, `q` detaches; inside an editor it is ordinary text. Ctrl-C
always detaches. Tab and Shift+Tab move between answer/Send or cancellation
choices, and Escape leaves the answer editor without losing its draft.

Detaching only leaves the screen: the supervisor continues. Cancellation is
different—it queues a request to the owning supervisor. Treat it as complete
only when status is `canceled` and `cleanup_complete` is true; this confirms the
owned native process groups stopped, not that arbitrary daemonized descendants
are gone. State and any worktree or evidence already created are still retained.

### Diagnosing container startup failures

- **`prepare` failure:** this is metadata-only inspection, not a helper test.
  Check the explicit socket and that engine's API v1.51, Linux/cgroup v2,
  memory/PID limits and built-in seccomp. Against that same socket, use
  `docker --host unix:///absolute/docker.sock image inspect sha256:<approved-id>`
  to compare the exact local image ID, engine/image architecture, declared
  volumes (none) and environment defaults (only PATH, HOME, LANG, LC_ALL, TZ).
  Do not diagnose against a tag or an ambient Docker context.
- **Start helper failure:** read the cause after `container helper prerequisite:`.
  This step already uses the role's full mounts and runtime cwd as the caller's
  nonzero UID/GID; it can fail before the boundary probe. A `container create
  failed`/HTTP error or mount-directory/`.git` error calls for the path checks
  below, not just image inspection. `did not execute its required Linux goal
  helper` can mean a missing/non-executable helper OR a runtime cwd inaccessible
  to that UID. Compare the exact image's trusted local build inputs with
  `Dockerfile.goal-tools` for Linux `/usr/local/bin/forgectl` and its required
  container-init protocol. `did not prove non-root, capability-free,
  no-new-privileges and seccomp execution` instead indicates failed identity/
  policy proof, including UID/GID, supplementary groups and capabilities.
- **Mount/UID checks (helper or probe):** inspect the failing role's absolute
  worktree, runtime cwd and Git paths against the VM's existing shared
  directories; check canonical paths, ownership and access for the requested
  UID/GID without changing permissions. Host existence alone is insufficient.
  Include the result-patch path for later mount failures. The probe error
  `container mount/UID boundary did not engage; verify runtime file sharing
  before model work` means its runtime write, read-only readable Git metadata
  or absent control-state checks failed. For coordinator/reviewer, `container
  model role received writable implementation access` means the read-only
  worktree check failed. Preserve errors and receipts; do not restart the
  VM/engine or change global sharing/permissions as a diagnostic shortcut.
- **Uncertain operation/cleanup:** inspect the matching nonce, immutable image
  and full container ID in `native-control/<role>-execution/` receipts (see
  below). `unknown` is not proof of absence: execution may have occurred and
  descendants may remain. Never replay that operation, erase its state or use
  a host-command fallback; an unavailable host must stay unavailable pending
  operator investigation, not a weaker isolation mode or provider substitution.

### Reading cleanup receipts

In the retained run directory, each role's `native-control/<role>/` keeps
`guardian-startup.json` and a final `guardian-receipt.json` after cleanup. Failed
native startup can also produce a receipt, with status `failed`. Native teams
retain a separate pair for each credential-free execution host in
`native-control/<role>-execution/`. Startup status `started` identifies the owned
native child: `native_pid`, `process_group`, `socket` and `started_at` must match
in the final receipt to pair the same process incarnation. Validation requires
a final status of `stopped`, no receipt errors and both cleanup booleans true;
missing or mismatched receipts are not evidence of a clean shutdown.

In container isolation, `native-control/<role>-execution/` is the container
execution host's private control directory. It contains one receipt sequence per
short-lived container operation instead of guardian startup/final receipts. A
final `removed` receipt must carry the matching image and full container
identity, a stopped state with PID zero, and confirmed runtime absence. A
missing, mismatched or `unknown` sequence is not evidence that descendants
ended safely; treat the host as unavailable rather than retrying the command.

`group_gone` records that the owned process group was absent when checked;
validation checks its absence again. `socket_gone` records that the socket path
was absent or its socket was removed, not that every process using it exited.
Goal-state `cleanup_complete` means the supervisor's cleanup returned no error
(including closing started roles and their execution hosts); it is not a task
correctness or all-descendant containment claim. Daemonized descendants that
leave the owned process group may survive even when these values are true.

Loss of the logical supervisor closes its lease pipe: a surviving guardian
responds by stopping the owned native process group, removing its socket and
writing the final receipt. Killing the guardian itself is different: it can
prevent that cleanup and receipt publication, leaving the native child alive.
Neither supervisor death nor a startup receipt alone proves cleanup, and an
abrupt supervisor death need not update goal-state `cleanup_complete`. Retained
PIDs are historical evidence, not a later kill list: PID/process-group IDs can
be reused, so do not use stored PIDs to kill processes later.

### Scripted CLI walkthrough

The singular `forgectl projects goal` tree provides the same retained product
without the TUI. Put the goal in a bounded regular, non-symlink JSON file:

```json
{
  "version": 1,
  "title": "Document widget setup",
  "objective": "Add the approved widget setup guide.",
  "repository": "/path/to/repository",
  "base_revision": "",
  "allowed_paths": ["docs/widget.md"],
  "acceptance": ["The guide contains a tested setup walkthrough."],
  "checks": [["/usr/bin/git", "diff", "--check"]],
  "model": "gpt-5-codex",
  "limit_minutes": 10
}
```

Replace the example model with the exact OpenAI model available to the local
Codex installation.

An optional `team` object records all three explicit assignments. For example,
add the following property to the goal object, substituting exact models that
the selected native installations support:

```json
"team": {
  "coordinator": {"harness": "codex", "provider": "openai", "model": "gpt-6.1-sol"},
  "implementation": {"harness": "pi", "provider": "openai-codex", "model": "gpt-6.1-sol"},
  "reviewer": {"harness": "claude", "provider": "anthropic", "model": "claude-opus-5-5"}
}
```

To opt this explicit team into Linux container tool isolation, add this sibling
property and replace the example digest with the ID from the already prepared
local image:

```json
"isolation": {
  "mode": "container",
  "socket": "/path/to/docker.sock",
  "image": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
```

`prepare` checks local engine metadata, image identity/architecture and image
defaults before source HEAD. It does not run the helper or mount/UID boundary
probe. `start` performs that helper and path probe for each role before native
model work; a failure refuses the run. It does not pull/build the image, start
Colima or another VM, or use a host-command fallback. Container mode uses Linux
binaries and argv semantics; make the approved worktree, role-runtime, Git
metadata and result-patch paths visible to the engine. Leaving `isolation` absent
retains the existing process-group cleanup behavior.

Leaving `team` absent preserves the existing version-one contract and all-Codex
interpretation; retained specs are not rewritten. Every selected harness must
be locally installed and natively authenticated. Pi accepts direct `openai` or
`openai-codex`; Claude Code requires `anthropic` and a canonical `claude-` model,
not an alias. No harness/provider fallback occurs. The configured launch binary
paths and `FORGECTL_PI_BIN` / `FORGECTL_CLAUDE_BIN` overrides are honored. Executable
entrypoint paths and their bytes are recorded and rechecked before supervision.

First prepare and review the returned `prepared.spec.base_revision`. Then copy
that full Git object ID into `--expect-head`; `start` refuses to launch unless
both the exact revision and `--approve` are present:

```sh
forgectl projects goal prepare --file /path/to/goal.json --json
forgectl projects goal start --file /path/to/goal.json \
  --expect-head <full-sha-from-prepare> --approve --json

forgectl projects goal list --json
forgectl projects goal status <goal-id> --json
```

The singular CLI uses the same default registry. To select the explicit root
shown above, place `--root /path/to/private/goal-registry` after `goal` in each
command, for example:

```sh
forgectl projects goal --root /path/to/private/goal-registry \
  status <goal-id> --json
```

If the source HEAD changed after preparation, prepare again and review the new
revision; do not substitute it silently. A dirty source checkout is allowed but
warned about: only committed HEAD enters the isolated worktree, never local
edits or untracked files.

When status is `needs_operator`, copy the current question ID and provide the
answer as bounded literal input. The answer clarifies the existing goal and
cannot expand its permissions:

```sh
forgectl projects goal respond <goal-id> --question <question-id> \
  --answer-file /path/to/answer.txt --json
# Or read the literal answer from stdin:
forgectl projects goal respond <goal-id> --question <question-id> \
  --answer-file - --json
```

Inspect or stop a retained run with:

```sh
forgectl projects goal artifact <goal-id> --kind patch
forgectl projects goal artifact <goal-id> --kind check --index 0
forgectl projects goal artifact <goal-id> --kind json --json
forgectl projects goal cancel <goal-id> --json
```

`artifact` verifies retained patch and check bytes against their recorded
digests before returning them. Goal state remains after `ready`, failure or
confirmed cancellation. Any branch, linked worktree, native receipts, result
patch or check outputs already created are retained with it for inspection; an
early failure or cancellation may happen before some or all of those artifacts
exist. The flow never commits, pushes, merges, deploys or installs the result,
and it provides no automatic cleanup command.

### What the result means

When a legacy Codex-trio run reaches model execution, it uses three native Codex
sessions on the selected direct OpenAI model: a coordinator, an implementation
worker and an independent reviewer. Task presets do not change that team. Explicit
native teams retain the same roles with their approved harness/provider/model
assignments; Hermes is not a supported self-service team member. A run may
fail or be canceled during workspace preparation before those sessions start.
On the model-execution path, the implementation worker is the only model role
allowed to edit the isolated worktree. After it stops, the supervisor freezes a
patch, requires the reviewer to pass that exact digest, then executes each
approved verification argv directly. A check must exit zero without changing
the reviewed patch, and the original source checkout must remain unchanged.

Legacy Codex sessions run with approval policy `never`, owner-private control state
and workspace-scoped writes. Apps, hooks, memories, multi-agent mode, plugins,
configured MCP servers, notifications and web search are disabled. Tool and
verification network access is disabled; only the direct OpenAI model transport
remains enabled. A denied operation is a refusal, not a reason to weaken the
boundary or select an external tool.

Explicit native teams use Codex app-server, Pi RPC and Claude Code stream JSON,
not a model-translation gateway. Their only model-callable tools are the
role-bound `goal_exec`, `goal_read`, `goal_write`, `goal_mail` and `goal_inbox`
bridge. A separate credential-free Codex execution host performs commands and
checks without creating a model thread. It is an execution dependency, not a
fourth model teammate. Built-in tools, ambient hooks, plugins and other MCP
servers are disabled; authentication remains with each native harness.
Accepted input, native consumption and settled completion are separate
receipts. A successful mailbox write is never proof that a model finished.

File helpers reject unsafe paths and disallowed writes. Arbitrary commands
retain a workspace-level kernel write boundary and a post-execution allowed-path
result gate, not a per-file OS allowlist. Process-group cleanup proves only the
owned groups disappeared: a daemonized child can leave its original group.
The real negative control reproduced that existing limitation. The current
cleanup contract retains and discloses it; stronger VM/container isolation is
separate follow-up work, not an implicit guarantee. Complete mixed-team result
acceptance remains unverified; neither successful native reply probes nor
passing unit tests establish it.

`ready` means the frozen patch passed independent native review, every approved
check passed, the patch stayed unchanged and the source checkout stayed
unchanged. It does **not** mean Cameron accepted the change, and it does not
mean committed, merged or deployed; `human_accepted` remains false. Review the
result patch, check outputs, native receipts and related artifacts in the
retained run directory, using `artifact` and `status` where applicable. Inspect
the separate linked worktree on its own before deciding what to do next.

If a nonterminal goal's heartbeat becomes stale, status reports `unknown` and
states whether the supervisor lease is still held. `unknown` is not proof that
the process is dead or safe to restart. Status, list and artifact reads never
restart or replay work, and respond/cancel refuse an unresponsive goal. Inspect
the retained state and supervisor log, plus guardian and native receipts when
present; there is no blind replay or promise of arbitrary crash recovery.

## Attended repository-goal pilot

`scripts/run-project-goal.mjs` exercises one bounded repository task through an
actual native team: Codex coordinates, Pi implements, and a separate read-only
Codex session reviews. The operator supplies a plain-language goal, allowed
existing tracked paths, and acceptance criteria. The supervisor flushes only the
task's dedicated mailbox; the agents author and consume their own messages using
`surface send --queue-only`, without attempting native transport from a sandbox.

This older attended workflow is an opt-in source-build pilot, not the compiled
self-service flow above, a general scheduler or the interactive Projects launch
flow. Run `node scripts/run-project-goal.mjs --help` for the
required explicit binary, model, extension and path arguments. The included
`scripts/project-keyboard-help-goal.json` is the first real repository goal.
The implementation worktree must be clean, linked, owner-controlled and disjoint
from both its primary checkout and a fresh owner-private runtime directory.
`--expect-head` pins the source revision; `--live` authorizes native model calls.
Supply the normal Pi authentication/safety extensions explicitly rather than
disabling them. The pilot verifies direct OpenAI routing before model work.

Inspect progress without launching anything:

```sh
node scripts/run-project-goal.mjs --status /absolute/private/runtime-root
```

`state.json` answers what is happening and whether an operator decision is
needed. The private runtime also retains the exact goal, append-only events,
project registry, native mailbox, reviewed `result.patch`, checkpoint and recovery
receipts. The project can be inspected with the existing `agents` and `timeline`
commands using the state's `project_root` and `project_id`.

`ready_for_verification` is deliberately not success: `result_verified` remains
false until a separate operator independently inspects and tests the returned
artifact. The pilot itself never commits, pushes, merges or installs. It stops
only processes it started and retains cleanup failures explicitly. Its recovery
exercise replaces a settled coordinator, recovers the same goal and result, and
proves that neither implementation dispatch nor source changes were repeated.
It does not prove arbitrary crash recovery. Interrupted/failed roots and claimed
worktrees are retained for inspection and cannot be automatically reused.
Canonical paths and hashes identify trusted local package inputs; they are not
authentication against another process running as the same OS user.

## Remaining integration

These commands remain a source-build integration, not a global installation. Native Codex attachment, UI reconnect, receiver restart, and coordinator replacement have live receipts in the [Projects implementation plan](../plans/2026-10-17-projects-surface.md). Pi RPC and Hermes ACP use read-only thread timelines rather than concurrent native interfaces.

Hermes shell-based replies remain subject to its native command policy. The opt-in `surface hermes --native-replies` channel instead accepts explicit assistant reply data, with matching live/history evidence and exact enrolled routing. It grants no tool permissions, publishes nothing from denied turns, and never retries an earlier denied command. See the [native reply contract](surface-team.md#message-only-native-replies). Final source-build acceptance is tracked separately in the implementation plan.

The existing [team mailbox](surface-team.md) remains the communication authority. Project context and teammate messages never count as operator approval. Desk's human approval queue remains separate from read-only project monitoring.
