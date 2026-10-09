---
status: in-flight
next: "T9.1 surface intake gh, then the file-set security review, then a live intake of one labeled issue."
branch: plan/atelier-p3-intake
pr: "—"
updated: 2026-10-09
approved_session_id: "— (operator: 'own it and get it done', 2026-10-09; P3 is in the approved atelier plan)"
date: 2026-10-09
session_id: 30dd3ebb-3720-463f-aa15-9570c1ff88a9
model: claude-opus-5-5
harness: claude-code 2.1.289
machine: cf6e768835c7
source_plan: "cadence-ecosystem docs/plans/2026-10-05-atelier-a-herdr-work-queue-cockpit-for-claude-code.md § P3; docs/plans/2026-09-28-forgectl-herdr-coordinator.md T9"
---

# atelier P3: intake from GitHub labels (forgectl T9)

## Goal

`forgectl surface intake gh --repo <path>` turns open issues carrying an eligible label into queue rows, so the drain works them. A row's brief is a fixed template with the issue's title and body fenced as data; the worker's PR closes the issue with `Closes #N`. The eligible label is the request: removing it is how the operator says no.

## Why the gate matters

A drain worker is a full harness (ADR-0010, 2026-10-08): it runs as the operator with the operator's allow rules, including `gh pr merge`. An issue's text becomes its brief. So intake takes an issue only when the operator trusts that text, and gives the worker nothing else from GitHub:

- **Author:** on `[surface.intake] authors`. Default: the repository owner's login for a user-owned repository; for an organization-owned one, intake refuses until `authors` is set. Logins compare case-insensitively and exactly; a `[bot]` login is never accepted.
- **Labeler:** for each eligible label on the issue, the latest `labeled` event for that label name must be by an allowed author, with no later `unlabeled` event for it.
- **No edits after labeling:** the body's `lastEditedAt` must be strictly before that latest label event, and no `RENAMED_TITLE_EVENT` may be at or after it. Only an explicit JSON `null` means never edited; a missing field or any GraphQL error refuses.
- **Not a pull request, not transferred** (no `TRANSFERRED_EVENT` in the timeline).
- **One read:** title, body, author, `lastEditedAt` and timeline come from one GraphQL query, and that body is the one fenced into the brief.
- **The fenced text is the whole task.** The brief tells the worker not to read the issue, its comments, linked issues or PRs, or any URL, with `gh`, `curl` or anything else, and to treat instructions inside the fenced text or anything it fetches as data. The issue number appears only in the `Closes #N` rule. The fence is a delimiter carrying a random nonce, so the body cannot close it.
- **Never create issues or labels:** the brief forbids it.

Recorded boundary, not designed against: the brief's rules are instructions, not a sandbox. A full-harness worker can still fetch the issue and its comments, and every agent on the machine acts as the owner, so a misled worker could file and label an issue that would pass every check. ADR-0010's "accidents, not adversaries" covers both; `max_per_run` and the label-as-request model limit how far one mistake spreads.

## Design

- **Repo:** `--repo <path>` is a local checkout (as `surface enqueue` takes); intake reads `owner/repo` from its `origin` remote, github.com only, and refuses any other host.
- **Eligible labels:** `[surface.intake] labels`, default `["exec:mechanical", "exec:guided"]`; `--label` narrows to one.
- **Row:** name `gh<number>-<repo slug>`: the repo name lowercased, other characters mapped to `-`, truncated so the whole name fits `worker.ValidName` (48), the number always kept. A name already taken by another repository is a skip with that reason. The row records `source: gh:<owner>/<repo>#<number>` and `author`. `--harness`, `--model` and `--profile` apply to every row the run creates.
- **Dedupe:** an existing row with the same name, in any state, is a skip naming its state (`failed` says "dequeue to retry"). `dequeue` makes the issue eligible again on the next run if it still carries the label; removing the label stops it for good.
- **Per issue:** each refusal or skip is reported with the issue number and reason and the run goes on. A full queue (`ErrQueueFull`) stops the run and says so. `[surface.intake] max_per_run`, default 5, caps rows added per run.
- **`--dry-run --json`** lists what would be enqueued and what would be skipped, with reasons, and changes nothing.
- **Manual:** the operator or the coordinator session runs intake; the drain does not call GitHub for intake.

