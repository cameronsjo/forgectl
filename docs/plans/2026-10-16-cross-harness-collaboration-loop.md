# Cross-harness collaboration loop

Status: in progress
Next action: rerun live Codex/Pi acceptance with explicit executable and model selection after correcting the first runtime failures.
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
- The second live attempt reached both runtimes but did not delegate work. Pi selected an unsupported `openai-codex/gpt-5.5` model; the verifier now checks explicit model selection and treats native model errors as failures. Codex's login shell selected an older installed forgectl despite the inherited `PATH`; delegation prompts and persisted reply hints now use the isolated executable's absolute path.
- Starting a workspace-write Codex thread persisted scratch trust entries in global configuration. Codex removed only its own two entries, preserving concurrent unrelated changes. The verifier now starts read-only threads with process-scoped untrusted-project overrides and checks for scratch-path persistence. Task turns retain a scratch-only workspace-write sandbox with network access disabled; this is not a global trust or sandbox change.
- A fresh no-model probe of the corrected initialization returned an idle read-only thread, left global configuration byte-identical, and stopped its owned server with exit 0. After the reply-path correction, all mail tests, the focused CLI tests, and the embedded-extension behavioral tests passed again; the shell-quoting test executes the rendered argument through a real shell.
- The direct OpenAI retry selected `gpt-5.4` from Pi's advertised list but the account rejected it before collaboration. Advertised model metadata is not account acceptance. A subsequent retry uses `openai-codex/gpt-5.6-sol`, found in both the current account metadata and Pi's native registry. An OpenRouter trial was blocked before execution pending third-party destination approval; Codex continued with the direct OpenAI provider instead.
- The shell regression initially triggered the pinned linter's dynamic-shell-argument rule. Its fixed test now supplies the trusted fixture through shell stdin; the focused regression and pinned linter both pass (`0 issues`). No production shell invocation changed in this adjustment.
- Pi completed a real direct-OpenAI setup turn on `gpt-5.6-sol`. The next live attempt reached a genuine Codex-authored delegation but its network-disabled sandbox refused the local Unix socket; the durable message correctly remained `queued` with exit 75. The verifier now supervises the registered private mailbox with automatic `flush` calls while waiting, preserving the model's network restriction instead of enabling broader access. No human writes or relays agent message bodies.
- A later retry exposed concurrent global model drift: Codex selected a newly configured `gpt-6.1-sol`, which the account rejected. The verifier now requires an explicit Codex model and checks the selected model for both initial and replacement coordinators. The next run pins the previously working `gpt-6-astra` coordinator and `gpt-5.6-sol` Pi worker. Supervisor review added ledger-scope validation and a strict durable-message ordering check for delegation, clarification, answer, and completion.
- The pinned-model supervised run delivered a genuine Codex task to Pi, intentionally lost the acknowledgement, and reconciled its receipt without another delivery; native Pi history contained exactly one matching message. Pi then authored a clarification, but Codex rejected its queue submission because the target thread was ephemeral. The verifier now creates persistent native Codex threads, retaining read-only initialization and the same scoped task sandbox. Native session history is allowed; global trust/configuration changes are not.
- With persistent threads, Codex and Pi completed the real task in durable delegation/clarification/answer/completion order. The worker artifact passed its executable positive check and duplicate-input negative control; lost-ack, busy delivery, stopped-receiver queueing, and same-session Pi restart passed. The run then stopped at a 15-second coordinator shutdown deadline while ancillary probe replies were still active; cleanup subsequently confirmed its exit. The verifier now keeps those transport probes silent, settles the old coordinator before replacing it, and requires actual native consumption/completion of the new coordinator's reply turn.
- Review found that Codex's workspace-write policy includes general temporary directories unless explicitly excluded. The verifier now excludes both defaults and checks the actual native sandbox with an allowed scratch write and a denied adjacent write. An independent no-model probe already passed that control; the final full live run must pass it too. Pi's earlier initialization timeout is addressed by a bounded two-minute startup window without removing authentication or safety extensions.
- The next attempt stopped before model work when metadata-only Pi help exceeded 30 seconds under extreme host load (`uptime` reported a one-minute load average of 961.81). A read-only process inventory found no remaining task harnesses; unrelated workloads were not terminated. Metadata probes now use the same bounded two-minute startup allowance, and Pi version inspection, like help inspection, excludes unrelated extensions and network catalog refresh. Actual model processes still retain their authentication and safety extensions.

## Implementation review

The first implementation adds run-scoped registration, a dedicated Codex endpoint, write-ahead `unconfirmed` state, and receipt-only reconciliation. Its API and limitations are documented in [surface team](../commands/surface-team.md). Targeted Go checks cover coordinator/worker identity, stale run rejection, registration replacement, private ledger handling, crash ambiguity, and no-redelivery receipt reconciliation.

Inspection of the installed Pi runtime found that `sendUserMessage` returns before asynchronous acceptance. The extension therefore waits for an exact native user-message match rather than treating the API return as success. A `message_start` receipt still precedes transcript persistence and is labeled accordingly. Independent review also covers session binding and socket ownership; these are gates before live acceptance.

The installed Codex Unix transport uses WebSocket framing, not JSON lines. The live verifier must complete a real initialization against its dedicated socket before any model task. Neither a raw socket connection nor a zero-exit proxy with empty output counts as that evidence.
