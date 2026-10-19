---
status: done
---

# First-class native teams for repository goals

Status: implemented, stabilized and verified; operator and recovery runbooks published on the feature branch
Next action: Cameron decides whether to open a PR for the stabilized feature branch. No PR, feature merge, deployment or global installation is included without separate authorization.
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

### Second-stage container design

Codex selects a local Docker Engine execution boundary, using the already-running Colima Linux VM on this machine. Read-only discovery confirmed its local Unix socket, Linux ARM64 engine, cgroup v2, seccomp, memory and PID limits. The Estate capability search found the existing forgectl execution owner, not another reusable container executor. This selection introduces no shared daemon, service restart, global installation, credential copy or model-provider change. Container creation and an explicitly built private test image are scoped acceptance work; existing containers, images and retained goals remain untouched.

- **Explicit approval.** Add an optional `isolation` object with `mode: "container"`, an absolute local Unix `socket`, and an immutable `sha256:` image ID. Absence preserves the existing process-group mode and exact old JSON bytes. The simple form exposes the mode and its prerequisites; advanced fields specify the socket and image. Preparation, frozen approval, run details, JSON and native role instructions name Linux command semantics, the pinned image, network denial and the stronger tool-descendant scope. Container mode requires an explicit native team, including for an all-Codex choice, so no legacy built-in tool path can bypass it.
- **Trusted image prerequisite.** Ship a small build recipe containing a Linux forgectl executable at a fixed path, Git, Python and a POSIX shell. Users can build a different trusted tool image for their checks, but must approve its immutable local ID. Runtime never pulls, builds or substitutes an image. Preflight verifies the local engine, image architecture and exact helper protocol before any model work. Reject image-declared volumes, unsupported environment defaults and conflicting runtime policy rather than inheriting them. Toolchain availability and Linux portability are visible prerequisites, not host-command fallbacks.
- **One container per operation.** A narrow standard-library Docker client talks only to the approved Unix socket, with no Docker credential/config loading or TCP endpoint. Each file helper, model command and approved check receives a fresh container. Mount only the implementation worktree, exact required Git directories, that role's runtime directory and the frozen patch when present. The source checkout itself is not mounted; its dirty/untracked files are not exposed. The worktree is read-only unless the role/request explicitly permits writes; Git directories and the separate worktree `.git` file are always read-only. The role runtime is writable; the patch is a single read-only file, never its control directory. Reject unsafe/symlinked or overlapping roots, other than the explicit `.git` overlay and a Git directory contained in its read-only common directory. Use private, non-recursive binds so nested host mounts are not imported. Host home, provider credentials, broker/control sockets and the daemon socket are never mounted. Preserve approved host absolute repository paths inside the container so check argv and Git worktree links retain their meaning. A typed supervisor file operation selects the fixed Linux helper path; arbitrary command argv are not rewritten. Calling that executable manually gives no authority beyond the same role mounts, just as an arbitrary shell program already has workspace-level authority and must pass the post-execution allowed-path gate.
- **Runtime policy.** Use a private PID namespace, no network, read-only root filesystem, bounded private `/tmp`, no published ports/devices, no restart or health check, no privileged/host namespace, `no-new-privileges`, default seccomp, and explicit CPU/memory/PID limits. A small trusted PID 1 runs as container root with only SETUID/SETGID capabilities so it can launch the actual command as the caller's nonzero UID/GID with no supplementary groups or capabilities. The workload cannot access PID 1's stdin or manufacture its lease. All workload execution is non-root; privileged init is limited to the fixed helper and cannot be selected by a model.
- **Bounded lease, not CLI ownership.** Attach a private stdin stream before starting the container. The host sends bounded heartbeats; PID 1 exits on EOF, missing heartbeats, a hard absolute startup deadline converted once to a monotonic timer, or command completion. Launch the workload with `/dev/null` stdin, only its standard output/error descriptors, empty supplementary groups, nonzero UID/GID and no effective/permitted/inheritable/ambient capabilities. Linux terminates all processes in that PID namespace when PID 1 exits, including double-forked and `setsid` descendants. A dropped/stuck stream or abrupt logical-supervisor death therefore cannot extend execution indefinitely. No assumption about `docker run` client death substitutes for this protocol. Real tests must attempt workload access to `/proc/1/fd/0`, stdin spoofing and SIGSTOP before accepting the design.
- **Owned cleanup and evidence.** Capture a fresh nonce/name before create and the full container ID after create; verify labels, image and effective policy before start. Do not retry uncertain create/start as a new execution. Close the lease, request stop/kill only for the verified owned container and inspect its stopped state before recording success; remove only that owned ephemeral container after confirmation. Retain per-operation startup/finish receipts with requested/effective policy, image/container identity and cleanup evidence under private native-control. Unknown cleanup poisons the host and prevents ready/canceled-clean. An interrupted supervisor cannot fabricate a finish receipt; later status remains honest even when runtime inspection proves the workload is gone. Existing native model processes retain their separate process-group receipts and limitation; the stronger all-descendant claim concerns tool/check execution only.
- **Integration.** Generalize the role execution-host seam to `Exec` plus `Close`, retaining the current Codex command host unchanged for old mode. The container host serializes operations, binds its role ceiling and refuses use after close or uncertain cleanup. Reviewer model commands use read-only worktree mounts; only exact supervisor-approved checks request writable worktree roots. Implementation tools and execution host must close before freezing, just as today. The same broker/file-helper gates and source/patch/check verification remain authoritative.
- **Acceptance.** First prove actual container lifecycle with allowed writes and existing denied targets, network/socket exclusion, non-root/no-capability execution, protected Git/control/source writes, immutable image matching, and lease EOF/stall/owner-death/forced PID-1 death. A self-expiring double-forked process must demonstrably exist before cancellation and be gone afterward; process-group absence alone is insufficient. Then add mock protocol/error/uncertain-create controls, run an actual native mixed-team repository goal through the new boundary, independently verify frozen review/check/source/receipts, and exercise compiled UI approval/start/cancel/inspection at both terminal sizes. Preserve and recheck older group-mode artifacts without rewriting them.

