---
status: done
---

# A real repository goal through Projects

Status: native repository-goal pilot and independent source/terminal acceptance complete
Next action: wait for a separately approved next goal or productization step. PR creation, installation, merge and deployment remain outside this authorization.

## Goal and authority

Cameron approved one real repository task carried from a plain-language goal to a reviewable, independently verified result, including interruption and recovery, without Cameron relaying messages. This follows the completed Projects monitoring and readability work. The outcome, not another launcher or agent framework, is the milestone.

Codex selects forgectl itself and this first goal: **make the Agents overview's keyboard controls discoverable without crowding the activity view, including the narrow pane Cameron uses.** The current `projectAgentsModel.updateKey` has no help key; the footer's narrow form exposes only selection and quit. Some open UX reports already have fixes in this branch, so those reports are not used as evidence of a current bug.

The native implementation task may change only `internal/tui/project_agents_unix.go`, its existing test file, and `docs/commands/surface-project.md`. The result must advertise `?` help in both wide and narrow footers, list all current key aliases, dismiss with `?` or Escape, preserve selection/offset/zoom/activity state when dismissed, fit a 50-column by 16-row terminal, and consume action/navigation/zoom keys without side effects while help is open. `q` and Ctrl-C still quit. Tiny terminals must remain bounded and readable rather than overflowing. The native implementation agent chooses the smallest solution consistent with those outcomes.

Codex separately owns the pilot infrastructure: `scripts/run-project-goal.mjs`, `scripts/test-project-goal.mjs`, `scripts/project-keyboard-help-goal.json`, the optional child-lifecycle hook in the reused `scripts/verify-cross-harness-loop.mjs` command runner, this plan, and the pilot usage section of `docs/commands/surface-project.md`. These are not additional files the native implementation worker may edit.

The first native run exposed one required communication control: sandboxed Codex-to-Codex `surface send` attempted `codex queue` and became `unconfirmed`, so no review ran. Codex preserves that attempt without retrying it and adds an explicit `surface send --queue-only` path in the existing mail service/CLI, its tests, and `docs/commands/surface-team.md`. This reuses all existing enrollment, run fencing, policy, durable queue and supervisor-flush checks; it does not broaden native permissions or reinterpret an ambiguous delivery. The implementation also left an ignored build binary, which the pilot's all-untracked-files scope check rejects. Future worker instructions explicitly direct build output to `/dev/null`.

Standing authorization covers isolated work, source commits and the session feature-branch push. No PR, merge, deployment, global installation/configuration, credential change, shared service restart, or third-party repository write is authorized. OpenAI-only execution remains in force; Claude and concurrent Hermes native UI remain deferred. Agent messages cannot approve an action or widen permissions.

## Approach and alternatives

Reuse `surface project` for logical identity, `surface team` for native enrollment, `surface send`/`inbox`/`flush` for communication, and native observations plus actual source/test evidence for results. The Estate's delivery-automation card identifies forgectl as the existing dispatcher; its September forgectl card has stale details, so current source/help governs. Do not revive a retired dispatcher or put collaboration in Desk's human approval queue.

Use `node scripts/run-project-goal.mjs --live` with an explicit goal file, clean isolated worktree and expected HEAD, fresh private runtime root, binary/model paths and native safety extensions. It creates a dedicated project and ledger for this task, never takes an existing shared ledger, and flushes only that ledger. It is not a daemon, general scheduler, new transcript store, or blanket claim of resumability. The operator can inspect the real project with the existing Agents/timeline views.

The private runtime contains the immutable input copy `goal.json`, append-only `events.jsonl`, atomic-replace `state.json`, the final `result.patch`, and a `checkpoint.json` tying task/goal hash/base/patch hash/message IDs to exact role bindings. State distinguishes setup, implementation, review, recovery, ready-for-verification, interruption and failure; no state means independently accepted until Codex's separate checks run. `--status ROOT` reads without launching or mutating. Queued/unconfirmed delivery remains visible in mailbox evidence; the entrypoint does not resend it. A terminated pilot is inspectable, not silently restartable from the same root.

A native Codex coordinator delegates to a native Pi implementation worker in an isolated Git worktree. A separately enrolled native Codex reviewer inspects the resulting patch, with no write authority to that worktree. The native Codex provider must be `openai`; Pi must report its exact requested provider/model and a direct OpenAI HTTPS endpoint before any model prompt and after work. Pi keeps its installed authentication and safety extensions; no native approval or command denial is bypassed. The coordinator and reviewer receive network-disabled Codex workspace policies limited to their runtime working directory and the dedicated ledger, outside the implementation worktree. Before model work, native `command/exec` must successfully write a private runtime probe and fail to write a new sentinel inside the implementation worktree. No existing source file is used as a negative-control write target. Code changes remain local until Codex independently inspects and verifies them.

