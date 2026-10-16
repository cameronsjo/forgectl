---
status: done
next: "Done: T1-T4. Remaining P1 items are tracked in forgectl#1244."
branch: feat/paperclip-p0-spike
date: 2026-10-12
updated: 2026-10-12
source_plan: "docs/plans/2026-10-12-paperclip-components.md (PR #1246); forgectl#1244 item 1"
session_id: fd4779c0-8a8c-4011-a13e-3cb85f94e0b7
model: claude-opus-5-5
harness: claude-code 2.1.289
machine: cf6e768835c7
---

# Paperclip worker credentials: let pane workers report status

> **Code removed 2026-10-15** with the rest of `integrations/paperclip/`; see `2026-10-12-paperclip-components.md`.

## Goal

A Paperclip agent running as a forgectl herdr worker sets its own issue to `in_review` at the end of a turn, with a one-line comment. Today it cannot: forgectl's worker environment allowlist (`internal/launch/invocation.go` `workerEnvKeys`) drops every Paperclip variable. Paperclip then answers each finished turn with an `issue_disposition_repair` wake (P0 baseline: runs `b5711052` and `3aee211f`, both `automation`, after codex turn `1f05abb6`), and a repair that fails parks the issue as `blocked`.

Done when a worker turn ends with its issue in `in_review`, set by the worker under that turn's run, and no repair wake follows. forgectl and the adapter never write the credential to argv, the ledger, `--json` output, a log line, or the brief text.

## Design

Use Paperclip's run-scoped agent JWT, not a long-lived key and not a forgectl environment change.

- The adapter declares `supportsLocalAgentJwt: true`. Paperclip then mints a JWT per run and passes it as `ctx.authToken` (Paperclip `v2026.1005.0`: `packages/adapter-utils/src/types.ts` `AdapterExecutionContext.authToken`; minted only when the run takes the legacy, non-native runtime path, which `herdr_forgectl` does). The token expires in 48 h, is bound to its run id, and loses write access when the run is cancelled (`server/src/middleware/auth.ts` around lines 374-401, per the posture review).
- Per turn, the adapter writes `~/.local/state/forgectl-paperclip/turns/<claude-session>.report` (0700 script, `O_EXCL|O_NOFOLLOW`, inside the existing private state dir) that embeds `PAPERCLIP_API_URL`, the issue id, the run id and the JWT, and takes two arguments: a status (`in_review` only) and a one-line comment. It calls `PATCH /api/issues/:id` with `Authorization: Bearer` and `X-Paperclip-Run-Id`, then `POST /api/issues/:id/comments`, with `curl --proto =http --max-redirs 0` to the loopback API.
- The brief names the script's absolute path and the exact command. The script never prints the token and passes it to curl through a file descriptor, but the worker can read the file (T4 review, I1).
- The script is removed in the adapter's `finally`, with the other turn files. A crash leaves at most a token that expires within 48 h.

The worker runs as the operator (ADR-0010), so it can read the script. The token is scoped to this agent and this run, and expires; that is the boundary claimed, and only under Paperclip's `authenticated` mode. Under `local_trusted` (the spike) any local process is Paperclip's instance admin, so no per-worker credential adds a boundary there; this plan makes status reporting work in both modes and claims containment in neither.

## Loop

| Opened by | Closed by |
|---|---|
| A per-turn report script holding a run JWT | T1: the adapter's `finally` removes it (`removeTurnFiles`); the JWT expires in 48 h if a crash skips the `finally` (`existing — Paperclip JWT TTL`) |
| Issues left in `in_review` by agents | `existing — a merged PR with Closes #N closes both sides` (decision 3); P3 GitHub sync, forgectl#1245 |

## Alternatives declined

