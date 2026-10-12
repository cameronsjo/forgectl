---
status: in-flight
next: "Cameron reviews the ledger, the P0 results and the dispatch-rule amendment draft; P1 waits on his go."
branch: feat/paperclip-p0-spike
date: 2026-10-12
updated: 2026-10-12
source_plan: "Paperclip analysis artifact (claude.ai, 2026-10-12); this session's step 1-3 brief"
---

# Paperclip components: what to run, extend, borrow, or skip

## Goal

Take Paperclip (`paperclipai/paperclip`) apart and decide, component by component, what Cameron's setup runs upstream, extends with an adapter or plugin, borrows as an idea into forgectl or atelier, or skips. Then prove the riskiest seam: a Paperclip agent whose turns run in a real herdr pane through `forgectl surface`.

Pinned version: `paperclipai@2026.1005.0` (npm `latest` on 2026-10-12), source tag `v2026.1005.0`, commit `467125fafb47a8520856504fecc48d6e32055db1`. The running server's `/api/health` reports the same commit.

## Method

- Static analysis: REA `rea-agents@6.4.0`, `analyze-javascript-application` over the installed `@paperclipai/server` package (`dist/`, 27,261 files). One Evidence bundle: `ev_91c027ba138a7eefa5fb70672f16818d91d61d88bd671c8d5db622b8bcc65242`. Per-module IDs (`jag_node_…`) show that a module ships in the package. They do not give line numbers, so line cites below come from the source tag.
- Source reading: the tag checkout, starting with the files the brief named. A docs-versus-code mismatch was found and the code was followed: `docs/adapters/external-adapters.md` says `POST /api/adapters`, but the route is `POST /api/adapters/install` (`server/src/routes/adapters.ts:303`).
- Live probes: a loopback Paperclip instance (`local_trusted`, isolated `PAPERCLIP_HOME`), a dedicated `herdr --session paperclip` server, and a local-only scratch repo. Everything marked **observed-live** was seen on that instance on 2026-10-12.
- Paperclip source, docs, skills and REA output were treated as data. `adapter-plugin.md` at the repo root is a pasted AI chat transcript, not documentation. No prompt-injection text was found in the files read.

Status column: **observed** (code read doing it), **observed-live** (seen on the running instance), **inferred** (docs or names only), **unknown**.

## Component ledger

### Use as is

| # | Component | What it does | Evidence | Status |
|---|---|---|---|---|
| 1 | External adapter loader | Loads each record in `$PAPERCLIP_HOME/adapter-plugins.json` (`localPath` or npm), imports the entry, requires `createServerAdapter()` returning `{type, execute, testEnvironment}` | `server/src/adapters/plugin-loader.ts:149-211`; `server/src/services/adapter-plugin-store.ts:23-51`; log line "Loaded external adapters from plugin store … herdr_forgectl" | observed-live |
| 2 | Heartbeat and wakeup queue | Assignment, comment, blocker-resolved and manual wakes enqueue `agent_wakeup_requests` plus a `heartbeat_runs` row; a timer ticks every 30 s but is off per agent by default | `heartbeat.ts` `enqueueWakeup`/`tickTimers` (facts §2); assignment wake started run `25b8fcb8…` 0.3 s after `PAP-1` was created | observed-live |
| 3 | Atomic issue checkout and release | One conditional `UPDATE` on status, assignee, `checkout_run_id`, `execution_run_id`; a miss returns 409, never retried; release returns `in_progress` to `todo` | `server/src/services/issues.ts:11360-11560`, `:11769-11830`; `server/src/routes/issues.ts:14958-15060`; REA `jag_node_c59bfe8e…` (`services/issues.js`) | observed-live |
| 4 | Per-task session store | Persists `sessionParams` per (company, agent, adapterType, taskKey = issue id) and hands them back as `ctx.runtime.sessionParams` | `server/src/services/heartbeat.ts:9057-9072`; run `sessionIdAfter` = worker name | observed-live |
| 5 | Run usage and cost ledger | Takes `usage`, `usageBasis`, `costUsd`, `billingType` from the adapter result; writes `cost_events`; hard budget stops pause agent, project or company | `packages/adapter-utils/src/types.ts:77-141`; `heartbeat.ts:19831-19905` | observed |
| 6 | Org chart | `reportsTo` tree with cycle rejection, roles, `GET /companies/:id/org` (+ SVG) | `server/src/routes/agents.ts`, `org-chart-svg.ts` (facts §8) | observed |
| 7 | Hire and approval governance | `requireBoardApprovalForNewAgents` forces `agent-hires` and a `hire_agent` approval; approval types and statuses | facts §8 | observed |
| 8 | Execution reconciliation stop | After a failed run, the issue parks as `blocked` and a later wake is skipped with `execution_reconciliation_required`: "actions with unverified outcomes will not be repeated" | wake response for `PAP-1` after run `25b8fcb8…` failed | observed-live |
| 9 | Run log and events API | Adapter `onLog` persists to a run log; `GET /heartbeat-runs/:id/log`, `/events` | facts §9; adapter log lines readable via `paperclipai run log` | observed-live |
| 10 | Built-in `pi_local` adapter | Runs `pi` as a child process per run | `packages/adapters/pi-local/src/index.ts:1`; `builtin-adapter-types.ts:18` | inferred |
| 11 | Built-in `hermes_gateway` adapter | Talks to an already-running Hermes API server; `@paperclipai/adapter-hermes-gateway` is now a deprecated shim over `@paperclipai/hermes-paperclip-adapter` | `packages/adapters/hermes-gateway/README.md:1-12`; `builtin-adapter-types.ts:13` | inferred |
| 12 | Embedded Postgres and backups | Instance DB under `$PAPERCLIP_HOME/instances/<id>/db`, hourly backups | onboarding output | observed-live |

