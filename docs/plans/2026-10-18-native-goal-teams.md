---
status: in-flight
---

# First-class native teams for repository goals

Status: in flight; Cameron authorized both cleanup stages, existing process-group cleanup first
Next action: verify a real mixed-team result and cancellation using the restored role-scoped execution path, then implement and verify stronger VM/container isolation.
Branch: `codex/cross-harness-loop`

## Goal and authority

Cameron asked Codex to choose one ambitious improvement and make it excellent. Codex chose first-class mixed-harness teams in the self-service repository-goal flow. Cameron explicitly requires Pi and Claude Code, and explicitly authorizes Claude Code's native Anthropic route. Codex and Pi retain explicit OpenAI routes; a missing credential, unsupported model or failed harness never authorizes a provider fallback.

This is one product capability: choose a real team, understand its exact execution boundary, start it without assembling pilot flags, follow its native work, and inspect an independently verified result. A selector without an actual native execution path is not completion. The earlier source-build, worktree and shared-state boundaries remain: commit and push this feature branch; no PR, merge, installation, deployment, global configuration/authentication changes, shared-service restart or third-party source edit. Existing runs and their exact retained bytes remain readable and unchanged.

Cameron subsequently authorized both previously offered paths: "Do both of those .. Do the cleanup stuff first." The referenced first path was retaining the existing cleanup limitation, not deleting session files or retained results. Codex will complete and verify that path first, then add stronger VM/container isolation. This authorizes the staged implementation, not a global installation, shared-service restart, destructive cleanup, or silent fallback from a stronger boundary.

## Current evidence

- Baseline is clean committed `e3ff7e8ca68573c0b8f55cd6d423d373777743f8`. The current one-form interface and its four task presets are complete; task presets are not team presets.
- `internal/surface/goal/run_unix.go` hardcodes concrete Codex sessions, Codex mailbox transport, Codex event envelopes and reviewer-owned command execution. `goalnative` already supplies bounded native RPC, private control state, a lease-owning guardian and proven process-group cleanup.
- The attended Pi pilot proves native collaboration, not portable self-service isolation. Pi's installed security documentation says it has no built-in sandbox. Exposing Pi's default filesystem or shell tools would therefore weaken the current goal boundary.
- The Claude reviewer already demonstrates clean-room settings with no user/project settings sources. A fresh no-model Claude stream-JSON probe established a bidirectional initialization/control channel. Native Anthropic authorization now removes the earlier provider gate; actual model turns and tool isolation remain to be verified.
- The Estate identifies forgectl as the execution owner. Extend its goal supervisor, existing mailbox and native adapters; do not introduce another dispatcher, daemon or shared service.

## Approach and decisions

1. Preserve a simple setup form. Add a distinct Team control and readable coordinator / implementation / reviewer assignments. Provide a useful mixed team containing both Pi and Claude Code, retain the existing Codex team, and allow explicit role/model choices without another wizard. Show the effective harness, provider and model for every role in frozen approval and in the run view.
2. Keep native harnesses native: Codex app-server, Pi RPC and Claude Code stream JSON. Separate logical role, native session identity, prompt acceptance, native consumption, settled completion and independent verification. Keep raw native evidence alongside a small normalized event contract; normalization must not invent stronger evidence than the native transport supplied.
3. Separate model sessions from tool/check execution. Reuse the existing Codex `command/exec` kernel sandbox as a non-model execution host with exact writable roots and network disabled. Pi and Claude expose only explicitly supplied goal tools through their native extension/MCP mechanisms; no ambient plugin, hook, MCP server, built-in shell/filesystem tool or model-controlled escape path may bypass that boundary. An unavailable boundary refuses the run before model work rather than weakening it. The command host is named as an execution dependency, not misrepresented as a fourth model teammate.
4. Use role-bound, private, bounded tool requests. Operator control records, native event evidence, the source checkout and Git metadata stay outside model-writable roots. Changing a tool request cannot choose another role's policy, command host, executable, working directory or write roots. Same-user cooperation is not authentication against a hostile process running as Cameron.
5. Reuse the guardian/lease lifecycle for every native process and its descendants. Detach keeps the supervisor running; cancellation and failure are not terminal-clean until owned process groups and endpoints are proven gone. No PID loaded from disk authorizes a kill; uncertain acceptance never authorizes replay.
6. Keep the mailbox as communication authority. The supervisor may deliver an approved envelope through its owned native connection rather than requiring an unrelated interactive inbox. A message's acceptance, native consumption and completed handling remain separate receipts. Native model replies, not writable mail or a tool result, establish review completion.
7. Preserve version-one retained specs exactly. New team fields are optional/additive and absent fields retain the all-Codex interpretation. Never rewrite an old spec merely to populate defaults, invalidate its goal digest or change old result semantics. New explicit teams are complete, bounded and validated; model/provider changes invalidate preparation.
8. Authentication remains native. Do not copy credential files, put secrets in argv/evidence, silently select a different account/provider, install a translation gateway or alter global settings. Preflight and Start must make missing prerequisites actionable. Metadata discovery must not start a paid model turn.
9. Hermes remains a desirable member of the same native-session boundary, not a separate orchestrator. Pi and Claude are the required acceptance path; do not present Hermes as available merely because an older attended transport exists. Record its exact supported state rather than claiming parity without evidence.