- **forgectl `surface launch --env-file` with a per-worker Paperclip agent API key** (this plan's first draft). Declined after the panel: a standard agent key never expires and needs a revoke path the adapter does not have (it sees no `surface close`), minting it needs a board credential the adapter does not hold in `authenticated` mode, and the env-file widens a security control that a run JWT does not need. forgectl#1239 item 1 stays open for other integrations.
- **A `task_bridge`-scoped agent key.** Narrower than a standard key, but it still never expires and still needs minting and revoking.
- **Static `env` in a `[launch.profiles]` entry**, or **the token in the brief or the worktree.** Shared config, the transcript, and a worker-writable directory, respectively.
- **Components plan row 22** preferred the run JWT as a forgectl worker credential; this plan uses it per turn through the adapter instead, which needs no forgectl change.

## Panel

Panel: security-posture, plan-reviewer ran — 29 findings, 26 folded in (most by replacing the env-file design with a run JWT), 3 declined (see Panel review — findings declined)

## Panel review — findings declined

- Plan review: a per-task `Interfaces:` block. Folded as one `Interfaces` section, since all tasks change one file set in one repo.
- Plan review: specify the `pi_local` instruction bundle here. Moved out of scope to forgectl#1244's "agents stop at in_review" item: `pi_local` is a different adapter with its own credentials.
- Posture review: `X-Paperclip-Run-Id` is unchecked with an agent key. Moot: the run JWT binds the run id and Paperclip rejects a mismatched header.

## Global Constraints

- No change to forgectl's worker environment, the trampoline, or `workerEnvKeys`.
- The JWT appears only inside the 0700 report script in the private state dir. Errors name the issue and the HTTP status, never a header or token.
- The report script accepts only `in_review` as a status, a comment of at most 500 bytes with no control characters, and calls only `127.0.0.1` on the server's port.
- Operator-only adapter settings stay in the server environment (PR #1246 commit `87f0ef0f`: an agent can edit its own `adapterConfig`).

## Interfaces

- Paperclip, at `v2026.1005.0`: `ctx.authToken` (run JWT), `PATCH /api/issues/:id` with `{status}`, `POST /api/issues/:id/comments` with `{body}`, both with `X-Paperclip-Run-Id`. T1 confirms the agent-actor rules for `in_review` in `server/src/services/issues.ts` before relying on them.
- forgectl: unchanged; `surface brief` carries the command, at most 600 characters.

## Orchestrator

Driver: opus. A credential path; implementation and review stay on Opus (Usage Principle U9).

## Tasks

- [x] **T1: adapter run JWT and report script.** Set `supportsLocalAgentJwt: true`. When `ctx.authToken` is absent, run as today and log that status reporting is off. Otherwise write the report script per Design, remove it in `finally`. Tests: script mode 0700 and refuses a planted link; refuses a status other than `in_review`, an oversized or multi-line comment; token absent from the brief and from every log line (checked by planting a canary token).
- [x] **T2: brief ends at in_review.** The brief tells the worker to finish with the exact report command and never to set `done`. README documents the flow and the `authenticated`-mode caveat.
- [x] **T3: live check against the baseline.** On the spike: positive control first, without T1 (the P0 runs above show `issue_disposition_repair` wakes). With T1: the issue is `in_review`; the issue activity log shows the actor as this agent with this turn's run id; `GET /api/agents/:id/wakeup-requests` (or the run list filtered by `wakeReason`) shows no `issue_disposition_repair` for the issue within 90 s (three heartbeats); a canary search of the ledger, the transcript and the run log finds no token.
- [x] **T4: security review before activation.** An Opus security review of the adapter file set (`integrations/paperclip/packages/adapter-herdr/src/*.ts`, the report script template, the Stop hook) and the Paperclip auth paths it relies on (`middleware/auth.ts`, `services/authorization.ts`, the issues routes).

## Related

- forgectl#1244 (rest of P1), forgectl#1239 (env passing, now not needed here), ADR-0010
- PR #1246 (adapter), `integrations/paperclip/README.md`

## Deviations

- T1: Paperclip refuses an agent's `in_review` without a review path and allows one assignee per issue, so the report script also sets `assigneeUserId` to an operator-chosen reviewer (`FORGECTL_PAPERCLIP_REVIEWER_USER_ID`) and clears `assigneeAgentId`. Without a reviewer configured it comments only. Found live: the first T3 run got HTTP 422 "Issue can only have one assignee".
- T3: a successful handoff ends the agent's run as `cancelled` with `issue_reassigned`; that is Paperclip cancelling an agent's run when its issue is reassigned, not a failed turn.
- T4 (Opus security review, 0 Critical, 2 Important, 3 Nit): the worker can read the run JWT, whose standard scope reaches any visible issue in the company; the design's "never sees the token" wording was wrong and is corrected in code comments, README and here. A token stays write-capable until expiry unless the run ends `cancelled`; the README now recommends `PAPERCLIP_AGENT_JWT_TTL_SECONDS` near the turn timeout. Nits fixed: the bearer header goes to curl through a file descriptor (no argv), the script is named by run id, and the summary must be printable ASCII.

## Learnings

- T3 evidence (spike, 2026-10-12): `PAP-23` ended `in_review`, assigned to `local-board`, with the comment and the update recorded as the agent under run `7f347b0d`; no `issue_disposition_repair` or other automation run followed; a JWT-shaped canary search of the run log, the forgectl ledger and the worker transcript found nothing, with a positive control matching.