Before create, persist `creating` with the generated name, nonce and exact configuration digest. Capture and verify the full returned ID, or inspect that exact name after an uncertain response; a mismatch or unavailable inspection becomes `unknown`, never a fresh create. Persist `created` before attach and `starting` before start. After an uncertain start, close the lease and inspect/stop the same verified identity; never replay the command. Persist `stopped` only after runtime inspection confirms PID zero/not-running, then remove that verified owned container and record `removed`. Failure to confirm any potentially started operation poisons the executor and prevents successful terminal cleanup. Bound every API response, attached stream, call deadline and cleanup attempt. Effective policy and intermediate evidence remain retained even when a side effect or final receipt fails.

The read-only independent plan/security/operability review at `2f5227fc` identified missing mount topology, existing probes' incompatible error expectations, uncertain-create handling, workload descriptor/group controls and Colima path sharing. Codex confirmed those seams in source and incorporated them here. The proposed helper-authentication blocker did not identify authority beyond the already disclosed workspace-level arbitrary-command boundary; a typed invocation avoids accidental rewriting without pretending the image executable is secret. Container probes must accept actual `EROFS` for existing read-only targets and explicitly verify non-mounted control paths rather than passing on a missing file. Disable global/system Git configuration, credential interaction and optional locks inside the workload; repository Git metadata remains read-only and the selected repository/image/runtime are trusted prerequisites.

The actual private Linux prototype now verifies the selected lease mechanism: EOF, a stalled heartbeat, explicit owned-container kill, PID-1 kill and SIGKILL of the logical host owner all stopped an observed, double-forked, reparented `setsid` daemon. The stalled stream terminated in 3.061 seconds; the other cases completed in 0.115 seconds or less. Its non-root child had zero effective capabilities and `NoNewPrivs=1`; lease-descriptor access failed with EACCES, SIGSTOP with EPERM, an existing readable protected file rejected writes with EROFS, and networking failed with `Network unreachable`. The first probe correctly refused `/private/tmp`, which Colima does not share; rerunning in the existing project share succeeded without changing Colima. Startup preflight must therefore exercise actual path-sharing and UID/GID access, not just host `stat`. All owned prototype containers were removed and the existing eleven running services remained present. Probe code is `/private/tmp/forgectl-container-lease.zALJHc`; final evidence is in the ignored `.claude/worktrees/container-lease-probe` directory of this feature worktree. This is design evidence, not acceptance of product code that has not yet been implemented.

A failed product probe changes the implementation or leaves container mode unavailable; it never downgrades to host execution. The intended scope is a local trusted runtime and image, not protection from a hostile Docker daemon, kernel exploit or another process already running as Cameron.

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
- [x] Complete the existing-cleanup stage with explicit limits and real native acceptance.
- [x] Design and implement stronger VM/container isolation after the first stage, including actual runtime boundary and descendant-lifecycle tests.
- [x] Verify the stronger mode with clean-build provenance, a useful native mixed-team result and compiled UI acceptance.
- [x] Add validated additive team contracts and native-session/event interfaces with compatibility and negative tests.
- [x] Implement the non-model execution hosts, role-bound goal tools and guardian-owned native process transport; unavailable boundaries refuse execution rather than being bypassed.
- [x] Implement and verify real Pi and Claude adapters without ambient tools, fallback providers or copied credentials.
- [x] Integrate team-aware mailbox delivery, native evidence, independent review and checks into the existing supervisor.
- [x] Add team selection, per-role approval/progress and actionable preflight to the one-form UI and JSON CLI.
- [x] Verify both stages' adversarial boundaries, logical-owner failure and the wide/narrow terminal journey; preserve the native-model guardian limitation separately from containerized tool containment.
- [x] Run a real mixed-team repository goal, independently verify its result and preserve prior evidence.
- [x] Complete docs, clean-build provenance, feature-branch push and requirement-by-requirement audit.

## Ownership and review