The alternative of launching each harness with its default tools is rejected because Pi has no native sandbox and permission labels are not a filesystem/network boundary. The original no-new-runtime decision is superseded by Cameron's explicit authorization of a second, stronger VM/container stage. Review must establish that native tool disabling and the explicit goal-tool path actually close the loop, not just look correct in generated settings.

### Approved cleanup stages

1. **Existing cleanup first.** Remove only the incompatible nested process-policy wrapper; keep the native Codex filesystem/network sandbox, credential-free execution host, role-bound tools, and owned-process-group guardian. Preparation and approval must disclose that daemonized children can leave those groups and survive cancellation. Cleanup receipts and terminal status must describe group cleanup, not all-descendant containment. Keep the escaped-child probe as an honest, self-expiring limitation test. Verify real allowed/denied operations, ordinary-child cleanup, native mixed-team execution, frozen review, approved checks, and explicit cancellation before moving to the next stage.
2. **Stronger isolation second.** Inspect available local isolation capabilities and existing Estate components, then persist the selected VM/container design before implementing it. The stronger boundary must own tool/check execution and all of its descendants, with no provider credentials, host-control writes, network access, or silent fallback. Verify escaped-child and supervisor-death negative controls against the actual isolated runtime, not merely a process-group receipt. Native model identity, exact worktree/patch/check semantics, and the first-stage results must remain verifiable. Installing or restarting shared infrastructure remains a separate authorization boundary.

The first stage is useful progress, not completion of the newly authorized two-stage objective. If a future runtime is unavailable, report that stage as unavailable rather than claiming its stronger guarantee for the existing cleanup path.

### Native and tool contracts

- A model session exposes identity, submit, wait-for-completion, wait-for-idle and close. It does not expose arbitrary native RPC or execute verification commands. Each submitted prompt has a supervisor request ID and content digest, correlated with the native input/turn identifiers. Delivery reports accepted/consumed separately from the final settled result. One in-flight prompt per role avoids hidden interleaving; the durable mailbox remains the queue.
- Normalized events retain role, run ID, native session ID, supervisor request ID, native message/turn ID, kind, status, exact text or digest, evidence source and settlement. Raw events remain separate evidence. Late, duplicate, missing and cross-session events cannot complete another request.
- The execution-only host has no model session, no `thread/start` or `turn/start`, no model/provider selection, a fresh private configuration home and an environment without provider credentials. Its client allowlist permits only initialization, required read-only capability metadata and command execution. Independent tests must reject model/auth RPC and show that native thread creation never occurred.
- The tool bridge binds policy on the supervisor side to a private per-role capability and run/session identity. Clients cannot supply or replace write roots, command-host identity, source/control locations or executable provenance. Requests are bounded, single-use/correlated and drained or canceled before freezing a result. Broker subprocesses use a separate environment allowlist without native-provider credentials; metadata and error logs never serialize full inherited environments.
- Pi uses `--no-builtin-tools` plus an exact custom-tool list, not `--no-tools` (which also disables custom tools). A new minimal embedded goal extension replaces neither Pi's global configuration nor the existing attended inbox extension. It performs only bounded local-channel requests and exposes no arbitrary Node/process/filesystem API to the model. Native RPC commands are allowlisted; supervisor messages cannot invoke slash commands/templates. Model endpoint, exact active tools and private native session path are rechecked around turns.
- Claude uses supervisor-owned stream-JSON pipes, explicit native session identity, no ambient settings sources, an exact generated MCP configuration, strict MCP selection and no built-in tools. Unresolved permission requests are denied. `subtype: success` alone is insufficient: final acceptance also requires no error, a valid terminal reason, matching session/request evidence and present final text. Native initialization/model/tool observations are retained without account/credential data.
- File helpers reject traversal, protected metadata/environment paths, unsafe links and disallowed source writes before acting. Shell and approved test execution retain the existing disclosed workspace-level kernel boundary and post-execution allowed-path result gate; a literal path allowlist is not misrepresented as an OS per-file policy for arbitrary programs. The frozen patch is explicitly available read-only to the reviewer, with delivery/read evidence retained.
- Process-group absence is not a claim about arbitrary daemonized descendants. Before finalizing cleanup semantics, run an escaped-child negative control against the real execution boundary. Native process, broker and outstanding command ownership must be explicit; any observed surviving execution prevents ready/canceled-clean. Do not widen the older process-group receipt into an unsupported all-descendant guarantee.

