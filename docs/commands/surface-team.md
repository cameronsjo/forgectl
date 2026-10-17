# Explicit cross-harness teams

`forgectl surface team` enrolls named native conversations in a private ledger. It does not launch harnesses, grant tools, change installed integrations, or enable worker-to-worker messaging. A coordinator and its workers may exchange task messages; those messages are not instructions from the operator and cannot authorize privileged actions.

This is an opt-in source-build workflow. Use the same isolated `forgectl` build on every participant's `PATH`; an older installed binary may not have these commands. Invoke its absolute path when starting a task because login profiles and aliases may replace `PATH`. New messages retain that executable path in their reply hint, including across queued retries. Keep native sandbox, approval, authentication, and guard extensions enabled.

The separate [persistent-project commands](surface-project.md) record logical projects and threads. Creating those records does not enroll or launch a member; the mailbox remains the communication authority.

## Register members

```sh
forgectl surface team register coordinator --ledger /absolute/private/team \
  --harness codex --worktree /absolute/coordinator \
  --coordinator --socket /absolute/private/codex.sock --json

forgectl surface team register pi-worker --ledger /absolute/private/team \
  --harness pi --worktree /absolute/worker \
  --socket /absolute/private/pi.sock --json

forgectl surface team status --ledger /absolute/private/team --json
forgectl surface team extension --ledger /absolute/private/team --json
```

The ledger must be an absolute, non-symlink directory private to its owner (mode `0700`). Registration creates it when absent. Worktrees must exist. The extension command writes this build's embedded Pi extension into the ledger without overwriting a different build.

Registration returns a `member` and an `environment` object. Supply that environment to the corresponding harness process, preserving its normal environment. It contains `FORGECTL_LEDGER`, `FORGECTL_RUN_ID`, and either `FORGECTL_COORDINATOR` or `FORGECTL_WORKER`; Pi also receives `FORGECTL_INBOX`. These variables and owner-private files prevent accidental role or session confusion, not impersonation by another process running as the same operating-system user.

The same Pi inbox extension supports either registered role. Exactly one worker or coordinator identity must be set; a coordinator additionally requires its absolute ledger and run ID. Mixed worker/coordinator identities and a coordinator carrying a drain-worker identity remain inert. When starting a replacement harness, use its newly returned team environment rather than retaining the retired process's team identity variables.

A Codex socket selects the dedicated app-server via `codex queue --remote unix://PATH`. Codex queueing requires a persistent native thread; `ephemeral: true` threads reject queued submissions. Its thread may be omitted only during initial enrollment. Once the native thread is created, repeat registration with `--replace --thread THREAD_ID --run-id ORIGINAL_RUN_ID` to bind that same process. An omitted socket retains the legacy default Codex-daemon behavior; isolated teams should always supply one.

For a replacement process, register the same name with `--replace` and omit `--run-id` to generate a new incarnation. Old processes then fail identity checks for sends and events. Replacement preserves the member's coordinator role and watchers. It does not resume native conversation history: resume the intended session using the harness's own facilities and verify that identity separately.

New teammate messages snapshot the sender and recipient RunIDs. A queued message for a retired recipient run fails without delivery to the replacement; an unconfirmed message remains unconfirmed without asking the replacement for a receipt. Native-session discovery within the same run still permits delivery. Legacy envelopes and system notices without run IDs retain their older behavior and cannot establish exact project-thread attribution.

Repeating the same RunID is allowed only for an exact identity no-op or filling an initially empty native-session ID. It preserves observed state and watchers; changing a known session, role, harness, worktree, socket, or pane requires a fresh RunID. Another current member cannot claim that RunID. Persistent-project bindings additionally reject historical RunID reuse within their private ledger; ordinary team registration does not keep a historical identity index.

Pi refuses to bind an existing socket path, including a stale one, rather than risk unlinking another process's endpoint. On restart, register a fresh socket path and resume the saved native session. `FORGECTL_INBOX_RECEIPTS` names the private receipt file; its default is the socket path plus `.receipts`. Receipts retain their original run and native session: preserving a file across a new run does not let the replacement claim old consumption or reuse old message IDs.

Run/session-bound Pi requests use inbox protocol v2, carrying the expected RunID and any known native-session ID. The shipped extension refuses a legacy request on a run-bound socket and validates identity before delivery, receipt lookup, or state reporting. Old extension builds refuse v2 instead of silently delivering elsewhere. An acceptance response with a mismatched identity remains unconfirmed. A native-session switch while awaiting consumption cannot become success for the new session. Upgrade the source-built executable and its embedded extension together for these enrollments.

## Message outcomes

```sh
forgectl surface send pi-worker 'Review the task and reply with one clarification.' --json
forgectl surface inbox --json
forgectl surface flush --json
```