Root Codex owns the integrated contract, plan and delivery. The existing Pi and Claude research subagents subsequently received bounded adapter, UI, test and file-helper implementation assignments. Neither commits. Root Codex independently inspects their changes and runs the focused tests, native probes and compiled UI journey. The separate process-control research and final boundary review remained read-only.

The Pi and Claude reviewers completed independent read-only plan/security reviews against baseline `e3ff7e8c` and this plan. Root Codex independently confirmed the concrete Codex-only seams, Pi tool-flag distinction, the default environment's credential exposure, the guardian's process-group-only proof, and the recorded Claude control frames. Supported findings are incorporated into the contracts above and their tests. The claim that the existing reviewer cannot read the frozen patch confused writable roots with readable roots; it is not an existing failure, but the new bounded file helper must deliberately expose that read-only artifact. Neither same-user socket identity nor a digest echoed in model prose is cryptographic authentication. These reviews do not establish runtime safety; the native probes and mixed-team acceptance remain required.

## Completion evidence

Both cleanup stages and the final review-sequence correction have independently verified native and UI evidence. The additive team and isolation contracts preserve exact retained version-one JSON bytes and invalidate approval on explicit boundary changes. Normal goal/native/tool-channel/container/UI tests and the affected CLI tests pass; real execution-boundary tests pass after the staged corrections recorded below. The chronological checkpoints retain earlier failures and outstanding-work statements as history; the final requirement audit records the completed state. The menu-inventory test `TestMenuJSON_LiveTree` fails on an unchanged archive of baseline `e3ff7e8c` for the same two pre-existing project-list rows; this task does not change those commands.

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

### Native Codex dispatch correction

The next clean-build goal at `b3f8a864` started all three native roles with observed approved identities, but Codex could not dispatch its first assignment: the selected model's native metadata specifies `tool_mode=code_mode_only`, while the integration had disabled the code-mode host. Codex requested operator help instead of falsely claiming dispatch; Codex canceled the run through the goal CLI and verified `canceled`, inactive supervision and owned-group cleanup. Retained evidence is under `/private/tmp/forgectl-native-stage1-role.YIXSsB/registry/goals/goal-50df079679cde3c11a6f1d98`. No implementation change was produced.

The previous mock inventory exercised ordinary function tools, not this model-mode branch. A new local, credential-free mock drives the actual installed native runtime through code-mode-only dispatch and a real private broker. It failed with zero broker calls against the disabled host, then passed after enabling only the native JavaScript dispatcher while keeping shell, filesystem, browser, network tools and ambient integrations disabled. The probe requires the exact five nested goal tools, no Node/process/require/fetch/native execution API, rejected built-in/local-file/network module imports against existing positive fixtures, one correlated broker call, and returned broker output. The dispatcher is not a replacement filesystem sandbox or a provider fallback. The child host validator now independently rejects an execution startup working directory outside its ceiling, in addition to the parent and per-command checks.

### Native transcript creation privacy

The clean-build goal at `2a0500be` successfully used Codex's broker to queue its assignment, but refused the next Pi prompt: Pi created its lazy native transcript with inherited mode `0644`, while the adapter correctly requires an owner-private regular file. The initial harmless one-turn probe never refreshed state after that file appeared. The failed goal, unconfirmed delivery record and all cleanup receipts remain retained under `/private/tmp/forgectl-native-stage1-dispatch.mOKfbN/registry/goals/goal-4ea12b7e26c2fdb4a61f80fb`; it produced no verified result and was not replayed.

Codex now sets `umask 0077` inside the dedicated guardian child before native startup, leaving the caller and existing files unchanged. The strict Pi validator remains intact. A real guardian test explicitly inherits `0022` and failed on a newly created `0644` transcript before the fix; it now requires `0600` files and `0700` directories. The optional paid native probe now requires two separately correlated broker turns from both Pi and Claude. Full native package tests, the actual no-model sandbox/role/code-mode/owner-death suite, the privacy regression and repository-pinned lint pass. Two-turn paid evidence and useful mixed-team acceptance remain outstanding.

The clean `6354e038` build subsequently passed both two-turn native probes. Root Codex independently verified distinct request/native completion IDs, the Pi transcript's `0600` mode, and paired guardian cleanup receipts in `/private/tmp/fg-native-reply-3152877023` and `/private/tmp/fg-native-reply-4272891566`. The read-only review found no blocking issue; the shared 90-second test deadline is a deliberate total bound, not a per-turn allowance.

### Cancellation visibility correction

The compiled 100x24 UI successfully prepared and started a real mixed goal, observed all three native identities, and reached its requested operator question. The cancellation probe then exposed an actual UI defect: after manual scrolling, opening cancellation reset the offset but left manual-scroll mode enabled, hiding the confirmation below the overview. The probe also used the wrong confirm label (`Cancel goal` rather than `Request cancellation`). Its recovery queued CLI cancellation; the retained run `goal-a34f7f873a8f043154a54e94` under `/private/tmp/forgectl-native-stage1-private.k8P6Gw/ui-registry-100x24` reached canceled with inactive supervision and owned-group cleanup. That recovery is not UI-cancellation acceptance.

