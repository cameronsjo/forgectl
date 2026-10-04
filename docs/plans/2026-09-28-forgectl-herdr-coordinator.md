---
status: approved
branch: plan/herdr-coordinator
approved_in: let-s-level-up-our-eager-owl
approved_session_id: 95a2c916-a3c7-4bd0-9c2b-be2a99c294e0
next: T0 security review of the T0 file list, then T1 (start with the trust-inheritance check)
---

# forgectl: a coordinator over herdr worker panes

## Goal

One Claude session acts as a coordinator. It splits an ask into tasks, starts one worker per task in its own git worktree inside herdr, briefs it, tracks it, and reads back its report. v1 workers are claude and codex. pi is refused in v1 because `forgectl launch` passes it no permission or sandbox flags (`internal/launch/launch.go:159-168`). The operator watches and steps into any worker from herdr's sidebar.

This carries the hosted "Projects" coordinator-and-threads pattern into the terminal, without the extra machinery of the third-party herdr-projects plugin. The coordinator gets its per-session hooks, background PR poller, auto-merge skill and edits to other harnesses' config files from nowhere.

## Why forgectl, not a skill or a plugin

- `forgectl launch` already owns harness launch posture (permission mode, model) for claude, codex and pi (`internal/launch/launch.go:27`, `internal/launch/invocation.go:174-208`). The network route comes from `[proxy] launch_profile` and is injected on both the launch and surface paths (`internal/cli/launch_env.go:28-35`).
- `forgectl surface launch <dir> --surface herdr --name <n>` already starts a harness in a new herdr workspace without the manager seeing its arguments (`internal/cli/surface.go:43-100`).
- Putting the worker verbs next to those keeps one owner for "how an agent starts".

A skill still carries the coordinator's *behavior* (how to split work, when to trust a report). forgectl carries the *mechanism*.

## Evidence: 2026-09-28 live trial (work machine, herdr 0.9.1 fork, Claude Code 2.1.283)

The coordinator was an ordinary Claude session driving herdr's CLI by hand. The loop worked end to end: worktree per task, brief, wait, read report, clean up. Only one of six worker CLIs completed, and herdr's lifecycle state was wrong every time it mattered.

| Worker | Result | Failure the coordinator must handle |
|---|---|---|
| claude | report returned (`17*23=391`) | herdr reported `blocked` while it sat at its input prompt (plan-mode footer misread) |
| pi | no reply in 5+ min | stuck loading a large local model; herdr reported `idle` |
| codex | prompt lost | `codex` was an npx shim; herdr reported ready at npm's "Ok to proceed?" and the brief answered it |
| internal gateway CLI | did not start | version picker pointed at a missing build |
| two other agent CLIs | did not start | missing config / deleted venv |

Other findings:

- A new worktree workspace's root pane was taken over by a herdr-plus layout that ran Claude in it, so `herdr agent start --pane <root>` failed `agent_pane_busy`.
- Every worker loaded the full global plugin and rules context: a one-line answer cost 69k tokens (~$0.44). Workers need a lean launch profile.
- Folder-trust dialogs appear in every fresh worktree path.

## Proposed verbs (extend `forgectl surface`)

```sh
forgectl surface launch <repo> --surface herdr --worktree <branch> --harness claude|codex --name <n> [--profile worker]
forgectl surface ready  <n> [--timeout 120s]   # exit 0 only when the harness is at its input prompt
forgectl surface brief  <n> <prompt|@file>     # re-check ready, submit, confirm the turn started
forgectl surface wait   <n> [--timeout]        # until the turn settles; own state, not herdr's alone
forgectl surface read   <n> [--report]         # screen tail, or the worker's REPORT line
forgectl surface list   [--json] [--orphans]   # workers in this repo + herdr session: name, harness, branch, state
forgectl surface close  <n> [--keep-worktree]  # close the owned workspace; remove the worktree only if provably safe
```

Only `surface launch` exists today (`internal/cli/surface.go:57`); every other verb is new.

### Harness selection

`--harness` adds an override field to `InvocationRequest` (`internal/launch/invocation.go:87-102`), which today takes the harness only from the profile matched by directory (`internal/launch/profile.go:139-185`). When the override differs from the matched profile's harness, the profile's model does not apply. Posture never loosens: for each posture field, the worker takes the stricter of the matched profile and the worker profile, so a strict per-repo posture is not replaced by a looser worker default. The validation at `invocation.go:299-300` stays, and v1 also refuses pi (see Goal). Another harness is a later change to `ResolveBinary`, `selectPosture`, and `Profile.Validate`.

### One owned workspace per worker

forgectl's ownership marker lives in the herdr workspace label, and `Close` acts on whole workspaces (`internal/surface/herdradapter/start.go:38-54`, `close.go:37-54`). v1 keeps that model: each worker gets its own forgectl-created workspace, and `close` closes that workspace. The adapter types the bootstrap into the workspace's root pane (`start.go:106-131`). T1 confirms the root pane is an idle shell before typing, and fails naming the pane's command if anything else holds it (the herdr-plus layout case). No pane splitting in v1.