The coordinator must route clarification, implementation and review through the existing mailbox without Codex fabricating agent replies. A completed native turn is not a successful task; a review message is not an executable check. Codex separately verifies the actual returned diff, runs the affected checks and negative controls, and records the source revision/hash covered.

For the recovery exercise, first establish that the coordinator's turn settled, then stop the exact Codex `ChildProcess` object spawned and retained by this pilot, close its client socket and await its exit. The journal records PID, launch time, executable, socket, run and native thread; do not rediscover or kill a process by PID/name after losing that live handle. An unresolved exit blocks replacement. Enroll a fresh run/socket/native thread under the same logical coordinator thread and recover the existing goal/result from `checkpoint.json` and durable inbox records. Keep the implementation worker and its patch unchanged; compare the complete allowed diff hash and coordinator-to-worker message IDs before/after recovery. The replacement's completed native turn must produce a recovery record matching task, source revision, patch and result/review message IDs. An ambiguous delivery remains unknown rather than triggering another task. This proves bounded explicit replacement, not arbitrary crash recovery.

## Requirements

- The task MUST begin with an operator-authored goal and bounded change authority, not a prewritten solution or synthetic fixture task.
- Native coordination MUST carry implementation and review messages without a human message relay.
- Every role MUST have exact project/thread/run/native-session identity. Replacement MUST retain the logical thread and retire the old run.
- The implementation MUST occur in an isolated worktree; unrelated checkout state MUST remain untouched.
- The reviewer MUST be independent of the implementer and MUST NOT edit the implementation worktree.
- Evidence MUST distinguish queued/sent, native turn completion, review findings, executable checks, and accepted result.
- Recovery MUST preserve the same task identity and source bytes without duplicate implementation dispatch.
- The UI result MUST be inspected in an actual terminal at normal and narrow sizes, including opening/dismissing help and retaining selection.
- Source-level tests MUST include cases that fail without the new behavior; acceptance MUST NOT infer success from a model's report.
- The entrypoint MUST preserve failures and bounded progress as durable evidence; it MUST NOT claim general unattended production readiness.

## Work

- [x] Inspect reuse candidates, actual source contracts, current backlog, and the observed UI gap.
- [x] Review the pilot and resolve supported safety/operability findings.
- [x] Add and test the bounded pilot entrypoint and its operator-authored goal.
- [x] Run native delegation, implementation and independent review; preserve the actual artifacts.
- [x] Independently verify the patch and exercise coordinator replacement without duplicate work.
- [x] Inspect the real terminal interaction, integrate only the verified allowed patch, and rerun affected checks.
- [x] Record limitations and receipts, commit/push the session feature branch, and report the result without merging or installing.

## Review

Codex subagent `agents_ux_review` inspected current project/dispatch/recovery seams read-only at `c420a090`. Codex independently read the cited project, mailbox and native observation contracts. Reuse is supported; project-native stop/resume remains an explicit pilot lifecycle operation, and OpenAI-only routing is an acceptance constraint rather than a universal product promise.

The same independent reviewer examined this plan at `bd4bbf42` through plan, operability and UX lenses. Codex verified and incorporated its supported findings: separate infrastructure/worker file scopes; exact owned-process stop/exit rather than `close-view`; explicit atomic receipts and unknown states; a dedicated ledger; pre/post native OpenAI checks; executable reviewer write-denial and actual diff checks; precise help/dismiss/quit behavior and narrow-screen acceptance. That design review did not execute native acceptance; later implementation and acceptance have their separate receipts below.

## Completion evidence

The entrypoint contract tests pass with `node scripts/test-project-goal.mjs`, including invalid input, stale message identity, incomplete recovery, private status-file checks, and an actual detached command-group termination control. The existing verifier contracts pass with `node scripts/test-cross-harness-verifier.mjs`. Independent review led to canonical runtime inputs, a private dedicated ledger, tracked command children, an exclusive retained worktree claim, and primary-checkout before/after receipts. These are local trusted-package checks, not protection against another process running as the same OS user replacing installed package bytes or dependencies.

Native execution, source-level verification and actual-terminal acceptance passed in the fresh queue-only attempt below. The earlier Projects preview was used only as an unbound registry fixture; the new clean-built binary supplied the keyboard-help behavior under test.

Two pre-agent setup attempts revealed the project's ledger path is declared before its directories exist; the pilot now creates the dedicated private path before enrollment. The first real attempt (`/private/tmp/fggoal.vUjZhX`, task `goal-6d6504cd-7d8f-47da-917b-7c1044a0187e`) produced a three-file Pi patch and one unconfirmed coordinator-to-reviewer message. Codex stopped the exact supervisor after proving its three recorded child processes still belonged to it. Its final state is `interrupted`, not accepted; native cleanup completed, and no old message is reused for another attempt.

