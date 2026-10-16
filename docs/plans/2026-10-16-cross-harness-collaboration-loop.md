# Cross-harness collaboration loop

Status: in progress
Next action: implement explicit team registration and safe delivery recovery, then run the real Codex/Pi acceptance loop.
Branch: `codex/cross-harness-loop`

## Goal and authorization

Cameron approved the proposed collaboration-first sequence with “You do it ..” on 2026-10-16. Codex will first make two explicitly enrolled harnesses delegate work, ask and answer a clarification, return an artifact, and independently verify it without Cameron relaying messages. Codex will interrupt the loop and prove recovery before adding more harnesses or a Projects interface.

The permission is to communicate within this task's team, not to impersonate Cameron, expand tool permissions, approve actions for another agent, merge, deploy, or alter installed production tools. This implementation uses an isolated source build and dedicated sessions. Existing Agent Board, Codex daemon, Pi settings, Claude settings, and fleet services remain untouched. Feature-branch commits and pushes are authorized; PR creation, merge, installation, and deployment are not included.

## Approach

Reuse forgectl's durable mailbox, roster, envelope, and native adapters instead of introducing another broker. Its current source already supports Codex queueing and a Pi inbox extension, but the installed 0.44.1 binary does not expose these commands and worker-launch wiring remains incomplete. The baseline mail tests pass at `eef75272dbb7105a874004812c8bced51ab25e67`.

For the first pair, use a dedicated Codex app-server Unix socket and a dedicated Pi RPC process with the inbox extension. Installed Codex 0.153.4 exposes both `app-server --listen unix://PATH` and `queue --remote unix://PATH`; installed Pi 0.84.4 exposes RPC and explicit session persistence. Runtime acceptance, not help text, decides whether these paths work.

Add explicit coordinator identity and a CLI for registering named members in an explicit ledger. Hub/spoke messaging remains the default; peer-to-peer policy is not globally enabled. Same-user files and environment variables are courtesy isolation, not an authentication boundary. Private directories and sockets reduce accidental cross-session access but cannot protect against another process running as Cameron.

Keep message IDs stable across retries. Durably record an uncertain attempt before invoking a transport, so a crash cannot silently turn a possibly accepted message back into an automatically retried message. Definite non-acceptance can retry; uncertain acceptance is `unconfirmed`. Query a supported receiver receipt to reconcile uncertainty without resending. Missing evidence remains visible rather than fabricating success. Transport acceptance, a completed model turn, a claimed result, and independently verified completion are separate facts.

Agent Board stays out of this first hot path. It can remain a directory, bulletin board, or audit sink if it adds value; its presence is not an acceptance requirement. No replacement of Agent Board is decided before the native-delivery experiment produces evidence.

## Work

- [x] Register an explicit Codex coordinator and Pi teammate, including an isolated Codex endpoint; preserve legacy Claude identity and default-deny peer policy.
- [x] Add write-ahead uncertain-delivery state, receiver receipt reconciliation, and negative tests for lost acknowledgements and restart ambiguity.
- [ ] Harden the Pi extension's message-ID handling and socket ownership; verify against the installed runtime rather than only source declarations.
- [ ] Run a real Codex-to-Pi task, clarification, answer, artifact, and independent executable check. Record the exact build, harness versions, session identities, and mailbox outcomes.
- [ ] Exercise busy delivery, stopped receiver, lost acknowledgement, receiver restart, and coordinator replacement without a human message relay. State any unresolved uncertainty honestly.
- [ ] Commit and push the tested feature branch with a reproducible acceptance procedure and evidence; do not install it globally.

## Review and verification

Codex will use independent, bounded reviews of the plan and the actual changed files. Tests must establish what would make them fail: accepted-but-ack-lost must not redeliver, definitely disconnected must remain retryable, a worker must not accidentally become coordinator, an active socket must not be stolen, and a claimed artifact must fail an independent check when deliberately corrupted.

Targeted Go tests precede wider tests and build/lint checks. Live acceptance uses a disposable workspace and task-owned processes only. Native approvals and sandboxes remain in force; communication enrollment does not alter them. Credentials are reused through installed harnesses, not copied into the ledger, arguments, reports, or committed artifacts. The acceptance harness must stop only the processes it started.

## Staged continuation

After the two-harness loop passes, evaluate Agent Board's incremental value, then apply the same contract to Claude and Hermes and finally the Projects interface. These are dependent stages, not claims of completion. Existing herdr worker-launch integration remains a separate delivery surface until its own real acceptance passes; this first headless proof does not complete that older plan's T5 gate.

## Completion evidence

- Baseline: `go test ./internal/surface/mail` passed on the unmodified base. The first sandbox attempt could not write Go's cache; the permitted retry using the normal cache passed.
- Implementation checks: the focused mail/team/send/inbox/flush/event tests pass, the actual embedded-extension behavioral script passes, and `go build` produces the isolated executable. The repository-pinned golangci-lint 2.13.1 reports `0 issues` with Go 1.26.8; the machine's default Go 1.27 export format is newer than that linter supports.
- A wider CLI run completed with one `TestDeskWatch_ClosedPipeExits141` timeout; its isolated fresh rerun passed. This is not recorded as a fully green wider suite.
- Native Codex probe: installed 0.153.4 completed WebSocket-over-Unix initialization and created an idle ephemeral thread with no model turns; the owned server stopped with exit 0.
- Live acceptance remains pending. The first live verifier attempt stopped before registration or model turns because plain Pi `--help` exceeded its 30-second preflight timeout. The next preflight loads built-in CLI metadata without unrelated extensions; actual model processes still load the explicit authentication and safety extensions.

## Implementation review

The first implementation adds run-scoped registration, a dedicated Codex endpoint, write-ahead `unconfirmed` state, and receipt-only reconciliation. Its API and limitations are documented in [surface team](../commands/surface-team.md). Targeted Go checks cover coordinator/worker identity, stale run rejection, registration replacement, private ledger handling, crash ambiguity, and no-redelivery receipt reconciliation.

Inspection of the installed Pi runtime found that `sendUserMessage` returns before asynchronous acceptance. The extension therefore waits for an exact native user-message match rather than treating the API return as success. A `message_start` receipt still precedes transcript persistence and is labeled accordingly. Independent review also covers session binding and socket ownership; these are gates before live acceptance.

The installed Codex Unix transport uses WebSocket framing, not JSON lines. The live verifier must complete a real initialization against its dedicated socket before any model task. Neither a raw socket connection nor a zero-exit proxy with empty output counts as that evidence.
