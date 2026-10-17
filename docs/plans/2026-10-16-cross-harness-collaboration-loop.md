# Cross-harness collaboration loop

Status: Codex/Pi collaboration and native Codex-to-Pi coordinator handoff accepted; Claude, Hermes, and Projects remain staged
Next action: connect the verified native Hermes ACP process to the existing mailbox and prove real collaboration and recovery while preserving OpenAI-only traffic and native permission checks.
Branch: `codex/cross-harness-loop`

## Goal and authorization

Cameron approved the proposed collaboration-first sequence with “You do it ..” on 2026-10-16. Codex will first make two explicitly enrolled harnesses delegate work, ask and answer a clarification, return an artifact, and independently verify it without Cameron relaying messages. Codex will interrupt the loop and prove recovery before adding more harnesses or a Projects interface.

The permission is to communicate within this task's team, not to impersonate Cameron, expand tool permissions, approve actions for another agent, merge, deploy, or alter installed production tools. This implementation uses an isolated source build and dedicated sessions. No existing Agent Board, Codex-daemon, Pi, Claude, or fleet installation/service configuration is changed. Feature-branch commits and pushes are authorized; PR creation, merge, installation, and deployment are not included.

## Approach

Reuse forgectl's durable mailbox, roster, envelope, and native adapters instead of introducing another broker. Its current source already supports Codex queueing and a Pi inbox extension, but the installed 0.44.1 binary does not expose these commands and worker-launch wiring remains incomplete. The baseline mail tests pass at `eef75272dbb7105a874004812c8bced51ab25e67`.

For the first pair, use a dedicated Codex app-server Unix socket and a dedicated Pi RPC process with the inbox extension. Installed Codex 0.153.4 exposes both `app-server --listen unix://PATH` and `queue --remote unix://PATH`; installed Pi 0.84.4 exposes RPC and explicit session persistence. Runtime acceptance, not help text, decides whether these paths work.

Add explicit coordinator identity and a CLI for registering named members in an explicit ledger. Hub/spoke messaging remains the default; peer-to-peer policy is not globally enabled. Same-user files and environment variables are courtesy isolation, not an authentication boundary. Private directories and sockets reduce accidental cross-session access but cannot protect against another process running as Cameron.

Keep message IDs stable across retries. Durably record an uncertain attempt before invoking a transport, so a crash cannot silently turn a possibly accepted message back into an automatically retried message. Definite non-acceptance can retry; uncertain acceptance is `unconfirmed`. Query a supported receiver receipt to reconcile uncertainty without resending. Missing evidence remains visible rather than fabricating success. Transport acceptance, a completed model turn, a claimed result, and independently verified completion are separate facts.

Agent Board stays out of this first hot path. It can remain a directory, bulletin board, or audit sink if it adds value; its presence is not an acceptance requirement. No replacement of Agent Board is decided before the native-delivery experiment produces evidence.

## Work

- [x] Register an explicit Codex coordinator and Pi teammate, including an isolated Codex endpoint; preserve legacy Claude identity and default-deny peer policy.
- [x] Add write-ahead uncertain-delivery state, receiver receipt reconciliation, and negative tests for lost acknowledgements and restart ambiguity.
- [x] Harden the Pi extension's message-ID handling and socket ownership; verify against the installed runtime rather than only source declarations.
- [x] Run a real Codex-to-Pi task, clarification, answer, artifact, and independent executable check. Record the exact build, harness versions, session identities, and mailbox outcomes.
- [x] Exercise busy delivery, stopped receiver, lost acknowledgement, receiver restart, and coordinator replacement without a human message relay. State any unresolved uncertainty honestly.
- [x] Commit and push the tested feature branch with a reproducible acceptance procedure and evidence; do not install it globally.

## Review and verification

Codex will use independent, bounded reviews of the plan and the actual changed files. Tests must establish what would make them fail: accepted-but-ack-lost must not redeliver, definitely disconnected must remain retryable, a worker must not accidentally become coordinator, an active socket must not be stolen, and a claimed artifact must fail an independent check when deliberately corrupted.

Targeted Go tests precede wider tests and build/lint checks. Live acceptance uses a disposable workspace and task-owned processes only. Native approvals and sandboxes remain in force; communication enrollment does not alter them. Credentials are reused through installed harnesses, not copied into the ledger, arguments, reports, or committed artifacts. The acceptance harness must stop only the processes it started.