- `sent` (send exit `0`): the adapter reports its acceptance evidence, not completed work. Codex reports native queue acceptance. Pi requires an exact rendered-message match in a native user-message event or resumed history; an extension API return alone is insufficient.
- `queued` (send exit `75`): definite non-acceptance is eligible for a later retry, subject to backoff, expiry, and attempt limits.
- `unconfirmed` (send exit `1`): delivery may have happened. The mailbox records this state durably before invoking the transport, including when a process crashes between delivery and recording its result.
- `failed` or `expired`: the message is no longer automatically retried. Inspect `detail` rather than interpreting an exit code alone.

`flush` reconciles an unconfirmed Pi delivery by querying its receiver receipt, never by resending it. A matching accepted receipt changes it to `sent`; missing or conflicting evidence remains visible. Codex currently has no receipt reconciliation adapter, so an ambiguous queue invocation remains `unconfirmed`. `flush` exit `0` means the scan completed; inspect its `unconfirmed`, `failed`, and `expired` counters.

Pi receipts retain message ID and exact rendered-text digest. The rendered envelope includes the unique message ID. Native `message_start` occurs before transcript persistence; its receipt proves observed consumption, not durable conversation history, successful tool execution, or a completed model turn. Keep native history, artifact checks, and mailbox receipts as separate evidence.

No background broker is installed. Sends and native boundary events trigger flushing, and an operator or an existing supervisor may invoke `flush`. An entirely stopped team needs an explicit restart and flush. Unresolvable uncertainty must not be converted into a fresh message merely to obtain a green status: inspect native evidence first.

A network-disabled Codex sandbox can write its authorized mailbox while refusing the receiver's Unix-socket connection. The message then stays `queued`; this is not native acceptance. The live verifier acts as a task-scoped delivery supervisor by flushing the private team's queue while waiting for replies. It never writes message bodies on an agent's behalf or expands the agent's network permissions. This supervised transport, rather than direct socket access from the sandboxed shell, is part of that acceptance topology.

## Source-build verification

Run the focused checks from the forgectl repository:

```sh
TERM=xterm-256color go test ./internal/surface/mail ./internal/cli -count=1
node --experimental-strip-types scripts/test-forgectl-inbox.mjs
node scripts/test-cross-harness-verifier.mjs
go build -o /absolute/isolated/bin/forgectl .
node scripts/verify-cross-harness-loop.mjs --help
```

The behavioral extension test uses the actual embedded TypeScript source with a controlled extension API, including missing native acceptance, lost acknowledgements, duplicate delivery, and session recovery. It is not evidence that a live model completed a task.

The opt-in live verifier requires a fresh private scratch directory, explicit isolated binary paths, a locally installed `ws` module entrypoint through `--websocket-module`, and the installed Pi authentication and safety extensions through repeatable `--pi-extension` arguments. The WebSocket library handles Codex's native Unix transport without a TCP bridge or a new global installation. Prefer the native Codex executable rather than a wrapper so the verifier owns the server process it stops.

With these paths and existing authenticated model IDs set for the local machine:

```sh
scratch=$(mktemp -d /private/tmp/fctl-live.XXXXXX)
node scripts/verify-cross-harness-loop.mjs --live \
  --forgectl "$FORGECTL_BUILD" --root "$scratch" \
  --websocket-module "$WEBSOCKET_MODULE" \
  --codex "$CODEX_NATIVE" --codex-model "$CODEX_MODEL" \
  --pi "$PI_BINARY" --pi-provider "$PI_PROVIDER" --pi-model "$PI_MODEL" \
  --pi-extension "$PI_AUTH_EXTENSION" \
  --pi-extension "$PI_GUARDS_EXTENSION" \
  --pi-extension "$PI_RULES_EXTENSION" \
  --pi-extension "$PI_PLAN_EXTENSION"
```