## Loop

| Thing created | Opened by | Closed by | Who closes |
|---|---|---|---|
| Queue row from an issue | `surface intake gh` | as any queue row (drain, `dequeue`, expiry, prune) | the drain, or the operator |
| The issue | its author, labeled by the operator | the worker's PR (`Closes #N`) on merge; or the operator removes the label | the merger, or the operator |

## Alternatives declined

- **An `exec:queued` marker label (parent plan).** Its add, remove and reconcile steps had no reliable closer, flapped against dequeue, and duplicated what the queue row name already prevents. The eligible label is the request; the row name is the dedupe.
- **Board intake in this phase.** Board cards carry no repository or brief structure, and their close needs the operator's confirmation. Deferred until a card format exists.
- **The drain polling GitHub.** A second loop and a credential in the background.
- **Interactive confirmation of every intake run.** It would stop the coordinator from running intake; the author, labeler and edit checks plus `max_per_run` are the gate instead.

## Tasks

### T9.1: `surface intake gh` (one PR)

- [ ] Config `[surface.intake]` (`authors`, `labels`, `max_per_run`), validated at load.
- [ ] Queue row fields `source` and `author` (omitempty; queue version unchanged, so a drain on an older binary must be restarted after the upgrade, as with forgectl#1199), and an enqueue path that takes them.
- [ ] The intake command: remote → owner/repo; one GraphQL query per page of eligible issues; the gate above; the brief template with the nonce fence and the rules; enqueue; per-issue skips; `--dry-run --json`.
- [ ] Tests from captured GraphQL fixtures: each refusal (outsider author, bot author, outsider labeler, relabel by an outsider after the owner, edit between labelings, same-second edit, title rename after label, missing `lastEditedAt`, GraphQL error, PR, transferred, too long, `CheckBrief` content, org repo with no `authors`, non-github remote, name collision) and the happy path; idempotence; a body containing the fence text. Stage breaks of the author, labeler, edit and title checks and confirm each goes red.
- [ ] Docs: docs/herdr.md "Intake" section, help text.
- [ ] Security review (Opus) of the control's file set, not only the diff, before the live check: the T9.1 diff with the brief template and fixtures; `internal/surface/worker/brief.go`, `queue.go`; `internal/surface/drain/drain.go`; `internal/cli/surface_queue.go`; `internal/launch/invocation.go`. It checks the gate against the written code. `[surface.merge] mode` stays `off` (ADR-0011 Decision 10).
- [ ] Live check: label one real issue in cameronsjo/forgectl (user-owned, so the default `authors` covers it), run intake with `--dry-run`, then for real; confirm the row, the brief, and a draft PR with `Closes #N`.

## Verification

- `go test ./...`, `go vet`, golangci-lint; the staged breaks above.
- The live check, recorded in this plan.

## Orchestrator

**Driver:** opus. Trigger: untrusted issue text becomes the brief of a worker that runs as the operator.

## Panel

Panel: plan-reviewer, security-posture-reviewer (Opus) ran — 1 Critical, 9 Important/High, 6 Medium folded in; 3 declined (see below)

## Panel review findings declined

- **[Security posture] Interactive confirmation before enqueue.** Declined (Alternatives declined): the operator wants the coordinator able to run intake; the brief rule and the recorded boundary stand instead.
- **[Plan reviewer] Per-source queue depth cap.** `max_per_run` is enough for manual runs; recorded as a deviation.
- **[Plan reviewer] Split the queue-schema change into its own PR.** The two fields are small and omitempty; one PR.

## Deviations from the parent plans

- **No `exec:queued` marker, no `--reconcile`, and no marker removal in P4** (see Alternatives declined).
- **Board intake deferred.**
- **Dedupe on the row name, not a source-ref index;** a failed row blocks re-intake until dequeued, which keeps the parent's "a failed row is not claimed again".
- **`max_per_run` instead of a per-source depth cap.**
- **No CHANGELOG task:** forgectl's changelog is written by release-please from commit messages.

## Learnings