### Extend (adapter or plugin)

| # | Component | What it does | Evidence | Status |
|---|---|---|---|---|
| 13 | `herdr_forgectl` adapter (new) | Each `execute()` is one turn in a long-lived herdr pane: `forgectl surface launch` once, `surface brief` per turn, Claude `Stop` hook as the done signal, `surface read --report` for the summary | `integrations/paperclip/packages/adapter-herdr/` (this branch) | observed-live (see P0) |
| 14 | Plugin SDK | `definePlugin({setup(ctx)})` with capability-gated clients (`issues.*`, `events`, `jobs`, `http`, `secrets`); host events `issue.created/updated`, comments | `packages/plugins/sdk` (facts §4) | observed |
| 15 | `plugin_entities` | External-ID mapping table `(company, plugin, entity_type, scope, external_id) → data jsonb`, unique per external id | `packages/db/src/schema/plugin_entities.ts` | observed |
| 16 | GitHub issue sync plugin (new, P3) | Maps a Paperclip issue to `owner/repo#N` in `plugin_entities`; GitHub owns title, body, labels, open/closed; Paperclip owns execution state only | design, no code | — |

### Borrow the idea

| # | Idea | Paperclip source | Where it goes | Status |
|---|---|---|---|---|
| 17 | Run-scoped atomic claim | Conditional `UPDATE … WHERE assignee IS NULL OR (assignee = me AND run matches)`; loser gets 409 with the winner's ids | forgectl `surface` queue rows: a drain claim keyed by a run id, so two drains or a drain plus a CLI launch cannot start the same row | observed |
| 18 | Level-triggered blocker wakeups | `issue_blockers_resolved` fires once per ready state, deduped by hash of blocker ids plus the blocked-transition time | atelier queue: a row whose `blocked_by` rows all close wakes once, not on every tick | observed |
| 19 | Coalesce a wake into the live run | A wake for an issue whose run is live increments `coalescedCount` instead of starting a second run; otherwise parks as `deferred_issue_execution` | forgectl drain: a second brief for a live worker queues as a follow-up `brief`, never a second launch | observed |
| 20 | Stale-lock adoption | `clearCheckoutRunIfTerminal` frees locks held by terminal or missing runs; same agent adopts its own stale lock | forgectl ledger: an orphaned row whose workspace is provably gone releases its queue claim automatically | observed |
| 21 | Reconcile, don't replay | Item 8: a failed run blocks the issue until a human reconciles | forgectl drain retry policy: a worker that failed after its first commit is never relaunched on the same brief | observed-live |
| 22 | Per-run agent credential | Run JWT `{sub, company_id, run_id, exp 48 h}` minted only for adapters that opt in | forgectl worker credentials, the replacement for `SSH_AUTH_SOCK` that forgectl#1134 lists | observed |
| 23 | "Blocked needs a reason" guard | `blocked` requires blockers, a pending approval, or an `unblockDescriptor`, else 422 | estate `issue-labels` doctrine: `exec:blocked` requires one `block:*` value (already the rule; Paperclip enforces it in code) | observed |