Opening cancellation now restores automatic focus scrolling, and its warning/buttons wrap on narrow terminals. A regression reproduces the prior invisible controls at both 100x24 and 50x16, then requires both choices, the selected focus, pinned goal identity and bounded line width. Focused project-goal UI tests and pinned lint pass. Fresh actual UI cancellation remains required.

### First-stage acceptance

- A real useful JSON-CLI goal, `goal-ef521e2bf81e09761be68195`, completed on clean public source `6354e038` with observed Codex/OpenAI coordination, Pi/OpenAI-Codex implementation and Claude/Anthropic review. The retained root is `/private/tmp/forgectl-native-stage1-private.k8P6Gw/registry`. The only result file is `docs/commands/surface-project.md`.
- Root Codex independently verified the exact frozen patch `b7c0f295934a5e0668d04f649586e0d0f0f2b65de8cf2f7eed19bd7d17e957d1`, the directly correlated native reviewer response, both approved argv/output digests, unchanged post-check patch, original source snapshot and six paired guardian receipts. Read-only process inventory confirms those owned groups are absent; implementation model and execution-host receipts precede patch freezing. The CLI artifact bytes match disk. This remains group-only cleanup, not all-descendant containment.
- The reviewer correctly distinguished the preliminary abbreviated Git diff digest from the final full-index patch digest. Only the latter direct native review counted. Root Codex inspected the actual patch and implementation, then incorporated the useful documentation with one explicit correction: failed native startup can also publish a failed receipt. The original retained patch and review remain untouched; that small documentation clarification is root Codex's change, not falsely attributed to the frozen review.
- Fresh compiled UI `e0902123`, independently built with `vcs.modified=false`, started and canceled real mixed teams at 50x16 (`goal-be9a3d67ffcefc9a76c998b6`) and 100x24 (`goal-c706a05894fc14c94d214ccd`). Each first confirmed Keep running leaves the run active, then used the explicit Request cancellation button; all six paired receipts, unchanged retained spec and clean source were verified. Fresh retained-run views also visibly show canceled state, each role and owned-process-group cleanup. Evidence is under `/private/tmp/forgectl-native-stage1-ui.Kw10cB`.
- The completed result's Diff, every check output and JSON are visible at both terminal sizes with stable goal identity. The first narrow probe incorrectly rejected a 238-character stderr line split across page boundaries by pinned headers. Its regression-tested fix scans check output one row at a time; the actual unchanged UI now passes every assertion. The deliberate visible-Status negative control exits 1 with the expected error at both sizes. Reports: `ready-inspection-fixed/1792427502738358000-33294/report.json` and `ready-inspection-negative/1792427551517096000-41150/report.json` under the UI evidence root.

Both approved first-stage paths are real, not fixtures: JSON-CLI completion and compiled UI Start/cancellation. Native teams remain experimental; preparation now asks for retained review/check/cleanup inspection rather than claiming the tested path has never run. The second stage starts from these retained results without rewriting them or weakening their limits.

### Second-stage implementation checkpoint

Codex implemented the optional immutable-image/local-socket contract, typed Linux file helper, per-role execution-host selection, bounded Docker API client, one-operation container policy, private immutable receipt sequence and root-init/non-root-workload lease. The native harnesses, providers, credential handling, broker authorization and frozen-result checks remain unchanged. The simple UI exposes isolation separately from task/team presets, makes an all-Codex container team explicit, invalidates approval on boundary edits, and renders the exact image/socket plus separate native-model cleanup scope. Documentation includes the image recipe and distinguishes metadata-only preparation from actual helper/mount preflight after Start.

Actual product tests used the existing local Colima socket and explicitly built development image `sha256:9023bdfa7dcd29ac8b0ca5690cd26228cdbebef6e166d9cf7ff2439ea5aa1b34`. They prove file-helper read/write, non-root UID/GID with empty groups, inaccessible init stdin, workload `/dev/null` stdin, denied init SIGSTOP, denied existing Git metadata/root writes, absent control/socket mounts, denied network, paired removed receipts and actual runtime absence. A live double-forked, reparented `setsid` daemon was observed before each cancellation/fault. Cancellation, EOF, held-open stalled heartbeat, forced PID-1 death and abrupt logical-owner SIGKILL all terminate its namespace and heartbeat; the stalled case takes approximately 3.36 seconds, including the final 300 ms stable-heartbeat observation. The abrupt-owner test deliberately retains the missing final production receipt and records a separate observed stopped state instead of fabricating clean terminal evidence.

`FORGECTL_CONTAINER_INTEGRATION=1 go test -p=2 ./internal/surface/goalcontainer ./internal/surface/goal -run '^TestContainer' -v -count=1 -timeout=3m` passes with the explicit socket/image/shared-test-root environment. Latest retained evidence is under the ignored `.claude/worktrees/container-lease-probe` directory: container proofs `21782694`, `162303216`, `637174311`, `3677031325`, `1283094095`, `4232168421`, and three-role goal preflight `goal-boundary-proof-4188190077`. This tests the development artifact, not the still-required clean committed native acceptance image.