## Staged continuation

After the two-harness loop passes, evaluate Agent Board's incremental value, then apply the same contract to Claude and Hermes and finally the Projects interface. These are dependent stages, not claims of completion. Existing herdr worker-launch integration remains a separate delivery surface until its own real acceptance passes; this first headless proof does not complete that older plan's T5 gate.

The accepted loop does not need Agent Board in its delivery path: forgectl supplies the durable mailbox and each native harness supplies conversation delivery. Keep Agent Board available for discovery, cross-session announcements, or human-facing audit; do not rewrite or remove it on the strength of this experiment. This proves that the configured Board integrations are not a prerequisite for automatic task replies, not that every Board capability is redundant.

The different-harness coordinator swap is now accepted in the second live receipt below. Remaining acceptance gates are Claude and Hermes participation under Cameron's provider and native-permission constraints, herdr launch wiring, and a persistent Projects/thread interface. Neither accepted run proves those later gates.

### Accepted bounded unit: replace Codex with Pi

This unit reused the approved selectable-coordinator contract without adding a new transport or provider. Registration supported a Pi coordinator, but the embedded inbox required `FORGECTL_WORKER` and remained inert for the emitted coordinator environment. Codex corrected that role mismatch and proved a native cross-harness handoff before adding Hermes.

- [x] Activate the Pi inbox for exactly one explicit worker or coordinator identity; reject mixed coordinator/worker identities, and require the explicit ledger/run fields for a coordinator. Preserve the existing worker path and all receipt, socket, and permission controls.
- [x] Add behavioral role controls against the embedded extension, including missing and conflicting identities.
- [x] Extend the live verifier after its existing Codex replacement: retire that coordinator, register the same role with a fresh run and Pi session, and have Pi read the existing task ledger, delegate a bounded recovery check, receive its worker's native reply, and settle successfully. No native private-history conversion is implied.
- [x] Verify that the retired Codex run can no longer send or report events, that the Pi coordinator's reply is consumed in its own native session, and that cleanup includes every newly owned process.
- [x] Commit and push the verified unit; keep installed tools and services unchanged. Keep all model traffic on OpenAI.

The implementation's independent mail-package tests and embedded-extension behavioral checks pass. The role-denial tests synchronize with completed child initialization and assert no registered callbacks before checking absent endpoints, so slow startup cannot impersonate a denial. The parent's first behavioral run timed out on an existing ownership-socket readiness check under heavy host load; an unchanged rerun passed. Registration-envelope negative controls and verifier syntax also pass. The cross-harness live gate passed on 2026-10-17, followed by an independent receipt audit and fresh artifact checks after cleanup.

The first extended live run correctly failed before the handoff: the worker hardcoded the positive fixture path, so the independent duplicate-input check exited zero. The delegation had not explicitly specified the input-path argument. Codex made the CLI contract explicit rather than changing the generated worker artifact or weakening the check; the negative check now also requires a duplicate-specific diagnostic, not an arbitrary failure or signal. Cleanup requested termination of both owned processes; Pi exited immediately and Codex exited zero after 46 seconds, beyond the old 30-second wait. The bounded exit wait now matches the two-minute startup allowance. This failed run never wrote `ACCEPTED`.

A retry launched inside the terminal tool sandbox failed to bind its private Unix socket with `EPERM`, before collaboration began. The verifier now awaits actual proxy listening before reporting startup or spawning native children, and a negative test requires a bind error to reject normally without a startup receipt. The live loop needs an approved host-level process; the nested Codex task sandbox remains restricted as described above.

The next approved host-level attempt received an OpenAI usage-limit error during Pi setup at 2026-10-17 01:01 UTC, before delegation. It stopped its owned Pi process and left the global configuration hash unchanged. Cameron resumed the task with “Continue”; the fresh 13:25 UTC run completed successfully without changing provider or model.

### Remaining provider and authentication gates

Cameron requested that model traffic stay on OpenAI. Codex has not enabled Anthropic or OpenRouter traffic, installed a translation gateway, or changed global authentication. No verified OpenAI-only native Claude route is available from this investigation; Claude live acceptance remains deferred under that constraint, not declared impossible.