## Requirements and acceptance

R1. Cameron MUST be able to select and run a mixed team containing real native Pi and Claude Code from the compiled self-service UI and equivalent JSON CLI. At least one useful repository change MUST complete through native coordination, implementation, independent frozen-patch review, exact approved checks and verified cleanup.

R2. Team selection MUST be independent of Bug fix / Feature / Docs / Custom task presets. Per-role harness/provider/model choices MUST be visible, editable and part of frozen explicit approval. No normal text key, stale response, changed field or duplicate pending action may start or alter an approved team.

R3. Codex, Pi and Claude MUST report observed native session and model identity. Requested configuration alone is insufficient. A mismatch, ambiguous completion, fallback provider, unavailable capability or rejected authentication MUST fail visibly without changing permissions or secretly substituting a harness.

R4. Pi and Claude model tools MUST execute only through the approved role-bound sandbox. Positive and negative controls MUST prove allowed writes work while source/Git/control writes and tool network access fail. Tests MUST attempt direct native-tool, path/symlink, stale-role and cross-role bypasses. The tests must establish what would have made them fail.

R5. Native transport MUST preserve bounded messages, request/turn correlation, exact final text, refusal/error/abort state, cancellation, and no duplicate dispatch under uncertain delivery. Pi idle/agent-end alone and Claude text without a successful correlated result MUST NOT establish completion.

R6. The implementation MUST stop before the final frozen-patch review. The selected native reviewer MUST pass the exact supervisor-computed patch digest directly. Verification MUST use exact approved argv through the independent execution boundary, preserve the patch, and prove the original source unchanged.

R7. Partial startup, native exit, supervisor death, timeout and operator cancellation MUST retain honest state and evidence and clean up only owned processes. Unconfirmed cleanup remains unknown/failed, not ready or safely canceled. Tests MUST exercise real process/lease paths as well as unit fixtures.

R8. Old specs, retained runs, JSON callers and the existing Codex-only workflow MUST continue to work. Old artifacts MUST retain their exact bytes. No global installation, provider configuration, credentials, shared services or primary checkout may be changed.

R9. The compiled UI MUST make prerequisites, effective team, per-role progress/errors and retained evidence legible at 100x24 and 50x16 using ordinary controls. Real PTY probes MUST exercise team editing, approval invalidation, native-run inspection and explicit cancellation without reviving the old wizard.

R10. Delivery MUST include focused unit/adversarial tests, native boundary/process probes, a real mixed-team goal, independent artifact verification, current documentation, and a clean committed build with exact feature-branch remote equality. Passing stubs or a selector screenshot alone is not success.

R11. Codex MUST complete the existing-cleanup stage before implementing the stronger VM/container stage. The final stronger mode MUST demonstrate bounded termination of daemonized tool descendants on cancellation and supervisor death, preserve R1–R10, expose its effective boundary before approval, and refuse execution rather than fall back when that boundary is unavailable. First-stage group-only cleanup MUST NOT be described as satisfying this stronger requirement.

## Work

- [x] Verify current native protocols, observed model selection and tool-disabling mechanisms.
- [x] Resolve the process-containment decision: Cameron authorized existing cleanup first, then stronger VM/container isolation.
- [ ] Complete the existing-cleanup stage with explicit limits and real native acceptance.
- [ ] Design, implement and verify stronger VM/container isolation after the first stage.
- [x] Add validated additive team contracts and native-session/event interfaces with compatibility and negative tests.
- [x] Implement the non-model execution host, role-bound goal tools and guardian-owned native process transport; stronger containment remains unavailable, not silently bypassed.
- [x] Implement and verify real Pi and Claude adapters without ambient tools, fallback providers or copied credentials.
- [x] Integrate team-aware mailbox delivery, native evidence, independent review and checks into the existing supervisor.
- [x] Add team selection, per-role approval/progress and actionable preflight to the one-form UI and JSON CLI.
- [ ] Verify adversarial boundaries, lifecycle failures and the wide/narrow terminal journey.
- [ ] Run a real mixed-team repository goal, independently verify its result and preserve prior evidence.
- [ ] Complete docs, clean-build provenance, feature-branch push and requirement-by-requirement audit.