Independent read-only review found two real state bugs. A cancellation combined with unknown cleanup originally allowed a second create; the regression observed two creates for both cancellation and deadline errors. Any cleanup error now poisons the host irrespective of cancellation. Post-stream inspection originally accepted `created`, empty or inconsistent non-running state; it now requires an observed `exited` state with consistent terminal flags. The reviewer inspected the corrections and independently reran normal container/goal tests without finding another concrete blocker.

Root Codex also reproduced a receipt-write failure preventing the owned stop request (`kill=0`, workload still running). Cleanup now continues verified-owned termination, confirms stopped state, preserves the container when evidence cannot be recorded, and returns unconfirmed rather than claiming success. Each immutable receipt syncs its file and parent directory. The actual three-role startup probe initially failed because shell redirections attempted the protected file before opening stderr, and a special builtin could terminate the shell. Its corrected ordinary `printf` opens stderr first; actual coordinator/implementation/reviewer probes now pass. Typed file-operation tests prove failed container helpers do not fall back to a host command or rewrite arbitrary model argv. The two narrow G204 lint exceptions cover only the fixed Linux init's intentionally dynamic, privilege-dropped workload launch and the test's own executable child; they do not exempt general command execution.

Final clean-build provenance, native mixed-team execution, source/frozen-review/check/container-receipt verification and compiled UI acceptance remain outstanding. No shared runtime was restarted, no global tool/provider configuration changed, and no existing container or retained run was removed.

### Second-stage acceptance

The clean source clone at `c76423bc576d20bd5a5d8389887c532bc801cd3c` produced a host binary with matching revision and `vcs.modified=false`. Building the committed `Dockerfile.goal-tools` from that same clean source produced immutable local image `sha256:c7e5f19ef549553b9387aac9363b57624060d5213dc6f02ec4e7715e2a5c3b58`. The clean-image `^TestContainerReal` suite passes, including actual file/boundary denial, three-role preflight, cancellation, lease EOF, stalled heartbeat, PID-1 death and logical-owner death. Evidence, build logs and image inspection are retained in the ignored worktree sibling `native-container-acceptance.onqFuO` (the acceptance root below); this is not the earlier development image.

The useful goal `goal-15005c323966829e69e7ce27` reached ready with observed Codex/OpenAI, Pi/OpenAI-Codex and Claude/Anthropic identities, an independent direct frozen review and both approved checks. Root Codex's read-only `audit-evidence.py` verifies patch `7b4c368689c363b69239386a03aea285a374bceee628326afeec2ef41bd6df40`, exact CLI/disk artifacts and argv/output digests, unchanged source, implementation shutdown before freeze, three paired native guardian receipts and all 48 container operations' identity/policy/removed receipt sequences. Fresh local Engine GET requests return 404 for every exact owned container ID. An initial private auditor mismatch was Python-versus-Go JSON HTML escaping of `>`; the corrected calculation agrees with an independent Go standard-library encoder. No check or product guarantee was relaxed.

The native team found and corrected documentation errors before its preliminary review. It then requested a frozen patch too early, so the coordinator asked the operator instead of inventing acceptance. Root Codex answered that exact question with the implemented two-stage protocol; after all roles became idle, the supervisor stopped implementation, froze the real patch, obtained the separately correlated exact-digest native review and ran both approved checks. The result therefore includes one transparent operator clarification, not an unattended-success claim. Root Codex added the same lifecycle distinction to the initial native contract; its phrase regression failed before the fix, then the full goal package and pinned lint passed. Independent read-only review confirms the paragraph preserves the mandatory later direct review, checks and source snapshot. A final clean native run will verify that correction.

Root Codex inspected the useful diagnostic patch against the actual metadata/helper/mount code and incorporated it unchanged. The adjacent removal of the temporary no-final-acceptance caveat is Codex's separate documentation change; the retained native patch/review remain immutable.

Compiled clean-build UI Start/cancellation passes at 50x16 (`goal-935419eee4eb9d0662c1240d`) and 100x24 (`goal-485bb356cd0369431ae874b3`). Each uses the explicit Linux-container selection, exact socket/image and three native assignments; ordinary text keys do not start, Keep running does not request cancellation, and Request cancellation reaches canceled with inactive supervision. Each run retains three native guardian pairs and eight removed container sequences; fresh runtime absence, source/spec preservation and private receipt modes are verified. Retained canceled views show the boundary and cleanup scope at both sizes. The completed result's Diff, every check output and JSON also pass all assertions at both sizes. Evidence is retained under the acceptance root's `ui-cancel-*`, `ui-canceled-*` and `ready-inspection/1792431825942312000-79392` directories.

The clean container binary also re-audits the first-stage result without changing its original source, spec, patch or receipts: six native/execution guardian pairs, the original `b7c0f295...` patch and exact native review/check evidence remain valid. The stronger runtime does not retroactively relabel older group-only cleanup.

### Final native acceptance and delivery