`--name` is the ledger key and is carried in the ownership label. herdr's create accepts only `label` (`start.go:38-43`), so the sidebar shows the ownership label, not a free-form display name.

### Worktrees

`projects.Worktree` makes a fresh bare clone and refuses an existing directory (`internal/projects/worktree.go:47`), so T1 adds a helper that runs `git -c core.hooksPath=/dev/null worktree add` against the existing checkout. Disabling hooks stops a fetched branch from running its own `post-checkout` hook (husky, `.githooks`) before any trust dialog. Worktrees go under `<repo>/.claude/worktrees/<name>`, which keeps the cwd inside the repo so the repo's launch profile still matches by path prefix (`profile.go:149`).

That location depends on Claude Code not inheriting folder trust from the already-trusted repo above it. T1 checks this first: launch claude in a new worktree under a trusted repo and confirm the trust dialog appears. If it does not, worktrees move outside the repo and T1 adds an explicit profile match for them.

### Ledger

- **Location:** `$XDG_STATE_HOME/forgectl/surface/` (default `~/.local/state/forgectl/surface/`), created with the symlink refusal that usage data already uses (`internal/config/usage_base.go:42-73`). Ledger files are mode 0600.
- **Key:** the repo's top-level path plus the herdr session name. Not the coordinator's session id: `/clear` starts a new id, and the old workers would drop out of `list` while their workspaces and worktrees live on.
- **Row:** name, harness, branch, worktree path, started-at, and the encoded `backend.Ref` (`backend/ref.go:151-156`, `:655-674`). `close` and `list` act through `Adapter.Close` and `Adapter.Probe`, which need the full `Ref` (server incarnation and ownership tag), not raw pane or workspace ids.
- **Write order:** `launch` writes a `pending` row before `git worktree add`, then fills it in after each step (worktree, workspace, harness). A launch that fails partway leaves a row that names what it created, so `list --orphans` can find it.
- **Reconcile:** `list` checks each row against herdr and git and reports one of three states per worker: `present`, `gone`, or `unreadable`. A herdr error (for example `protocol_mismatch` between client and server) is `unreadable`, never `gone`. `close` and any pruning refuse on `unreadable`.

Workers do not die with the coordinator: herdr keeps their workspaces running. `list --orphans` shows rows whose workspace is gone or whose launch never finished, and `close` works on any row, so a crashed coordinator's workers can be cleaned up by the next one.

## Readiness and state: don't trust herdr's lifecycle alone

`ready` and `wait` combine three signals, and a harness counts as ready only when all agree:

1. herdr agent status (a hint, never proof). T2 names the herdr CLI call that returns it.
2. A per-harness screen predicate: the harness's own input prompt is visible, and no known blocking screen is showing.
3. For `wait`: the screen has been stable for N seconds after a state change.

Known blocking screens in v1: the folder-trust dialog, Claude Code's plan-approval dialog, any y/N permission prompt, npm's "Ok to proceed?", a model-loading screen, and a version picker. `ready` fails naming the screen it sees. It never answers one.

Per-harness predicates live in data (TOML), not code, so a new screen is a config row. They load only from forgectl's own config directory, never from the worktree or repo, so a branch cannot mark its trust dialog as ready. A harness with no predicate table fails `ready`.

`brief` must never answer a dialog. In Claude Code's trust and permission dialogs, Enter picks the highlighted "Yes", and a dialog can appear between any check and the send. So `brief` sends in two steps: it types the text without Enter, reads the screen back to confirm the text sits in the harness's input box with no dialog showing, and only then sends Enter as a separate call. It then requires a `working` state within 5 seconds.

Each brief carries a random per-brief marker and asks the worker to end with `REPORT <marker>: …`. `read --report` takes only text after the last echo of the brief, so it cannot match the brief's own instruction. The coordinator verifies against git (`git -C <worktree> status`, `git log`), never the report alone. The existing `herdr-orchestrator` skill's untrusted-worker rules apply unchanged.

T2 and T3 list every herdr CLI call each verb makes (screen read, text send, Enter, status) and add a sensitive-runner exec kind for each (`internal/exec/sensitive.go:101-148`); none exists in the adapter today, and `pane run` currently reuses `KindHerdrCreate` (`start.go:106`). Sending text is a new write capability, so it gets its own kind, separate from screen reads, and is allowed only against a `Ref` whose ownership tag matches a ledger row. It can never reach the operator's own panes or a workspace forgectl did not create.

## Closing a worker without losing work

`close` removes the worktree only when all of these hold. Otherwise it closes the workspace, keeps the worktree, and prints which check failed:

- `git status --porcelain --ignored` is empty. Ignored files (`.env`, notes, build output) count as work.
- HEAD is on a branch. A detached HEAD's commits are reachable only through the worktree's own reflog, which removal deletes.
- The branch has no commits missing from its upstream, or it has no upstream and no commits beyond the base it was created from.
- No stash entry was created on this branch.