## Ownership and review

Root Codex owns the integrated contract, plan and delivery. The existing Pi and Claude research subagents subsequently received bounded adapter, UI, test and file-helper implementation assignments. Neither commits. Root Codex independently inspects their changes and runs the focused tests, native probes and compiled UI journey. The separate process-control research and final boundary review remained read-only.

The Pi and Claude reviewers completed independent read-only plan/security reviews against baseline `e3ff7e8c` and this plan. Root Codex independently confirmed the concrete Codex-only seams, Pi tool-flag distinction, the default environment's credential exposure, the guardian's process-group-only proof, and the recorded Claude control frames. Supported findings are incorporated into the contracts above and their tests. The claim that the existing reviewer cannot read the frozen patch confused writable roots with readable roots; it is not an existing failure, but the new bounded file helper must deliberately expose that read-only artifact. Neither same-user socket identity nor a digest echoed in model prose is cryptographic authentication. These reviews do not establish runtime safety; the native probes and mixed-team acceptance remain required.

## Completion evidence

Not complete. The additive team contract preserves exact retained version-one JSON bytes and invalidates approval on explicit team changes. Normal goal/native/tool-channel/UI tests and the affected CLI tests pass; real execution-boundary tests now pass after the staged corrections recorded below. The actual mixed-team result and UI cancellation remain unverified. The menu-inventory test `TestMenuJSON_LiveTree` fails on an unchanged archive of baseline `e3ff7e8c` for the same two pre-existing project-list rows; this task does not change those commands.

### Verified checkpoint

- Native Codex startup and the actual outbound model tool inventory pass `FORGECTL_GOALNATIVE_INTEGRATION=1 go test ./internal/surface/goalnative -run '^TestCodex' -v -count=1`. The inventory test uses a local, credential-free mock model endpoint, not a paid external model call. The actual inventory contains the five approved goal tools plus native request-user-input, which the supervisor denies.
- Real Pi `openai-codex/gpt-6.1-sol` and Claude Code `anthropic/claude-opus-5-5` each completed one harmless `goal_inbox` request and an exact final reply. `FORGECTL_NATIVE_REPLY_INTEGRATION=1 FORGECTL_NATIVE_SUPERVISOR=/private/tmp/forgectl-native-teams-dev go test ./internal/surface/goalnative -run '^TestNativeRealReplyWithHarmlessBroker$' -v -count=1` passed. The probe broker refuses all filesystem/command tools. Independent inspection confirmed matching accepted/consumed/completed identities, final native IDs, and paired guardian receipts with the owned group and socket gone. Artifacts: `/private/tmp/fg-native-reply-61760395` and `/private/tmp/fg-native-reply-2970942894`.
- The embedded Pi extension executes all five shared tool schemas against the actual private broker in a Node-backed contract test. Claude's exact MCP allowlist is tested and observed. Completion observers now finish before successful wait/idle returns; regressions block the observer to exercise the red path.
- Client cancellation reaches the broker handler, and broker close drains outstanding requests before patch freezing. Capability reads bind to the checked private directory and file. The file helper runs through the same sandbox executor, pins parent descriptors, refuses links, atomically replaces content and preserves existing executable modes. Direct compiled-helper positive/negative evidence is retained at `/private/tmp/forgectl-file-helper-current.jTrnU4`.
- Compiled PTY journeys at 100x24 and 50x16 verify team selection, exact role edits, full role values in approval, invalidation after edits, and no launch from ordinary text keys. The narrow role-triple clipping found by the first probe is fixed. Root Codex reran `/private/tmp/probe-team-ui.py` and read every returned assertion; artifacts are in `/private/tmp/forgectl-ui-team-artifacts`. These are prepare-only journeys, not an actual mixed run or its cancellation.
- The review's claim that a separate run/session string would authenticate the private tool channel is not adopted: each capability and endpoint is fresh and immutable for one role incarnation, and closing it invalidates stale clients. Mailbox delivery separately validates run, native session and harness. Public identity strings cannot strengthen a stolen same-user capability. Cross-role, stale-channel and wrong-incarnation negative controls cover the actual contract.

### Unresolved execution boundary

The following nested-policy failure describes the earlier checkpoint, not the restored first-stage implementation below.

