# Explicit cross-harness teams

`forgectl surface team` enrolls named native conversations in a private ledger. It does not launch harnesses, grant tools, change installed integrations, or enable worker-to-worker messaging. A coordinator and its workers may exchange task messages; those messages are not instructions from the operator and cannot authorize privileged actions.

This is an opt-in source-build workflow. Use the same isolated `forgectl` build on every participant's `PATH`; an older installed binary may not have these commands. Invoke its absolute path when starting a task because login profiles and aliases may replace `PATH`. New messages retain that executable path in their reply hint, including across queued retries. Keep native sandbox, approval, authentication, and guard extensions enabled.

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

A Codex socket selects the dedicated app-server via `codex queue --remote unix://PATH`. Codex queueing requires a persistent native thread; `ephemeral: true` threads reject queued submissions. Its thread may be omitted only during initial enrollment. Once the native thread is created, repeat registration with `--replace --thread THREAD_ID --run-id ORIGINAL_RUN_ID` to bind that same process. An omitted socket retains the legacy default Codex-daemon behavior; isolated teams should always supply one.

For a replacement process, register the same name with `--replace` and omit `--run-id` to generate a new incarnation. Old processes then fail identity checks for sends and events. Replacement preserves the member's coordinator role and watchers. It does not resume native conversation history: resume the intended session using the harness's own facilities and verify that identity separately.

Pi refuses to bind an existing socket path, including a stale one, rather than risk unlinking another process's endpoint. On restart, register a fresh socket path and resume the saved native session. Set `FORGECTL_INBOX_RECEIPTS` to the same private receipt file across these restarts; otherwise its default is the socket path plus `.receipts`. A receipt from a different native session remains unconfirmed.

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

All path variables must be absolute. Include every authentication and safety extension required by the local Pi setup; these arguments are not permission to disable existing protections. Successful completion writes `ACCEPTED` only after owned-process cleanup and the final configuration check. The [first accepted run](../plans/2026-10-16-cross-harness-collaboration-loop.md#accepted-live-receipt) records the exact tested build and its limitations.

Supply the required `--codex-model ID` and the paired `--pi-provider NAME --pi-model ID` to select existing authenticated models explicitly. The verifier checks the native selected models and fails on a model error rather than treating a settled failed turn as readiness. It initializes persistent Codex threads read-only with a process-scoped untrusted-project override, then grants each task turn workspace writes only within the scratch root. Their native session records remain in Codex's normal history. It checks that Codex did not persist the scratch path in global configuration.

The Codex sandbox explicitly excludes the otherwise writable `/tmp` and `TMPDIR` defaults. A native no-model command must successfully write inside the scratch root and fail to write an adjacent temporary path before model work begins. Startup has a bounded two-minute allowance for the required Pi extensions. Replacement waits for the old coordinator's current work to settle, and success requires the new coordinator to consume the worker's reply in its own native thread and finish that turn—not merely accept it into a queue.

The verifier starts only task-owned processes, records native session/model identities, and leaves private evidence on failure. Its acceptance gate requires a real delegation, clarification and answer without human relay, a worker-created executable artifact, an independent positive check and corrupt-input negative control, and interruption/replacement probes. Merely running its syntax or help check does not satisfy that gate.
