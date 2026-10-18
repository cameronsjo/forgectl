# A real repository goal through Projects

Status: in progress
Next action: review the bounded pilot, then exercise a real implementation and independent review through native team sessions.

## Goal and authority

Cameron approved one real repository task carried from a plain-language goal to a reviewable, independently verified result, including interruption and recovery, without Cameron relaying messages. This follows the completed Projects monitoring and readability work. The outcome, not another launcher or agent framework, is the milestone.

Codex selects forgectl itself and this first goal: **make the Agents overview's keyboard controls discoverable without crowding the activity view, including the narrow pane Cameron uses.** The current `projectAgentsModel.updateKey` has no help key; the footer's narrow form exposes only selection and quit. Some open UX reports already have fixes in this branch, so those reports are not used as evidence of a current bug.

The task may change only `internal/tui/project_agents_unix.go`, its existing test file, and `docs/commands/surface-project.md`. The result must offer on-demand keyboard help, preserve selection and activity state when dismissed, fit a 50-column by 16-row terminal, avoid invoking focus/evidence actions while help is open, and retain the existing quit and safety behavior. Tiny terminals must remain bounded and readable rather than overflowing. The native implementation agent chooses the smallest solution consistent with those outcomes.

Standing authorization covers isolated work, source commits and the session feature-branch push. No PR, merge, deployment, global installation/configuration, credential change, shared service restart, or third-party repository write is authorized. OpenAI-only execution remains in force; Claude and concurrent Hermes native UI remain deferred. Agent messages cannot approve an action or widen permissions.

## Approach and alternatives

Reuse `surface project` for logical identity, `surface team` for native enrollment, `surface send`/`inbox`/`flush` for communication, and native observations plus actual source/test evidence for results. The Estate's delivery-automation card identifies forgectl as the existing dispatcher; its September forgectl card has stale details, so current source/help governs. Do not revive a retired dispatcher or put collaboration in Desk's human approval queue.

Use an opt-in source-build pilot entrypoint with an explicit goal file, repository checkout, private runtime root, binary/model paths and native safety extensions. It persists goal, identity and progress receipts before dispatch, automatically flushes only the task's private mailbox, and leaves evidence on failure. It is not a daemon, general scheduler, new transcript store, or blanket claim of resumability. The operator can inspect the real project with the existing Agents/timeline views.

A native Codex coordinator delegates to a native Pi implementation worker in an isolated Git worktree. A separately enrolled native Codex reviewer inspects the resulting patch, with no write authority to that worktree. Native selected providers/models are checked, not inferred from a catalog. Pi keeps its installed authentication and safety extensions; no native approval or command denial is bypassed. The coordinator and reviewer receive network-disabled Codex workspace policies limited to the private task runtime, outside the implementation worktree. Code changes remain local until Codex independently inspects and verifies them.

The coordinator must route clarification, implementation and review through the existing mailbox without Codex fabricating agent replies. A completed native turn is not a successful task; a review message is not an executable check. Codex separately verifies the actual returned diff, runs the affected checks and negative controls, and records the source revision/hash covered.

For the recovery exercise, first establish that the coordinator's turn settled, stop only its owned native process, enroll a fresh run under the same logical coordinator thread, and recover the existing goal/result from durable records. Keep the implementation worker and its patch unchanged; do not redelegate implementation. An ambiguous delivery remains unknown rather than triggering another task. This proves bounded explicit replacement, not arbitrary crash recovery.

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
- [ ] Review the pilot and resolve supported safety/operability findings.
- [ ] Add and test the bounded pilot entrypoint and its operator-authored goal.
- [ ] Run native delegation, implementation and independent review; preserve the actual artifacts.
- [ ] Independently verify the patch and exercise coordinator replacement without duplicate work.
- [ ] Inspect the real terminal interaction, integrate only the verified allowed patch, and rerun affected checks.
- [ ] Record limitations and receipts, commit/push the session feature branch, and report the result without merging or installing.

## Review

Codex subagent `agents_ux_review` inspected current project/dispatch/recovery seams read-only at `c420a090`. Codex independently read the cited project, mailbox and native observation contracts. Reuse is supported; project-native stop/resume remains an explicit pilot lifecycle operation, and OpenAI-only routing is an acceptance constraint rather than a universal product promise.

The substantial-plan review is pending. No native task run or successful implementation is claimed by this plan.

## Completion evidence

Pending. Existing Projects acceptance and the readable preview are prerequisites, not evidence that this repository-goal milestone has passed.