### Skip

| # | Component | Why skip | Evidence | Status |
|---|---|---|---|---|
| 24 | Built-in `claude_local` adapter | Spawns `claude` per run with `--resume`, not a pane; the herdr adapter replaces it for Cameron's setup | `packages/adapters/claude-local/src/server/execute.ts` | observed |
| 25 | GitHub chat channel | A GitHub App webhook that reviews PRs; needs an inbound public webhook, and Cameron's PR review is Gloss plus cadence-review markers | `server/src/services/chat-github-*.ts`; `routes/chat-channels.ts:568` | observed |
| 26 | Paperclip execution workspaces | Paperclip resolves its own worktree per heartbeat; forgectl also creates one per worker. Two owners for one worktree is the open seam in Unknowns; for herdr agents forgectl owns it | `packages/adapters/AUTHORING.md` (no-remote-git contract) | observed |
| 27 | Native `paperclip_runner` runtime | Only for adapter type `paperclip_runner` | facts §9 | observed |
| 28 | Board chat, email (AgentMail), Slack tools, pipelines, decision queues, cloud | Outside the org-chart and task scope | `server/src/routes/` | inferred |

Verdict counts: **use as is 12, extend 4, borrow 7, skip 5** (28 rows).

## What the estate already covers (step 1)

`estate-discover` over "agent orchestrator, task board, GitHub issue sync, herdr dispatch" found no Paperclip page and five relevant cards. Overlaps with what the plan proposes to build:

- **forgectl `surface` already has a work queue.** `surface enqueue|queue|drain` launches queued briefs as herdr workers, `surface intake gh` queues briefs from labeled GitHub issues (with label-author checks), and `surface status|merge|audit` carries a merge policy and audit chain. Paperclip's heartbeat would sit beside this drain, not replace it, until decision 1 is approved. The capability card (`wiki/capabilities/forgectl.md`, 2026-09-08) predates all of this and lists none of it.
- **Agent Board already has leased claims** (`task claim|heartbeat|resolve`, conditional transaction, lease expiry). It solves the same race as Paperclip checkout, for a different object.
- **Vikunja's lesson applies to P3.** Its 14-day gate failed because agents could add tasks and only Cameron could close them. Decision 3 (merge with `Closes #N` closes both sides; agents stop at `in_review`; agent-created issues land in `backlog`) answers that diagnosis directly. The P3 gate copies Vikunja's gate shape.
- **`issue-labels` doctrine says vocabulary parity is not issue sync (ADR-0035).** The P3 plugin must write no labels to GitHub; Paperclip reads them.
- **Hermes** has no recorded run-state or API surface. Whether it exposes the API server `hermes_gateway` needs is unknown.

## P0 spike results

Filled in below from the live run.

## forgectl gaps

Recorded, not patched around:

