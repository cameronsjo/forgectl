---
status: proposed
branch: plan/herdr-coordinator
approved_in: let-s-level-up-our-eager-owl
approved_session_id: 3af13e43-fcc9-4812-a361-71b08655a291
next: run the plan panel against forgectl source, then approve or revise
---

# forgectl: a coordinator over herdr worker panes

## Goal

One Claude session acts as a coordinator. It splits an ask into tasks, starts one
worker per task in its own git worktree inside herdr, briefs it, tracks it, and reads
back its report. A worker can be any CLI agent herdr hosts: claude, codex, pi, and
others. The operator watches and steps into any worker from herdr's sidebar.

This carries the hosted "Projects" coordinator-and-threads pattern into the terminal,
without the extra machinery of the third-party herdr-projects plugin. The coordinator
gets its per-session hooks, background PR poller, auto-merge skill and edits to
other harnesses' config files from nowhere.

## Why forgectl, not a skill or a plugin

- `forgectl launch` already owns harness launch posture (permission mode, model, network
  route) for claude, codex and pi.
- `forgectl surface launch <dir> --surface herdr --name <n>` already starts a harness in
  a new herdr workspace without the manager seeing its arguments.
- Putting the worker verbs next to those keeps one owner for "how an agent starts".
  Layout files (herdr-plus worktree/project templates) stop carrying their own
  permission flags and call forgectl instead.

A skill still carries the coordinator's *behavior* (how to split work, when to trust a
report). forgectl carries the *mechanism*.

## Evidence: 2026-09-28 live trial (work machine, herdr 0.9.1 fork, Claude Code 2.1.283)

The coordinator was an ordinary Claude session driving herdr's CLI by hand. The loop
worked end to end: worktree per task, brief, wait, read report, clean up. Only one of
six worker CLIs completed, and herdr's lifecycle state was wrong every time it mattered.

| Worker | Result | Failure the coordinator must handle |
|---|---|---|
| claude | report returned (`17*23=391`) | herdr reported `blocked` while it sat at its input prompt (plan-mode footer misread) |
| pi | no reply in 5+ min | stuck loading a large local model; herdr reported `idle` |
| codex | prompt lost | `codex` was an npx shim; herdr reported ready at npm's "Ok to proceed?" and the brief answered it |
| internal gateway CLI | did not start | version picker pointed at a missing build |
| two other agent CLIs | did not start | missing config / deleted venv |

Other findings:

- A new worktree workspace's root pane was taken over by a herdr-plus layout that ran
  Claude in it, so `herdr agent start --pane <root>` failed `agent_pane_busy`. Fixed in
  the layout by making tab 1 a plain shell. forgectl should not depend on that: split its
  own pane.
- Every worker loaded the full global plugin and rules context: a one-line answer cost
  69k tokens (~$0.44). Workers need a lean launch profile.
- Folder-trust dialogs appear in every fresh worktree path.

## Proposed verbs (extend `forgectl surface`)

```sh
forgectl surface launch <repo> --surface herdr --worktree <branch> --harness claude|codex|pi|<kind> --name <n> [--profile worker]
forgectl surface ready  <n> [--timeout 120s]   # exit 0 only when the harness is at its input prompt
forgectl surface brief  <n> <prompt|@file>     # submit; fail if not ready
forgectl surface wait   <n> [--timeout]        # until the turn settles; own state, not herdr's alone
forgectl surface read   <n> [--report]         # screen tail, or the text after a REPORT marker
forgectl surface list   [--json]               # workers this coordinator started: name, harness, branch, state
forgectl surface close  <n> [--keep-worktree]  # stop harness, close pane; remove worktree unless dirty
```

State file: one small JSON ledger per coordinator session under forgectl's state dir
(name, harness, pane id, workspace id, worktree path, branch, started-at). No daemon.
`list` reads it and reconciles with `herdr agent list` and `git worktree list`.

## Readiness and state: don't trust herdr's lifecycle alone

`ready` and `wait` combine three signals, and a harness counts as ready only when all agree:

1. herdr `agent_status` (a hint, never proof).
2. A per-harness screen predicate: the harness's own input prompt is visible, and no
   known blocking screen is showing (folder trust, npm "Ok to proceed?", model loading,
   version picker).
3. For `wait`: the screen has been stable for N seconds after a state change.

Per-harness predicates live in data (TOML), not code, so a new CLI is a config row.
Unknown harness kinds fall back to screen-stable-plus-marker only.

The brief always asks the worker to end with `REPORT <name>: …`, and `read --report`
extracts that. The coordinator verifies against git (`git -C <worktree> status`,
`git log`), never the report alone. The existing `herdr-orchestrator` skill's
untrusted-worker rules apply unchanged.

## Worker launch profile

`--profile worker` gives a lean launch: a fixed small plugin set, no always-on rules
beyond safety, the worktree pre-trusted (or trust handled by `ready` with an explicit
operator-approved answer), and the operator's normal permission posture. Never bypass
permissions by default.

## Out of scope (refused, per the 2026-07-15 steal/refuse study)

- No standing daemon, PR poller or auto-merge. Merge stays manual.
- No persistent worker identity. Workers die with the coordinator session.
- No cross-machine dispatch in v1 (herdr `--machine` exists; revisit later).

## Tasks

- [ ] T1: `surface launch --worktree --harness --name` + ledger write. Split a fresh pane; never reuse the workspace root pane.
- [ ] T2: per-harness readiness predicates (TOML) + `surface ready`, with fixtures from the trial screens above.
- [ ] T3: `brief` / `wait` / `read --report`.
- [ ] T4: `list` (ledger reconciled with herdr and git) and `close` (refuses to remove a dirty worktree).
- [ ] T5: `--profile worker` lean launch.
- [ ] T6: coordinator skill: split, dispatch, verify-against-git, report. Extends `herdr-orchestrator` from one Codex pane to N workers.
- [ ] T7: re-run the six-CLI trial as the acceptance test.

## Verification

- Unit: each readiness predicate is tested against a captured screen that must pass and one that must fail (trust dialog, npm prompt, model loading).
- Acceptance: T7 trial. Every worker that starts either returns a `REPORT` line or is reported by `ready`/`wait` with the specific blocking screen, never a false "ready".
- `close` negative control: a worktree with an uncommitted file is kept.

## Panel

Panel: none — a plan draft to carry home for review in the forgectl repo; the panel runs there against the real source.

## Orchestrator

**Driver:** opus