`close` never deletes the branch. It re-derives the worktree path from `git worktree list` and requires it to sit under `<repo>/.claude/worktrees/`; it never trusts the path stored in the ledger row.

## Worker launch profile

`--profile worker` sets an explicit posture rather than inheriting the operator's. The built-in defaults are `permission_mode = "plan"` and `allow_danger = true`, and codex defaults to `sandbox = "read-only"` (`internal/launch/profile.go:107-113`). A plan-mode worker cannot write to its worktree and ends its turn at the plan-approval dialog, and `allow_danger` emits `--allow-dangerously-skip-permissions` (`launch.go:27-30`).

v1 worker posture:

- claude: `permission_mode = "acceptEdits"`, `allow_danger = false`. Edits in the worktree go through; shell commands still prompt, and a prompt is a blocking screen that `ready`/`wait` report.
- codex: `sandbox = "workspace-write"`, approvals on request.
- A lean context: `Profile` has no field for plugins or rules today, so T5 adds one that points at a worker settings file passed to the harness. The nearest existing control is `StrictMCP` (`profile.go:82-96`).

No pre-trust. Claude Code's folder-trust dialog guards repo-supplied hooks, MCP servers, and `CLAUDE.md`, and `--worktree <branch>` can name a branch fetched from a remote. The trust dialog is a `ready` failure that names it, and the operator answers it in the worker's pane. Trust is never written to the harness's config by forgectl and never typed by the coordinator.

## Out of scope (refused, per the 2026-07-15 steal/refuse study)

- No standing daemon, PR poller or auto-merge. Merge stays manual.
- No persistent worker identity across repos or herdr sessions.
- No cross-machine dispatch in v1 (herdr `--machine` exists; revisit later).
- No harnesses beyond claude and codex in v1. pi waits until `forgectl launch` can pass it a permission or sandbox posture.
- herdr-plus layout files keep their own launch flags for now. Moving them onto forgectl is a later change in the herdr-plus repo.

## Tasks

- [ ] T0: security review of the code these tasks extend, before T1: `internal/surface/herdradapter/{start,close,herdradapter}.go`, `internal/exec/sensitive.go`, `internal/launch/{profile,launch,invocation}.go`, `internal/config/usage_base.go`. Repeat it over the T2, T3, and T5 diffs before T7.
- [ ] T1: `surface launch --worktree --harness --name`: harness override on `InvocationRequest`, `git worktree add` helper under `<repo>/.claude/worktrees/`, one owned workspace per worker with an idle-root-pane check, pending-then-filled ledger rows. First, the trust-inheritance check under Worktrees.
- [ ] T2: per-harness readiness predicates (TOML) and `surface ready`, with fixtures from the trial screens and every blocking screen listed above; name the herdr calls and add their exec kinds.
- [ ] T3: `brief` (type without Enter, read back, then Enter, then confirm `working`), `wait`, `read --report` with per-brief markers.
- [ ] T4: `list` (three-state reconcile, `--orphans`) and `close` (the four removal checks, never deletes the branch, refuses on `unreadable`).
- [ ] T5: `--profile worker`: explicit posture per harness, stricter-of merge with the matched profile, worker settings file field, no pre-trust.
- [ ] T6: coordinator skill: split, dispatch, verify-against-git, report. It extends `herdr-orchestrator`, which lives in the cadence plugin monorepo (`cameronsjo/cadence`), so T6 is a separate PR there after T1–T5 ship.
- [ ] T7: re-run the trial with claude and codex as the acceptance test, on a named machine and herdr build with matching client and server protocol versions.

## Verification

- Unit: each readiness predicate is tested against a captured screen that must pass and one that must fail (trust dialog, plan-approval dialog, y/N prompt, npm prompt, model loading).
- `brief` negative controls: a blocking screen that appears after `ready` passed, and one that appears between typing and Enter, are both caught, and Enter is never sent.
- `read --report` negative control: a screen showing only the echoed brief yields no report.
- `close` negative controls, one per removal check: an ignored file, a detached HEAD with a commit, an unpushed commit, and a stash entry each keep the worktree. The branch survives every `close`.
- `list` negative control: a herdr call that fails (for example, a protocol mismatch) yields `unreadable`, and `close` refuses.
- Exec-kind negative control: a text send aimed at a `Ref` with no matching ledger row is refused.
- Posture negative control: a repo profile stricter than the worker profile stays strict under `--harness`.
- Partial launch: a failure injected after `git worktree add` leaves a ledger row that `list --orphans` shows.
- Acceptance: T7. Every worker either returns a `REPORT` line or is reported by `ready`/`wait` with the specific blocking screen, never a false "ready".

## Panel

Panel: plan-reviewer + red-team + security-posture — revised 2026-10-04 against forgectl source; security-posture found the first revision weakened (brief could answer a dialog, unrestricted pi, posture override, worktree hooks), all four fixed here.

### Panel review findings declined

none declined

## Orchestrator

**Driver:** opus