The queue-only correction passes `go test -race -p=2 ./internal/surface/mail ./internal/cli -run 'Test(Queue|SurfaceSend)' -count=1` with Go 1.26.8, both Node contract suites, and golangci-lint 2.13.1 against the actual diff from `47edae8f`. Independent read-only review found no remaining supported blocker. The full mail suite passes. The full CLI suite is not green: `TestStateVerbsDeclareJSON` and `TestMenuJSON_LiveTree` also fail unchanged at `b08218d5`, before this correction. They identify missing group-command JSON exemptions and unnamed required project-list arguments; repairing those earlier Projects contracts is recorded as follow-up work, not hidden by the focused result.

The fresh attempt at `/private/tmp/fggoal.4ueM7r` completed native implementation, independent review and explicit settled-coordinator replacement with exit 0. Its task is `goal-9e5a4f41-a23e-4bf4-ab06-ac0651513fe8`, project `7309673b-85ed-4dc5-9c36-7643f05c6226`, and implementation base `f6b0c7ccaa4a93e35001a1d6ae96c0b98e66996e`. The reviewed full-index patch SHA-256 is `4ebf9c4384d48220d95a979e38fa5906c02865732f75c937d96f7c9e36c2355c`. The Pi result is message `1e93565b-f54c-4486-85e8-4ce9fed23efb`; independent Codex review is `00beef2b-4d10-4512-a476-73da4ea9a7d4`. All three native roles reported the requested OpenAI provider and `gpt-5.6-sol` model. Native sandbox probes denied coordinator/reviewer writes to the source worktree before model execution.

Recovery replaced coordinator run `5bbc5ed0-26f9-49ab-8499-bc99c6a4e1f3` with `850fbdc3-4a57-48a5-9909-0047de6bf4e0` under the same logical thread, after proving the old process group and socket had stopped. The replacement recovered matching task, goal, base, patch, result/review messages and role bindings. Patch bytes and the complete coordinator-to-implementation message-ID list stayed identical; exactly one implementation assignment exists. Final state is deliberately `ready_for_verification`, `result_verified: false`, with `cleanup_complete: true`, not an agent-authored acceptance claim.

Codex independently inspected the three-file patch, verified no untracked/ignored output, ran the full TUI suite and focused `TestProjectAgents` race tests, and ran golangci-lint 2.13.1 against the actual diff from the pinned base. All passed; the patch hash stayed unchanged across checks. A behavior-only footer regression compiled and failed at the unchanged base, and the real-terminal probe rejected that base's missing help footer. Integrated files match the native-reviewed bytes exactly. The broader terminal-safety pin also fails unchanged at `b08218d5` (`TestTextPrintersUseCappedHelpers`, naming `projectMetadataName` and `safeProjectAgents`); that earlier contract is follow-up work, not silently relabeled green. Missing frontmatter in this session's three plans was corrected and the official plan-index generator/check now passes.

The integrated implementation is committed at `d997ca2c676353ae8bdd6fbb9d98b0ca4aa62f2e`; both TUI checks passed again after integration, and pinned lint passed against the actual committed diff from `a2a11100`. A clean clone built `/private/tmp/forgectl-goal-accepted.qhpcmD/bin/forgectl` with Go 1.26.8, `vcs.modified=false`, that exact revision, and SHA-256 `9282875dd516f211158f3b74a70889392689f93ebabadbc6801de7b79f0cad8f`. `git push origin codex/cross-harness-loop` succeeded and an exact `git ls-remote origin refs/heads/codex/cross-harness-loop` comparison confirmed delivery of that implementation revision.

`python3 /private/tmp/forgectl-goal-build.KLL9Os/verify-help.py` passed against that binary in actual 100×24 and 50×16 PTYs. It verified both help footers, all key aliases and descriptions visibly within each viewport, `?`/Escape dismissal, preserved selection/time window after each individual action/navigation/zoom key, and quitting from help via `q` at 100×24 and Ctrl-C at 50×16. Source tests separately prove focus/evidence callbacks cannot run and offset/activity snapshots remain intact. Codex read the reconstructed help/restored screens, not just the probe's exit code. Raw terminal streams, screens and `receipt.json` are under `/private/tmp/forgectl-goal-accepted.qhpcmD/ui-help`; the verifier also proves its exact child process group is gone before passing. The unchanged-build run fails on the actual missing help footer, not on setup or cleanup.

Independent acceptance is recorded in `/private/tmp/fggoal.4ueM7r/independent-acceptance.json`, tying the reviewed patch, integrated file hashes, code commit, clean binary, checks and terminal receipts together. The native pilot state remains honestly `ready_for_verification`; this separate Codex receipt, not a native agent's confidence, records `result_verified: true`. A separate host check proved all four recorded native process groups returned `ESRCH` and all four sockets were absent. No PR, global installation, merge, deployment, shared service restart, or old ambiguous-message retry occurred. This proves an attended goal-to-reviewed-result loop and explicit settled-coordinator recovery, not arbitrary crash recovery, a general scheduler, or an interactive goal-launch UI.
