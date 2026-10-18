---
status: in-flight
---

# A real repository goal through Projects

Status: in progress
Next action: exercise a real implementation and independent review through native team sessions, then independently verify the returned patch.

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
- [ ] Run native delegation, implementation and independent review; preserve the actual artifacts.
- [ ] Independently verify the patch and exercise coordinator replacement without duplicate work.
- [ ] Inspect the real terminal interaction, integrate only the verified allowed patch, and rerun affected checks.
- [ ] Record limitations and receipts, commit/push the session feature branch, and report the result without merging or installing.

## Review

Codex subagent `agents_ux_review` inspected current project/dispatch/recovery seams read-only at `c420a090`. Codex independently read the cited project, mailbox and native observation contracts. Reuse is supported; project-native stop/resume remains an explicit pilot lifecycle operation, and OpenAI-only routing is an acceptance constraint rather than a universal product promise.

The same independent reviewer examined this plan at `bd4bbf42` through plan, operability and UX lenses. Codex verified and incorporated its supported findings: separate infrastructure/worker file scopes; exact owned-process stop/exit rather than `close-view`; explicit atomic receipts and unknown states; a dedicated ledger; pre/post native OpenAI checks; executable reviewer write-denial and actual diff checks; precise help/dismiss/quit behavior and narrow-screen acceptance. The reviewer did not execute native acceptance. No successful implementation is claimed by this plan.

## Completion evidence

The entrypoint contract tests pass with `node scripts/test-project-goal.mjs`, including invalid input, stale message identity, incomplete recovery, private status-file checks, and an actual detached command-group termination control. The existing verifier contracts pass with `node scripts/test-cross-harness-verifier.mjs`. Independent review led to canonical runtime inputs, a private dedicated ledger, tracked command children, an exclusive retained worktree claim, and primary-checkout before/after receipts. These are local trusted-package checks, not protection against another process running as the same OS user replacing installed package bytes or dependencies.

Native execution and terminal acceptance remain pending. Existing Projects acceptance and the readable preview are prerequisites, not evidence that this repository-goal milestone has passed.

Two pre-agent setup attempts revealed the project's ledger path is declared before its directories exist; the pilot now creates the dedicated private path before enrollment. The first real attempt (`/private/tmp/fggoal.vUjZhX`, task `goal-6d6504cd-7d8f-47da-917b-7c1044a0187e`) produced a three-file Pi patch and one unconfirmed coordinator-to-reviewer message. Codex stopped the exact supervisor after proving its three recorded child processes still belonged to it. Its final state is `interrupted`, not accepted; native cleanup completed, and no old message is reused for another attempt.

The queue-only correction passes `go test -race -p=2 ./internal/surface/mail ./internal/cli -run 'Test(Queue|SurfaceSend)' -count=1` with Go 1.26.8, both Node contract suites, and golangci-lint 2.13.1 against the actual diff from `47edae8f`. Independent read-only review found no remaining supported blocker. The full mail suite passes. The full CLI suite is not green: `TestStateVerbsDeclareJSON` and `TestMenuJSON_LiveTree` also fail unchanged at `b08218d5`, before this correction. They identify missing group-command JSON exemptions and unnamed required project-list arguments; repairing those earlier Projects contracts is recorded as follow-up work, not hidden by the focused result.