Clean committed source `21fcaf44811ddf0c86178fadfe83c9b812686f74` produced a matching `vcs.modified=false` host binary. The helper image remains the explicitly approved `c7e5f19e...` image: its helper/container source is unchanged by the review-contract correction. The current-source real container suite passed again against that exact image, including all three roles, observed escaped descendants, cancellation, EOF, stalled lease, PID-1 death and logical-owner death. Current-source native boundary/code-mode/role/owner-death probes also pass, including the honest group-cleanup limitation probe.

The final useful goal `goal-b9868c32b582afd9aad83bbb` started at 17:49:26Z and reached ready at 17:56:22Z, with no questions or operator intervention. Its frozen patch is `e30a37c8fcf933d622a2f2c90c918391fcb70195d437559383455d9a2f490870`. Root Codex independently verified all three observed native identities, the direct reviewer response, both exact approved checks and output hashes, the unchanged frozen patch/source, implementation shutdown before freeze, three guardian pairs and all 43 owned container sequences with fresh runtime 404 responses. Private auditor negative controls reject missing cleanup, wrong review/argv/spec identity, a missing native role and a still-present container. The original approved Python check's next-heading delimiter could include the following level-two section; a separate strict heading-boundary assertion confirms every required term is actually inside the intended new subsection. No approved check or retained artifact was changed.

The final frozen review honestly limited its source inspection to the documentation and brief. Root Codex separately inspected `coordinate` and `verify`, then incorporated the exact 17-line documentation hunk. Root Codex also corrected older result-section claims that still called container isolation future work and incorrectly described every mode as using a Codex command host. Those corrections are separate from the native-reviewed patch. The retained result stays unchanged and `human_accepted` remains false.

Final compiled UI inspection passes every assertion at 100x24 and 50x16: stable goal identity, real patch content, all check headings/outputs, scrolling and JSON. Its report is `ready-inspection/1792432621996094000-97739/report.json` under the ignored sibling `native-container-final.jBzskG`. The earlier visible-Status negative control correctly exits 1 with only `negative_absent` false at both sizes; its retained report is `ready-inspection-negative/1792431947458427000-87459/report.json` under `native-container-acceptance.onqFuO`.

The current binary also verifies the original version-one all-Codex goal `goal-e0f2b16b582fdecd0ae9b546`, not just the newer explicit-team run: absent team/isolation fields, original spec bytes and digest, patch `a4591283fd543c0a4b0b008fbaf9fbea549703e2fef0ae1778435de26ca635a2`, directly correlated original reviewer event, both check artifacts/commands, unchanged source and all three absent owned groups. Its new separate receipt is `legacy-codex-audit.json` under the final evidence root; the original evidence was not rewritten.

Current-source validation is retained in `final-package-tests.log` (full goalcontainer, goalnative, goaltools, goal and TUI packages), `final-cli-tests.log` (project-goal CLI tests), `final-native-boundary-tests.log`, `final-container-tests.log`, `final-lint.log` and `final-linux-lint.log`. Both pinned golangci-lint 2.13.1 runs report zero issues. An initial sandbox attempt failed at Unix socket/PTY creation and the host-level rerun passed; a separate mistaken `./cli` package path was corrected to the actual `./internal/cli`. Neither environmental/verifier failure was treated as product success. The final docs/index closure changes no runtime code; a separate clean delivery clone and exact remote-branch check preserve the original acceptance source snapshots.

### Final requirement audit

- **R1 — real self-service mixed team:** the first-stage useful result and final `goal-b9868c32b582afd9aad83bbb` complete with observed native Codex/Pi/Claude coordination, implementation and independent review. Compiled UI Start/cancellation exercises the same explicit mixed team in both modes at both terminal sizes.
- **R2 — independent, frozen selection:** task/team/isolation controls and per-role edits are covered by the committed TUI tests and actual approval journeys. Boundary edits invalidate approval; ordinary text does not start work. Retained specs name every harness/provider/model and the exact image/socket.
- **R3 — observed identity and refusal:** final normalized events match all three actual native session/provider/model identities. Native identity, unsupported tool configuration, capability, provider and stale-incarnation negative tests pass; no fallback route was added.
- **R4 — real tool boundary:** current real native and container probes prove allowed writes, role ceilings, existing protected Git/control/source boundaries, absent private sockets and denied network. Actual Codex code-mode dispatch exposes only the broker bridge. Broker and file-helper tests reject cross-role/stale capabilities, links, malformed requests and forbidden writes.
- **R5 — transport honesty:** current native adapter and broker tests cover accepted/consumed/settled correlation, bounded framing, errors/aborts, duplicate/uncertain delivery, client cancellation and draining. Native completions and final text, not writable mail counts, establish results.
- **R6 — independent frozen verification:** the final audit matches the exact native reviewer turn/text/digest, approved argv/output hashes, unchanged before/after patch and original source. Implementation's native guardian and every container operation finish before the freeze event.
- **R7 — lifecycle and ownership:** real owner-death, cancellation, lease-failure and escaped-descendant probes pass. Actual UI cancellations have paired owned receipts and runtime absence. Uncertain-create/start, inconsistent exit state, canceled-but-unknown cleanup and receipt-write failures refuse success and prevent replay; missing final receipts remain missing rather than fabricated.
- **R8 — compatibility and no shared changes:** exact additive-spec tests pass. Current-binary audits preserve both the original no-team all-Codex result and the explicit group-mode result. Source snapshots remain clean; no primary checkout, global configuration, credentials, installed binary, existing service or retained run was changed.
- **R9 — ordinary usable UI:** compiled 100x24/50x16 journeys verify editing, full approval, explicit Start, live native roles, Keep running, Request cancellation, retained canceled state and all ready-result evidence. The deliberate visible-label negative control rejects the same rendering path.
- **R10 — tested durable delivery:** clean committed builds, actual native goals, independent artifact/runtime audits, current documentation and pinned lint supplement the focused Go tests. Feature-branch pushes are verified by exact `git ls-remote` equality; no PR, merge or installation is claimed.
- **R11 — both stages in order:** first-stage acceptance lands in `2f5227fc`, followed by the persisted container design in `05b6c9eb` and implementation in `c76423bc`. Actual container cancellation and logical-owner death terminate observed daemonized tool descendants; unavailable isolation refuses execution. Native model processes retain the explicitly disclosed group-only scope.