On the 2026-10-17 continuation, read-only checks found no `hermes` executable, no `~/.hermes/auth.json`, and no `OPENAI_API_KEY` in the process environment. Codex asked whether Cameron permits an isolated Hermes setup and its native import of Codex credentials into a private authentication store. Cameron answered “Make it happen,” authorizing that setup. This does not authorize another model provider, changes to existing global authentication, mutation of the third-party Hermes checkout, or use of a production relay.

### Next bounded unit: native Hermes participation

- [x] Install the pinned, locally inspected Hermes source into a private temporary environment, without modifying the third-party source checkout or installing a global command.
- [x] Import only the expiring Codex access token through Hermes's native API-key credential-pool command into a separate private `HERMES_HOME`; verify the native selected provider and model without printing tokens or placing them in the task ledger. Do not import the shared refresh token. Preserve the existing Codex auth file.
- [ ] Reuse the smallest verified native session protocol and existing forgectl delivery contract; retain native permission checks and distinguish receipt, native consumption, turn completion, and independently checked output.
- [ ] Prove real Hermes participation, interruption/restart behavior, and selected-session delivery with disposable task-owned processes. Keep uncertain delivery visible rather than resending blindly.
- [ ] Record exact source/build/runtime evidence, commit and push the verified feature changes, and leave existing installations untouched.

Projects remains behind the agreed harness acceptance sequence. Existing herdr launch wiring remains a separate gate; this headless proof does not complete it. Agent Board remains optional for discovery, announcements, and audit, rather than a mandatory delivery broker. These items are deferred here, not silently dropped or marked complete.

### Hermes native preflight evidence

Codex archived the clean, locally inspected Hermes source at `02d1da49de5086946256cc157ff928dcffbe8ca1` into `/private/tmp/fctl-hermes.TwuIsK/source` and installed its frozen lock with the ACP extra into that root's private virtual environment. Native Hermes `0.15.1` reports `Hermes ACP check OK`; the original third-party checkout remains unchanged. Nothing was installed globally.

Inspection found that the ordinary OAuth import copies Codex's single-use refresh token, despite its independence wording. Refreshing that copy could invalidate Codex's existing login. Codex did not execute that import. Instead, the existing native `auth_add_command` API-key path accepts an access-only credential for `openai-codex`. The native importer reads the original credentials in process; only `access_token` is handed to that command, never the refresh or ID token. This expiring credential deliberately cannot refresh itself; expiration requires another explicitly scoped import or a separately authorized independent login. It is not a permanent authentication setup.

The offline `/private/tmp/fctl-hermes.TwuIsK/verify-access-pool.py` check exercised native pool selection, forced runtime resolution, and reactive refresh with network access forbidden. The access-only entry never called OAuth refresh; a fake OAuth entry did engage the refresh negative control. A separate native import/preflight verified one manual API-key entry, no singleton tokens, no refresh token, an owner-only store, unchanged Codex authentication bytes, and direct `openai-codex`/`codex_responses` routing. An independent source review confirmed that pool loading does not import Codex's global credentials.

The native ACP probe `node /private/tmp/fctl-hermes.TwuIsK/probe-native-acp.mjs` exited `0` with receipt `/private/tmp/fctl-hermes.TwuIsK/probe-983e8cc3-f2e7-4798-b392-1a7b0f1547a6/ACCEPTED.json`. Hermes completed a real `openai-codex:gpt-5.6-sol` turn, stopped, restarted, and loaded native session `0d34054e-361b-4470-8ad2-2bd9b56eb65d` with exactly one matching user and assistant message. Both owned processes exited `0`. Both sessions retained native `default` approval mode; no tool permission request occurred. The private runtime uses its own `HOME` and `HERMES_HOME`, pins every auxiliary task (including background titles) to the same OpenAI provider/model, and has no configured fallback providers. Codex's original auth file was byte-identical after the probe, and Hermes still had no refresh token. This proves native authentication, turn execution, and session persistence, not cross-harness collaboration.

The first native probe stopped before any prompt because its assertion expected an unqualified model ID. Native ACP actually returns `openai-codex:gpt-5.6-sol`; source inspection confirmed the provider-qualified contract and the corrected probe passed. That earlier run did not receive an acceptance receipt.