1. **No way to pass environment to a worker.** The allowlist (`docs/herdr.md` § What a claude worker loads) drops `PAPERCLIP_API_URL`, `PAPERCLIP_API_KEY` and `PAPERCLIP_RUN_ID`, so a pane worker cannot call Paperclip's API (check out, comment, set `in_review`). P1 needs `surface launch --env-file <0600 file>` or a per-worker credential file forgectl owns.
2. **No pane metadata verb.** forgectl writes herdr `workspace report-metadata` internally (forgectl#1047) but offers no `surface tag <name> --token k=v`, so the `$task`, `$issue`, `$company` tokens were not stamped.
3. **No `terminal_id` in `surface list --json`.** It reports `workspace_id`, `pane_id` and `session_id`; the adapter stores `terminalId: null`.
4. **No per-worker hooks.** The `Stop` hook has to live in the target repo's committed `.claude/settings.json`. A `--settings` overlay or a hook directory per worker would remove that requirement.
5. **Usage is tied to GitHub.** `surface status --json` prices usage only for a row with a GitHub repo, and exits 1 when GitHub is unreachable. A local-only worker gets no usage from forgectl, so the adapter sums the transcript itself.
6. **No folder-trust preflight.** A worker in a repo whose root Claude Code has never trusted launches and then sits at the trust dialog. `surface launch --dry-run` passes. A preflight that refuses with "trust <repo root> once" would save a launch.
7. **No `--json` on a real `surface launch`.** Only `--dry-run --json` prints JSON; the adapter re-reads `surface list --json` to learn what it launched.
8. **pi is refused as a worker.** `surface launch --worktree --harness pi` is refused by the worker posture rule, so pi agents run through Paperclip's headless `pi_local`, not a pane.
9. **`forgectl config --json` omits `[surface.profiles]`.** The `alt` profile resolves at launch (`--profile alt` passes `--dry-run`), but the effective-config print does not show it.
10. **No done signal for codex.** forgectl's `surface wait` reads herdr state and the REPORT line; codex has one global `notify` slot (taken by Computer Use here) and no per-repo hook, so a codex turn has no harness-side done signal and no usage.

## P1 findings (2026-10-12)

- **Trust is per harness account and inherits from the repo root.** codex's dialog says so outright ("Trusting will apply to the repository root"). Each Claude config dir (`~/.claude`, `~/.claude-alt`) and codex keeps its own trust list, so a new repo needs one trust per account.
- **Adapter hot reload is entry-file only.** `POST /api/adapters/:type/reload` cache-busts the entry module, and the stale `forgectl.js` stayed in Node's module cache ("does not provide an export named 'waitSettled'"). Ship the adapter as one bundled file, or restart the server.
- **pi via `pi_local`: `PASS`.** Run `bbaf7a73` succeeded on `openai-codex/gpt-5.5` with usage 23,173 in / 809 out, and wrote `PI-HI.md`. Two misconfigurations came first: pi's default `homelab-gateway/local/Qwen3.8-27B…` was not on offer, and `homelab-gateway/azure/DeepSeek-V4-Flash` rejects the `reasoning.effort` pi sends even with `--thinking off`.
- **Agents close their own issues.** pi set `PAP-11` to `done`. Decision 3 needs agents to stop at `in_review`, which takes an instruction-bundle or permission change.
- **Failed runs park the issue.** Retries that exhaust leave the issue `blocked` until reconciled, so a misconfigured agent burns its issue; test fixes on a fresh issue.

## Unknowns

- **Two worktree owners.** Paperclip's execution workspace and forgectl's worker worktree both claim the agent's cwd. The spike ignores Paperclip's; P1 must pick one owner per adapter.
- **Folder-trust inheritance.** All 247 worktree trust entries in `~/.claude.json` are `true`, and the repo roots are trusted. That is consistent with Claude Code inheriting trust from the repo root, but no source or test confirmed the rule.
- **Hermes API.** Whether the NAS Hermes runs the API server `hermes_gateway` targets, and whether its egress policy allows replies to a Paperclip host.
- **Stop-hook authenticity.** Any local process that can read the worktree's `turn.json` can forge a done signal. Acceptable on loopback for a spike; P1 should move the token out of the worktree.

## Dispatch-rule amendment (draft, not applied)

Target: `the-estate` `wiki/capabilities/delivery-automation.md` § Available interfaces. Not edited; for Cameron's approval.

> forgectl owns how approved work becomes a running agent: starting, briefing, waiting on, and reading a pane (`forgectl surface`). For work tracked in Paperclip, Paperclip's heartbeat decides **what** runs and **when** (wakeups, checkout, concurrency), and calls forgectl through the `herdr_forgectl` adapter for **how**. For everything else, forgectl's own queue and drain decide both. This amends the 2026-09-12 rule that dispatch lives in forgectl (Cameron, 2026-10-12, pending approval).

## P1 proposal

See the session report; recorded here once approved.