## Stabilization and operator handoff

Cameron requested branch stabilization, runbooks and session closeout after the
two cleanup stages passed acceptance. Codex retains the existing feature branch
and original acceptance artifacts. This follow-up does not authorize a PR,
merging the feature, global installation, deployment, or deletion of retained
runs. The generated changelog remains owned by Release Please.

Cameron subsequently authorized merging `origin/main` into this feature branch
for stabilization. That authorization does not merge the feature into main or
open a PR. The refreshed integration target is
`82fff4ade7a352cf1121ad9c65d9c128646678b4`.

- [x] Independently review the net branch changes and disposition supported findings.
- [x] Publish source-accurate setup, operation, recovery and evidence-review runbooks.
- [x] Run the full local suite and branch script checks; distinguish baseline or environment failures from regressions.
- [x] Merge the authorized main revision into the feature branch and verify the combined result.
- [x] Verify docs, lint, final build provenance and exact feature-branch remote equality.
- [x] Reconcile remaining work and record the durable handoff.

### Stabilization findings and dispositions

The first full Go 1.26.8 suite exposed branch-wide contracts that the earlier
focused acceptance did not exercise. Codex reproduced their red paths before
correcting the source; no architecture, JSON, or capped-output guard was
disabled.

- Container HTTP bodies, digest inputs and private receipts now have the
  existing one-call raw-JSON guard annotations; their exact encoding is
  unchanged. The helper's ASCII/numeric protocol probe uses the terminal-safe,
  value-preserving encoder.
- The hidden container-init command handles a standalone help request without
  executing the helper. A workload's later `--help` remains an argument.
- Project registry flag validation moves out of positional `Args` into the
  guarded `RunE`, so the menu no longer falsely requests positional arguments.
  An independently tested intermediate `PreRunE` version bypassed JSON error
  wrapping and was rejected. The existing root refusal and no-state-creation
  checks remain intact. The two menu rows were a branch defect, not a proven
  default-branch failure; the earlier baseline attribution is corrected here.
- Native executable resolution stays in the CLI, injected into the goal
  manager. The manager still canonicalizes and hashes the selected binary and
  refuses a missing resolver before creating state. The unchanged adapter
  dependency barrier no longer sees `internal/launch` through `goal`.
- Project presentation names and Agents display text use fixed capped helpers
  before applying the destination's display-width bound.
- The pinned whole-branch linter found two path-taint false positives: creating
  the fixed `source` child of a test-owned fixture directory, and the read-only
  `Lstat` in the private-directory checker. Exact file/rule/source exclusions
  cover only those calls; production path validation and symlink refusal are
  unchanged.
- Review's proposed Hermes streaming defect conflated live fragments with
  durable message replay. It was not supported by runtime evidence, so the
  fail-closed durable-turn check remains unchanged. New raw-schema tests cover
  live multi-chunk replies with one durable final message and reject duplicate
  durable frames, both with and without tools.
- The combined suite also exposed Windows compilation gaps, the goal screen's
  duplicated suspend filter, and a host-Git call outside the shared profile.
  Non-Unix JSON-process stubs fail closed; Unix-only adapter fixtures are
  correctly tagged. The goal screen uses the shared suspend options. Host Git
  retains its absolute binary, environment scrub, output caps and process-group
  timeout while adopting the shared Local profile. The only new Git-literal
  allowances are counted TUI verification recipes, not execution exceptions.
  Pinned lint confirmed G204 on the shared explicit-binary constructor; its
  exact file/rule/source exclusion replaces the previous constructor's inline
  allowance. Production callers supply `git` or the fixed `/usr/bin/git`, not
  goal-controlled executable input, and profile arguments remain first.