The next implementation extends forgectl rather than adding a global broker. An explicitly enrolled Hermes process uses ACP stdio behind its private per-process inbox socket. Durable mailbox state remains authoritative; the supervisor records a pending receipt before forwarding, refuses busy delivery before forwarding, and reconciles consumption only from matching native history. A missing or ambiguous history result remains unconfirmed and is not resent. Native permission requests are denied rather than silently approved, and native default mode is preserved. Transport consumption, native turn termination, and independently verified task output remain separate evidence.

## Accepted cross-harness handoff receipt

On 2026-10-17 at 08:32 America/Chicago (13:32 UTC), `node scripts/verify-cross-harness-loop.mjs --live ...` exited `0` and wrote `ACCEPTED` in `/private/tmp/fctl-live.KPUAyy`. This run repeats the entire original task and interruption sequence, then replaces the coordinator with a different native harness. Raw native events remain private.

- Product build: clean commit `83cc69016e7a5e9f356f3abd69d37fac8570fdd6`, executable SHA-256 `8e039772931a6ec2e5015118f7f8d7fdb565f53c8a7104aea90efed85dc386f0`. Compiled product source is unchanged at verifier commit `fea2e3495f54466ad2c56ce81fc7d09ca448d3c5`; verifier SHA-256 is `5bb72eeabc06b10e14a0e1925d5458c34be344b2311b947163396521b673c1dd`.
- Native runtimes: Codex `0.153.4` with `gpt-6-astra`; Pi `0.84.4` with direct `openai-codex/gpt-5.6-sol` and the four existing authentication/safety extensions. No other model provider was used.
- Task `cross-harness-ede02dd0-1096-4f73-bed2-309bde38c69e` completed in delegation/clarification/answer/completion order `0,1,2,3`. The utility passed its positive check with exit `0` and rejected the separate duplicate-input fixture with exit `1` and a duplicate-specific diagnostic. Codex independently reran both checks after cleanup. The execution journal contains one record for the task.
- Lost acknowledgement reconciliation observed one proxy delivery, one native user message, and one pending/accepted receipt pair. Busy delivery reconciled to `sent`; stopped delivery returned `queued`/exit `75`; restarting Pi reopened its original native session and consumed the queued message. These are observations, not a distributed exactly-once guarantee.
- Original Codex thread `01a14a0b-d94e-7933-9eeb-8f86906fa8e1` was replaced by thread `01a14a0e-75ea-7751-8ab2-7f2de58facd4`, which consumed its own worker reply and completed the native turn. That replacement was then retired before Pi assumed the coordinator role.
- Fresh Pi coordinator session `a634edc2-31de-4707-8613-cdb725641d0c` recovered the earlier completed task ID from the durable inbox; its initial prompt did not contain that task ID. It delegated a recovery check, received exactly one matching native reply, completed an acknowledgement containing the recovery marker, and settled. The original artifact and execution journal remained unchanged. Retired Codex sends and events failed specifically for stale `FORGECTL_RUN_ID` (send exit `2`, event exit `1`).
- The actual Codex sandbox allowed a scratch write and denied an adjacent write. Four configuration checks recorded the same global-config hash and no persisted scratch trust. All five owned native processes exited: two Codex servers with `0`, three Pi processes with `143` after requested termination. No failure or cleanup-failure receipts were present.
- The independent audit checked all 20 required milestones, native reply ordering, fresh-session task discovery, one execution record, and five owned-process exits. Removing the cross-harness roundtrip receipt made that audit fail. Its initial stale-send assertion incorrectly expected exit `1`; inspection confirmed the CLI's stale-send exit `2` with the required diagnostic, and the corrected full audit passed.
- Receipt hashes: `events.jsonl` SHA-256 `fe8dc2d62b31fa51b11eff635ae99229e08ce3bdbdc54c4e667fb90daad6aa7a`; `ACCEPTED` SHA-256 `748c9c80e6a0e2d78c69eb309bfec94c119b7f4707028088bccc996b73e1f553`; worker utility SHA-256 `c66de1076a35ac2ef15dbc8ebb2867b4a7fddbaf6938115470ad46e6ef4b3c9a`.

Codex pushed the tested feature branch and independently verified remote head `fea2e3495f54466ad2c56ce81fc7d09ca448d3c5` before this receipt-only documentation update. No PR, merge, production installation, or deployment was performed.

## Accepted live receipt