All path variables must be absolute. Include every authentication and safety extension required by the local Pi setup; these arguments are not permission to disable existing protections. Successful completion writes `ACCEPTED` only after owned-process cleanup and the final configuration check. The [first accepted run](../plans/2026-10-16-cross-harness-collaboration-loop.md#accepted-live-receipt) and [accepted Codex-to-Pi coordinator handoff](../plans/2026-10-16-cross-harness-collaboration-loop.md#accepted-cross-harness-handoff-receipt) record their exact tested builds and limitations.

Supply the required `--codex-model ID` and the paired `--pi-provider NAME --pi-model ID` to select existing authenticated models explicitly. The verifier checks the native selected models and fails on a model error rather than treating a settled failed turn as readiness. It initializes persistent Codex threads read-only with a process-scoped untrusted-project override, then grants each task turn workspace writes only within the scratch root. Their native session records remain in Codex's normal history. It checks that Codex did not persist the scratch path in global configuration.

The Codex sandbox explicitly excludes the otherwise writable `/tmp` and `TMPDIR` defaults. A native no-model command must successfully write inside the scratch root and fail to write an adjacent temporary path before model work begins. Startup has a bounded two-minute allowance for the required Pi extensions. Replacement waits for the old coordinator's current work to settle, and success requires the new coordinator to consume the worker's reply in its own native thread and finish that turn—not merely accept it into a queue.

The verifier then retires that Codex coordinator and enrolls Pi under the same coordinator name with a new run, socket, receipt file, and native session. Pi must discover the earlier completed task ID from the durable inbox, delegate a recovery check, consume the worker's reply, and complete a native acknowledgement. The original verified artifact and execution journal must remain unchanged. This is an explicit task-context handoff, not conversion of Codex's private conversation history into Pi history.

The verifier starts only task-owned processes, records native session/model identities, and leaves private evidence on failure. Its acceptance gate requires a real delegation, clarification and answer without human relay, a worker-created executable artifact, an independent positive check and corrupt-input negative control, and interruption/replacement probes. Merely running its syntax or help check does not satisfy that gate.

## Native Hermes sessions

`surface hermes` owns one native Hermes ACP process and exposes its enrolled session through a private Unix socket. It is a per-process adapter, not a new global broker. Install and authenticate Hermes separately; forgectl does not import credentials or change native configuration.

```sh
forgectl surface team register hermes-worker --ledger /absolute/private/team \
  --harness hermes --worktree /absolute/worker \
  --socket /absolute/private/hermes.sock --json
```

Supply the returned environment to the supervisor, preserving the intended private `HERMES_HOME`. Start from the registered worktree:

```sh
forgectl surface hermes --command /absolute/venv/bin/hermes-acp \
  --model openai-codex:MODEL_ID --json
```

The supervisor creates a native session when the registration has no thread ID; otherwise it loads that exact session. Startup checks the configured provider-qualified model and native `default` permission mode, then binds the session to the roster. It refuses mismatched role, run, worktree, socket, or session identities. `--listen-socket` may select a different owned listener when a task-scoped transparent proxy occupies the registered route socket.

Native permission requests are cancelled, not approved on another agent's behalf. Unsupported client operations are rejected. This headless adapter does not provide interactive approvals or a filesystem sandbox; Hermes's existing native terminal policy still applies. Team enrollment grants neither file-write permission nor authority to bypass a denial.

Hermes delivery persists a pending receipt before prompting and immediately remains `unconfirmed`. After the turn, an exact message in native session history can confirm consumption. `flush` asks for that receipt without resending the message. A busy receiver returns `queued`; an unknown receipt stays `unconfirmed`. Neither history acceptance nor a settled native turn proves task success.

To restart, stop the owned supervisor, replace the member registration with a fresh run and socket, and pass the original native session to `surface team register --thread`. The supervisor reads that registered session; its optional `--session-id` must match it. Receipt files are scoped to the member and native session in the private ledger. Another supervisor cannot concurrently own that receipt store, and an existing socket is never stolen.

### Hermes source-build verification

```sh
go test ./internal/surface/hermes ./internal/surface/mail
node scripts/test-hermes-verifier.mjs
scratch=$(mktemp -d /private/tmp/fh.XXXXXX)
node scripts/verify-hermes-collaboration.mjs --live \
  --forgectl "$FORGECTL_BUILD" --root "$scratch" \
  --websocket-module "$WEBSOCKET_MODULE" \
  --codex "$CODEX_NATIVE" --codex-model "$CODEX_MODEL" \
  --hermes "$HERMES_ACP" --hermes-python "$HERMES_PYTHON" \
  --hermes-home "$HERMES_HOME" --hermes-model "$HERMES_MODEL"
```

Use absolute executable paths, retaining the virtual-environment Python path rather than resolving its interpreter symlink. `HERMES_MODEL` is the unqualified OpenAI model ID. This verifier requires a preconfigured private access-only OpenAI credential, no copied refresh token, all auxiliary models pinned to OpenAI, and no provider fallbacks. It validates that configuration through native Hermes helpers and checks that Codex's global authentication and configuration remain unchanged. An expired credential is a failed prerequisite, not permission to refresh or replace another harness's login.

Hermes authors source as a bounded base64 mailbox payload; Codex materializes those exact bytes in its existing sandbox. The verifier requires executable positive, duplicate-input, missing-path, and clarified-default checks, plus native source provenance. This is worker-authored source with coordinator materialization, not a claim that Hermes wrote or executed a file. A separate real native write request must be denied. Lost acknowledgement, busy/stopped delivery, same-session restart, stale-run rejection, selected-session refusal, unique native consumption, and owned-process cleanup are independent gates. A live `ACCEPTED` receipt, not unit tests or help output, is required to claim this collaboration works.

The [accepted Hermes run](../plans/2026-10-16-cross-harness-collaboration-loop.md#accepted-hermes-collaboration-receipt) records the exact clean build, native sessions, artifact and evidence hashes, independent history audit, and remaining herdr/Projects gates.