- The closed-pipe test's child rebuilt the entire CLI inside its 20-second
  deadline. The current branch failed that startup twice while clean main
  passed; Codex did not attribute it to a baseline failure or widen the bound.
  The exact desk-helper subprocess now enters its existing test directly;
  normal integration tests still build the CLI through TestMain.
- The recovery runbook now distinguishes legacy goals' three model-control
  directories from explicit native teams' additional execution-host controls.
  Root Codex checked the actual dispatch paths and an independent reviewer
  confirmed the correction.
- The inbox script could spend its socket-readiness budget loading the child
  and TypeScript extension. An explicit bounded child-ready handshake now
  separates startup from protocol checks without widening receipt deadlines.
  Child failure includes exit status and drained stderr; an early-exit negative
  control proves that startup failure is not mistaken for readiness.
- Independent correctness review inspected the final source diff and platform
  stub; independent security review inspected the host-Git profile change.
  Both returned no remaining supported findings. Root Codex separately reran
  the affected Git, goal, backend, CLI and shared-keymap regressions.

### Stabilization closeout evidence

The stabilized source is `8fd505b504027eb7fe79c25fc4f862f78ffb8c66`, following
the authorized merge of main at `82fff4ade7a352cf1121ad9c65d9c128646678b4`.
Root Codex pushed that source commit and verified exact remote equality with
`git ls-remote --heads origin refs/heads/codex/cross-harness-loop`. The closing
handoff changes only this plan and the runbook's build-provenance guidance.

- **Whole tree:** Go 1.26.8 `go test -p=2 -count=1 ./...` passed all 90 tested
  packages; six packages have no tests. The earlier CLI, typed architecture,
  Git-profile and suspend-filter failures all pass on the final source.
- **Static checks:** repository-pinned golangci-lint 2.13.1 `run ./...` reports
  zero issues. Go 1.26.8 `go vet -p=2 ./...` passes for Darwin, Windows,
  FreeBSD and Linux. Formatting and `git diff --check` pass; the Release Please
  changelog-owner check confirms no feature-owned changelog change.
- **Scripts:** all six Node contract/behavior suites and both Python terminal
  probe contract suites pass. One independent inbox run exceeded its bounded
  startup deadline while compilation was concurrent; the unchanged script
  subsequently passed after those compiler jobs finished. No protocol deadline
  was widened to obtain that pass. The retained failed invocation is not
  counted as success.
- **Build and UI:** a build in the nested worktree embedded the primary
  checkout's revision and was rejected by the provenance assertion. A clean
  detached clone reports the exact stabilized source SHA and
  `vcs.modified=false`. That compiled binary passes all ten retained-detail
  assertions at both 50x16 and 100x24: patch, every check, JSON, scrolling,
  identity and process cleanup. It starts no new native goal.
- **Native evidence:** the current binary independently re-audits
  `goal-b9868c32b582afd9aad83bbb`: the original frozen patch digest is unchanged,
  all three native identities and guardian receipts match, both approved
  checks match, and all 43 owned containers remain removed. The source checkout
  and retained artifact bytes remain unchanged. Real container boundary,
  lease-failure, cancellation and owner-death probes, plus the real no-model
  native execution-host probe, also passed against the merged helper sources;
  the later stabilization edits do not alter those Unix boundary/helper files.
- **Documentation:** the two operator runbooks and their command/README/plan
  cross-links pass the compiled docs checker with all seven requested roots
  checked and none skipped. Native-model process-group limits, trusted host-Git
  configuration, experimental status and the no-replay recovery boundary are
  explicit rather than implied to be stronger isolation.

Local command logs are retained under `/private/tmp/forgectl-stabilize.709Px8/`:
`go-test-final.log`, `lint-final-fixed.log`, `vet-final-*.log`,
`vet-fixed-freebsd.log`, `vet-fixed-linux.log` and `serial-test-*`.
The clean source, binary provenance, UI captures and fresh retained-result audit
are under the original checkout's
`.claude/worktrees/native-stabilized.3IDdwS/`, principally `final-source/` and
`delivery/`. Original native acceptance artifacts remain in place. No global
binary, credentials, shared service or primary checkout was changed. These are
local results, not a claim of hosted CI, release, deployment or human acceptance.

### Deferred outside this delivery

- Hermes remains unsupported as a self-service native team member; the attended pilot is not advertised as parity. A future native adapter needs its own exact protocol, identity and boundary acceptance.
- All-descendant containment for the native model harness processes themselves remains separate work. The new container guarantee covers tool/file/check execution, not a hostile daemon/kernel or other same-user process.
- Host-Git filter/configured-hook hardening is separate from tool containment. The current helper retains the existing trusted-repository boundary, rather than claiming that the shared Local profile blocks every repository-configured executable. The operator runbook explicitly requires trusted Git configuration and attributes; extending the bounded, byte-preserving Git seam to the stronger filter/hook helpers needs its own acceptance controls.
- Opening a PR, merging the feature into main, releasing or installing it remain separate operator decisions. No hosted-CI result is claimed for a feature-branch push without a PR.