On 2026-10-16 at 19:17 America/Chicago (2026-10-17 00:17 UTC), `node scripts/verify-cross-harness-loop.mjs --live ...` exited `0` and wrote `ACCEPTED` in the private run root `/private/tmp/fctl-live.hx3eAL`. The [command reference](../commands/surface-team.md#source-build-verification) describes reproduction. Raw native events remain private because they can contain locally loaded guidance.

- Product build: clean commit `311b5e1fcc17ee3c3dee9821600ff9d64ad64e19`, executable SHA-256 `b226a2772790095439d6fb2cbc73a12ecf22de5c58f6f473d5f1faafbd2d3b79`. Product source was unchanged at verifier commit `5a4e5b9890fc9745a0e6c32574cfc22781be3903`; the verifier SHA-256 is `ae5e69170509e6863c2d357785d06e99b112e304941c6f374575ccddd6ac8683`.
- Native runtimes: Codex `0.153.4` with `gpt-6-astra`; Pi `0.84.4` with direct `openai-codex/gpt-5.6-sol`. Pi loaded the existing estate-auth, cadence-guards, rules, and plan-mode extensions. No OpenRouter traffic was used.
- Task `cross-harness-8e6a2618-35aa-4537-b12d-93c51be21b0e` produced durable delegation, clarification, answer, and completion records in positions `0,1,2,3`. The worker-created JSONL summarizer returned the required final-status counts, and the duplicate-message negative control exited `1` with the expected duplicate-ID error. Codex independently reran both checks after cleanup.
- The intentionally lost acknowledgement reconciled with one proxy delivery, one accepted receipt, and one matching native user message. The worker execution journal contained one execution record. These are observations of this run, not a distributed exactly-once guarantee.
- A message delivered during an active Pi turn reconciled to `sent`. A stopped Pi receiver returned `queued`/exit `75`; restarting at a fresh socket reopened native session `404e6ee8-8a1f-436c-ba4d-7fa7284509f5`, preserved one original delegation, and consumed the queued recovery message.
- The retired coordinator's send and event were rejected. Replacement thread `01a14736-df07-76a2-9553-d3012b48ec37` received the worker reply and completed native turn `01a14738-1dfa-7821-8d6a-32f561e38f13`; it did not reuse original thread `01a14733-de88-75b0-9fe6-4fdf9eaa83ff`.
- The actual Codex sandbox permitted a scratch write and denied an adjacent temporary-directory write. Its global configuration hash stayed unchanged throughout this accepted run. Both owned Codex servers exited `0`; both owned Pi processes exited `143` after requested termination. No cleanup or configuration-check failures were recorded.
- Receipt hashes: `events.jsonl` SHA-256 `430c4dc57b7ca5a7bb4dcef07e1c0d9392c901bbd31dbb75fe7bb53b4582cae8`; `ACCEPTED` SHA-256 `b7eddb06963f0eb9a7e344733796ab4942a91a81ea354de47d1160363c731119`; worker utility SHA-256 `0edfdfac3be728f2412bc39ab24d72aeac614e255e4bbd5e81b4e569eefe5b58`.

The source-build feature branch was pushed and its remote SHA matched `5a4e5b9890fc9745a0e6c32574cfc22781be3903` before this receipt-only documentation update. No PR, merge, production installation, or deployment was performed.

## Development evidence and corrected failures

- Baseline: `go test ./internal/surface/mail` passed on the unmodified base. The first sandbox attempt could not write Go's cache; the permitted retry using the normal cache passed.
- Implementation checks: the focused mail/team/send/inbox/flush/event tests pass, the actual embedded-extension behavioral script passes, and `go build` produces the isolated executable. The repository-pinned golangci-lint 2.13.1 reports `0 issues` with Go 1.26.8; the machine's default Go 1.27 export format is newer than that linter supports.
- A wider CLI run completed with one `TestDeskWatch_ClosedPipeExits141` timeout; its isolated fresh rerun passed. This is not recorded as a fully green wider suite.
- Native Codex probe: installed 0.153.4 completed WebSocket-over-Unix initialization and created an idle ephemeral thread with no model turns; the owned server stopped with exit 0.
- The first live verifier attempt stopped before registration or model turns because plain Pi `--help` exceeded its 30-second preflight timeout. The next preflight loads built-in CLI metadata without unrelated extensions; actual model processes still load the explicit authentication and safety extensions.
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