The real escaped-child negative control found an existing limitation: a child calling `setsid` survives command termination and native process-group cleanup until its self-expiry deadline. The guardian receipt establishes only process-group shutdown, not all-descendant cleanup. The original credential-free command host did pass allowed-write, denied-write and private-socket negative controls, but those earlier results do not establish the stronger process property.

A deny-only macOS profile blocks `setsid`, `setpgid` and `posix_spawn` process-group bypasses, but it cannot be nested inside the existing native Seatbelt sandbox. The current integrated command host therefore refuses even its harmless allowed-command probe: `FORGECTL_GOALNATIVE_INTEGRATION=1 go test ./internal/surface/goalnative -run '^TestExecutionHostRealBoundaryAndNoModel$' -v -count=1` fails with exit 71 and `sandbox_apply: Operation not permitted`. Reversing the profile order also fails; the inspected installed app-server schema offers no supported additional syscall policy. No `dangerFullAccess`, external-sandbox substitution, custom filesystem profile or global change was made.

The earlier decision wait is resolved: Cameron authorized both paths, existing cleanup first. The failed nested-policy experiment remains evidence, not the selected first-stage implementation. The useful repository-change acceptance run, native run inspection/cancellation, complete lifecycle proof, final clean-build delivery and R1/R4/R6/R7/R9/R10/R11 audit remain required. Harmless native replies and passing unit tests are not substitutes for them.

### First-stage execution restored

Codex removed the incompatible nested policy and its experimental helpers, preserving the native execution sandbox, role/root checks, credential-free host and guardian. Preparation now explicitly discloses the daemonized-child limitation, and the UI describes confirmed owned-process-group cleanup rather than generic cleanup. Invalid/empty/NUL argv remain refused. The explicit-team model prompt now instructs every harness to use the five broker tools; its stale instruction to use Codex's disabled native shell/apply-patch path was caught by a failing regression before correction. The legacy-team prompt remains unchanged.

`FORGECTL_GOALNATIVE_INTEGRATION=1 go test ./internal/surface/goalnative -run '^TestExecutionHostReal|^TestGoalNativeRealCodexAbruptSupervisorDeath' -v -count=1 -timeout=3m` passes: allowed writes succeed; protected writes, private Unix sockets and TCP access fail against live positive controls; the execution-only host creates no model session; logical-owner death stops its native group and ordinary shell/child; and the self-expiring escaped-child probe demonstrates the disclosed limitation. The socket denial assertion was corrected to match Node's actual `connect EPERM` rather than an unrelated prose rendering of that same error.

The independent read-only cleanup review's claimed broker-socket leak was not adopted: Go's owned Unix listener unlinks on close, and the capability file is retained separately under native-control, not in the socket directory. Root Codex verified the installed standard-library implementation and added a passing actual listener/close/socket-absence/empty-directory-removal test. Killing the guardian itself is distinct from killing the logical supervisor: an earlier misnamed test targeted the guardian and did not establish owner-death cleanup. The corrected owner-death test uses the real parent/helper lease path; guardian failure and stronger all-descendant containment remain separate concerns, not hidden green claims.

All affected goaltools/native/goal/TUI package tests pass, and repository-pinned golangci-lint 2.13.1 reports zero new findings against baseline `e3ff7e8c`. After the model-prompt correction, the affected goal package and lint were rerun successfully. A useful real mixed-team goal, actual UI cancellation, artifact/source verification and the second isolation stage remain outstanding.

### Role-scoped execution correction

The first actual mixed-team goal at clean public revision `5b9e91a9` refused before model work: the coordinator boundary probe could write the implementation worktree. The native execution sandbox implicitly included its startup working directory despite narrower per-command roots. This was a real isolation defect in the shared-host wiring, not a credential/provider failure. The failed goal and its receipts remain retained at `/private/tmp/forgectl-native-stage1.ygFzB6/registry/goals/goal-486f781ebbcfb7a88bfb25b5`; its source clone remains clean.

Codex replaced the shared command host with one credential-free host per role, bound both startup and command working directories to approved roots, and kept coordinator/reviewer tool commands in their private runtime directories. The reviewer host's wider check ceiling is available only to the supervisor's approved-check path. The implementation execution host now also closes before patch freezing. A regression failed against the earlier permissive host; the real no-model role matrix proves each role can write its own directory, cannot write another role's directory, and the reviewer can write the implementation directory only with explicit supervisor check roots. Full affected package tests, the real boundary/owner-death suite and pinned lint pass. After changing the test's file reads to scoped directory handles, the affected real-role test and lint were rerun successfully. The actual mixed-team result and UI cancellation are still required.
