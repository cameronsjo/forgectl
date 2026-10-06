---
status: in-flight
branch: plan/herdr-coordinator
approved_in: let-s-level-up-our-eager-owl
approved_session_id: 95a2c916-a3c7-4bd0-9c2b-be2a99c294e0
next: T5 (worker profile and the ADR-0010 hardening floor) before any T8 run; forgectl#1077 (close after a herdr restart) and #1079 (hooks on git status; gitfile read) before T8 runs unattended; forgectl#1051 Option B waits on a herdr call that starts a pane with a command
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

`--name` is the ledger key. It is not carried in the ownership label: `Close`, `Probe` and reconcile match that label exactly against the tag alone, and adding the name would weaken the match (see Deviations). herdr's create accepts only `label` (`start.go:38-43`), so the sidebar shows the ownership label, not the worker name; forgectl#1047 shows the name through `workspace report-metadata` instead.

### Worktrees

`projects.Worktree` makes a fresh bare clone and refuses an existing directory (`internal/projects/worktree.go:47`), so T1 adds a helper that runs `git -c core.hooksPath=/dev/null worktree add` against the existing checkout. Disabling hooks stops a fetched branch from running its own `post-checkout` hook (husky, `.githooks`) before any trust dialog. Worktrees go under `<repo>/.claude/worktrees/<name>`, which keeps the cwd inside the repo so the repo's launch profile still matches by path prefix (`profile.go:149`).

That location depends on Claude Code not inheriting folder trust from the already-trusted repo above it. T1 checked this first. Claude Code 2.1.289 does inherit trust from any trusted ancestor, so a worktree under a trusted repo gets no dialog. Worktrees stay under `<repo>/.claude/worktrees/` by Cameron's decision (see Deviations): the folder-trust dialog is not a control for workers, and `core.hooksPath=/dev/null` on `worktree add` plus the T0 review are.

### Ledger

- **Location:** `$XDG_STATE_HOME/forgectl/surface/` (default `~/.local/state/forgectl/surface/`), pinned by descriptor with `privdir` and opened with `openat` and `O_NOFOLLOW`, then checked for owner, type, link count and entry identity, as the usage store does (`internal/launch/usage_file_unix.go`; `usage_base.go` only resolves the base). The file name is a hash of the key. Ledger files are mode 0600 in a 0700 directory.
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

`brief` must never answer a dialog. In Claude Code's permission dialogs Enter picks the highlighted "Yes" (2.1.289's trust dialog highlights "No, exit", which Enter would still answer), and a dialog can appear between any check and the send. So `brief` sends in two steps: it types the text without Enter, reads the screen back to confirm the text sits in the harness's input box with no dialog showing, and only then sends Enter as a separate call. It then requires a `working` state within 5 seconds.

Each brief carries a random per-brief marker and asks the worker to end with `REPORT <marker>: …`. `read --report` takes only text after the last echo of the brief, so it cannot match the brief's own instruction. The coordinator verifies against git (`git -C <worktree> status`, `git log`), never the report alone. The existing `herdr-orchestrator` skill's untrusted-worker rules apply unchanged.

T2 and T3 list every herdr CLI call each verb makes (screen read, text send, Enter, status) and add a sensitive-runner exec kind for each (`internal/exec/sensitive.go:101-148`). T1 added `herdr.pane-inspect` and gave `pane run` its own `herdr.bootstrap` kind. Exec kinds are labels only; nothing in the sensitive runner decides on them. So "a text send is allowed only against a `Ref` whose ownership tag matches a ledger row" is an adapter check T3 writes (ownership via `locate`, pane belongs to the workspace, ledger match), not a property of the kind. That check stops forgectl from typing into the operator's own panes or a workspace it did not create. It does not stop a worker that drives herdr directly over the socket every pane can reach.

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

- claude: `permission_mode = "acceptEdits"`, `allow_danger = false`. Edits in the worktree go through; shell commands still prompt, and a prompt is a blocking screen that `ready`/`wait` report. **Amended 2026-10-05:** `auto` is also allowed on a machine that opts in, only behind the ADR-0010 hardening floor (T5).
- codex: `sandbox = "workspace-write"`, approvals on request.
- A lean context: `Profile` has no field for plugins or rules today, so T5 adds one that points at a worker settings file passed to the harness. The nearest existing control is `StrictMCP` (`profile.go:82-96`).

No pre-trust. Claude Code's folder-trust dialog guards repo-supplied hooks, MCP servers, and `CLAUDE.md`, and `--worktree <branch>` can name a branch fetched from a remote. The trust dialog is a `ready` failure that names it, and the operator answers it in the worker's pane. Trust is never written to the harness's config by forgectl and never typed by the coordinator.

## Out of scope (refused, per the 2026-07-15 steal/refuse study)

- ~~No standing daemon, PR poller or auto-merge. Merge stays manual.~~ **Reversed 2026-10-05 by the foreman amendment** (see Amendment: foreman queue, drain, and merge gate). Each original ground and what answers it:
  - *Machinery from nowhere* (the herdr-projects plugin's per-session hooks and background poller): the drain is a forgectl verb (`surface drain start|stop|status`), detached, under one global flock, with no session hooks and nothing installed into a harness. Stopping it is one command.
  - *Edits to other harnesses' config*: still refused. The drain launches workers through `surface launch` and writes only forgectl's own state dir.
  - *Unreviewed merges*: merge stays off by default. ADR-0011 allows it per machine only, through a gate check that GitHub's ruleset requires by App id, so no worker can merge by any path. The gate needs named required checks, an approver a worker cannot impersonate, and a per-repo path allowlist, all at one head SHA, with every merge and refusal audited.
- No persistent worker identity across repos or herdr sessions.
- No cross-machine dispatch in v1 (herdr `--machine` exists; revisit later).
- No harnesses beyond claude and codex in v1. pi waits until `forgectl launch` can pass it a permission or sandbox posture.
- herdr-plus layout files keep their own launch flags for now. Moving them onto forgectl is a later change in the herdr-plus repo.

## Tasks

- [x] T0: security review of the code these tasks extend, before T1: `internal/surface/herdradapter/{start,close,herdradapter}.go`, `internal/exec/sensitive.go`, `internal/launch/{profile,launch,invocation}.go`, `internal/config/usage_base.go`. Repeat it over the T2, T3, and T5 diffs before T7. **Amended 2026-10-05:** also repeat it over the T8, T9, and T10 diffs, each before the step that switches its control on.
- [x] T1: `surface launch --worktree --harness --name`: harness override on `InvocationRequest`, `git worktree add` helper under `<repo>/.claude/worktrees/`, one owned workspace per worker with an idle-root-pane check, pending-then-filled ledger rows. First, the trust-inheritance check under Worktrees.
- [x] T2: per-harness readiness predicates (TOML) and `surface ready`, with fixtures from the trial screens and every blocking screen listed above; name the herdr calls and add their exec kinds.
- [x] T3: `brief` (type without Enter, read back, then Enter, then confirm `working`), `wait`, `read --report` with per-brief markers. **Amended 2026-10-05 (T2 security review I1):** the first brief goes in at launch as the harness's initial-prompt argument through the trampoline socket, so it never travels as keystrokes into a live TUI. `ready` reads only signals the worker can set (its screen, and herdr status any pane can report), so typed briefs after the first are guarded against accidents only, and must not carry an approval.
- [x] T4: `list` (three-state reconcile, `--orphans`) and `close` (the four removal checks, never deletes the branch, refuses on `unreadable`).
- [ ] T5: `--profile worker`: explicit posture per harness, stricter-of merge with the matched profile, worker settings file field, no pre-trust.
- [ ] T6: coordinator skill: split, dispatch, verify-against-git, report. It extends `herdr-orchestrator`, which lives in the cadence plugin monorepo (`cameronsjo/cadence`), so T6 is a separate PR there after T1–T5 ship.
- [ ] T7: re-run the trial with claude and codex as the acceptance test, on a named machine and herdr build with matching client and server protocol versions. **Amended:** extended by the foreman acceptance script (see the amendment section).
- [ ] T8 (foreman P2): queue and drain. Own plan at pickup.
- [ ] T9 (foreman P3): intake from GitHub labels and the board. Own plan at pickup.
- [ ] T10 (foreman P4): verdicts, usage, merge gate, closers. Own plan at pickup.

## Amendment: foreman queue, drain, and merge gate (2026-10-05)

The foreman plan (`cadence-ecosystem` `docs/plans/2026-10-05-foreman-a-herdr-work-queue-cockpit-for-claude-code.md`) builds a `/foreman` pane in Claude Code on top of this plan. forgectl stays the engine: the pane and the coordinator skill call `--json` verbs and own no state. It changes this plan as follows.

- **Order.** T2–T4 run as planned. T5 runs next, before any unattended run of T8, because it carries the `auto` hardening.
- **T4 addition.** `surface list --json` rows carry `session_id`, `transcript` (`${CLAUDE_CONFIG_DIR:-~/.claude}/projects/<slug(worktree)>/<id>.jsonl`), `pane_id`, `workspace_id`, `branch`, `repo`, `stage`, and `pr`, so usage and PR state can be joined to a worker without a second ledger.
- **T5 amended (ADR-0010).** Add the hardening floor: only forgectl's settings load (`--setting-sources`, plugins off, `--strict-mcp-config`); forgectl#1050 fixed first; the base commit read from the GitHub API into a forgectl-only ref namespace; a deny-by-default Bash sandbox with `allowUnsandboxedCommands = false`; matching Read/Edit/Write deny rules and WebFetch/WebSearch denied; `useAutoModeDuringPlan = false`; and a short-lived token per worker from a forgectl "worker" GitHub App in place of the keychain `gh` login, revoked on `drain stop` and `close`. The opt-in is refused where the config is chezmoi-managed or the launch context holds Full Disk Access. The mode ranking is forgectl#1043. `auto` stays off until a security review on an Opus-class model has read the file set ADR-0010 names.
- **T8: queue and drain.** `surface enqueue|dequeue|queue`, `surface drain start|stop|status|events`. One `queue.jsonl` beside the per-repo ledgers; a global drain flock with pid and heartbeat; `queued → claimed` as a compare-and-set under the lock; every step `intent → act → confirm` with a startup reconcile; caps global 3 and 1 per repo. Reuses `internal/pr`'s claim and attempt handling (`drain.go`), or the T8 plan records why it cannot.
- **T9: intake.** `surface intake gh` (eligible `exec:*` labels from config, marker label `exec:queued`, author allowlist, fenced body) and `surface intake board`. Intake refuses PRs (the issues API returns them), and requires that the eligible label was applied by an identity other than the worker App.
- **T10: verdicts, usage, merge, closers.** The "merge gate" GitHub App and its `forgectl/merge-gate` check run, with each eligible repo's ruleset requiring it by App id (ADR-0011); `surface status --json` with PR, required checks, approvals, and policy verdict; `surface merge` under ADR-0011; `merge-audit.jsonl` and `surface audit`; close after `MERGED` or a 24h-graced `CLOSED`; `prune` with a daily usage rollup. Live cost comes from a new `cadence-hooks metrics price --transcript` action.
- **T6 grows** a coordinator mode: split an ask, `surface enqueue --batch` per task, point the operator at `/foreman`. It still ships in `cameronsjo/cadence`.
- **T7 grows** `scripts/foreman-acceptance.sh` in the meta-repo, with negative cases: a review at an older SHA does not merge, a racing push does not merge, a worker-posted `cadence-review` marker does not merge, a second `drain start` is refused.
- **herdr package boundary (forgectl#721, agreed).** herdr reads for the drain (agent status hints, pane reads) go through the shared `internal/herdr` client (forgectl#723), with a pinned server passed as a `Runner` that sets `HERDR_SOCKET_PATH`. Starting and closing workers stays in `internal/surface/herdradapter` through the sensitive runner. Readiness predicates live in `internal/herdr/ready`, which T2 creates. An errored herdr read is `unreadable`, never `gone`.

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

## Deviations

- **Folder trust is not a control for workers (T1, Cameron's decision 2026-10-04).** The trust-inheritance check ran on sjomba with Claude Code 2.1.289. A never-seen worktree under the trusted `forgectl` repo opened with no trust dialog; a `/tmp` directory under a profile that does not trust `/` showed the dialog, so the probe could see it. The main profile's `~/.claude.json` trusts `/`, so on that profile the dialog never fires anywhere and moving worktrees outside the repo (the plan's fallback) would change nothing. Worktrees stay under `<repo>/.claude/worktrees/`; `~/.claude.json` is not edited. The controls are `git -c core.hooksPath=/dev/null` on `worktree add` and the T0 review. Cameron removed the `/` trust from the main profile on 2026-10-04 (`projects["/"].hasTrustDialogAccepted = false`, one atomic edit, held after 60 s); the projects root is still trusted, so repos under it still pass trust to their worktrees. That hooks setting stops git hooks only; the branch's own `.claude/settings.json` hooks, `.mcp.json` servers and `CLAUDE.md` still load in an unattended worker, which forgectl#1050 tracks.
- **`--name` is not in the ownership label (T1).** The label is the exact-match ownership marker for `Close`, `Probe` and reconcile (`RecoveryTag.OwnershipName`, "the tag and nothing else"). The name-to-workspace mapping is the ledger's job; the sidebar name is forgectl#1047.
- **Interim worker posture floor (T1, from T0 finding I5/M1).** Until T5, a worker launch (`InvocationRequest.Worker`) allows only the plan's v1 posture and stricter: claude `plan`, `default` or `acceptEdits`; codex `read-only` or `workspace-write` with `untrusted` or `on-request` approvals. pi and anything else are refused; `allow_danger` is turned off and `add_dir` is dropped, so the worker edits only its own worktree. The first cut was a denylist (`bypassPermissions`, `danger-full-access`); polish security review showed `auto`, `dontAsk` and codex `never` passing it. T5 replaces it with the stricter-of merge; the ranking it needs is forgectl#1043.
- **Pane-identity env fix (T1, from T0 finding I1).** Every surface launch strips the launcher's `HERDR_PANE_ID`, `HERDR_TAB_ID` and `HERDR_WORKSPACE_ID`; a herdr launch marks its invocation and the trampoline adds back the new pane's own values. tmux and cmux have the same leak for their own variables: forgectl#1044.
- **New failure class `target-busy` (T1).** The idle-root-pane check refuses a busy pane with `FailureTargetBusy` and keeps the ref so the workspace is closed. The check re-reads the pane for about 1.5 s, because a prompt hook can briefly hold the foreground (1 sample in 75 on herdr 0.9.1). The leader of the foreground group must also be a known shell, because herdr's `shell_pid` is the pane's direct child whatever it is. And the group must hold the shell alone: a wrapper like `bash -lc 'source env.sh; claude'` runs the agent as a child inside the shell's own group, while a prompt hook is a child only for a moment and the re-read waits it out (3 of 3 live launches passed on a shell with starship, atuin and mise hooks). The leader must also be an interactive shell by its argv (no `-c`, no script operand), because a wrapper still sourcing its environment is alone in the foreground but never reads the pane. A pane with no shell yet is re-read, not refused. Across 12 live launches after these rules, 11 passed and 1 failed in the start phase without the busy warning; it did not recur in 6 instrumented runs, and it failed safe (workspace closed, row marked failed). The idle check reads process state only, so it cannot prove the shell has reached its prompt: typed input goes to whatever reads the terminal next, and a shell still loading its rc files passes. The fourth security pass found this; it is a design question for the typed-bootstrap mechanism (which predates T1), tracked as forgectl#1051. Cameron chose Option B on 2026-10-04: herdr starts the trampoline as the pane's command, so nothing is typed; it waits on herdr offering that call, and the idle check is the interim guard. The argv rule is an allowlist of interactive-only flags (`-l`, `-i`, `--login`, `--interactive`). It is check-then-type; binding the handshake peer to the pane is forgectl#1041.
- **Ledger details (T1).** RepoTop resolves a linked worktree to its main checkout, so all launches in a repo share one ledger. A failed row that created nothing (no worktree, ref or recovery tag) does not hold its name. The file check is a copy of the usage store's; one shared helper is forgectl#1048.
- **T0 findings filed rather than fixed in T1:** forgectl#1041 (handshake peer to pane), #1042 (profile `match` through a symlink falls back to defaults), #1043 (validate and rank posture fields for T5), #1044 (tmux/cmux pane env), #1045 (herdr workspace id reuse before T4's `close`), #1046 (`brief` must refuse control characters, for T3). Polish found #1048 (shared file check), #1049 (`projects worktree` runs repo hooks), #1050 (branch-supplied Claude Code config in workers) and #1051 (prompt-ready signal before typing the bootstrap).
- **T2 fixtures are fresh captures, not the trial's (T2).** The trial screens were on the work machine. T2 captured live screens on sjomba with herdr 0.9.1, Claude Code 2.1.289, Codex 0.160.0 and npm: the claude prompt (acceptEdits, plan, text typed), its permission prompt, plan-approval dialog and folder-trust dialog, npm's "Ok to proceed?", an idle shell, and the codex composer. Three blocking screens have no capture and are matched from the trial notes only: codex's approval prompt (the codex model was refused for the account, so no turn ran), model loading, and the version picker. Their rows are marked `uncaptured` in `internal/herdr/ready/predicates.toml`.
- **Predicates live in `internal/herdr/ready`, embedded, with one override file (T2).** The built-in table is a TOML file compiled into the binary; `<config dir>/forgectl/surface-ready.toml` replaces it whole when present (regular file, not group- or world-writable, 64 KiB cap). Nothing is read from a repo or worktree.
- **Screen facts the predicates depend on (T2).** Claude Code puts U+00A0, not a space, between `❯` and typed text; starship's shell prompt uses the same `❯` glyph, so the claude predicate requires the input box's rule lines above and below; every Claude menu draws `❯ N.` on its highlighted row, which the predicates treat as blocking. The plan-approval dialog's highlighted option is "Yes, clear context … and bypass permissions", so an Enter there would switch the worker to bypass mode.
- **`ready` stops at the first blocking screen and retries unreadable reads (T2).** A blocking screen needs the operator, so waiting longer only delays saying so. One failed herdr call says nothing about the worker, so it is retried until the timeout. herdr's `done` counts as ready alongside `idle`: both mean "at its input", and `done` flips to `idle` once anyone views the pane.
- **T2 live check.** With this build on herdr 0.9.1: a worker launched into an untrusted repo returned `blocked: folder-trust dialog` within 1.2 s (exit 1); after the dialog was answered by hand, `ready` returned `ready` (exit 0). A worker in a repo nested under the trusted forgectl checkout also got the trust dialog, so trust did not pass down to a different git root; T7 should expect the dialog for any repo not trusted on its own.
- **T3: the first brief is an argv element, not a socket message (T3).** `surface launch --brief` appends `--` and the brief to the harness argv; the trampoline socket already carries the whole argv, so no protocol change was needed. Both harnesses keep what follows `--` as the prompt (Claude Code 2.1.289 answered `-- mcp` as a prompt; Codex 0.160.0 took `-- --help` as one). A launch brief may hold newlines, capped at 64 KiB.
- **T3: typed briefs are one line of at most 600 characters (T3).** Measured on herdr 0.9.1 and Claude Code 2.1.289: `pane send-text` types a `--` literally, so no separator is used and a typed brief may not start with `-`; Claude Code turned 900 typed characters into `[Pasted text #1]` while 700 stayed literal; a newline would be Enter. Longer follow-ups go in a file in the worktree. Codex rotates composer placeholders, which read as typed input, so a typed brief to codex can be refused as "the input box already holds text" (the safe direction).
- **T3: the report marker cannot come from the echo (T3).** The brief spells the REPORT line out in words, so the echoed brief never matches `REPORT <marker>:`, and `read --report` also skips everything above the last line naming the marker. The marker is written to the ledger row before Enter (or with the pending row, for a launch brief).
- **T3: wait reads verdict stability, not screen stability (T3).** A harness's status line and any mod drawn above the prompt change every second, so the plan's "screen stable for N seconds" became "ready verdict held for `--settle`", plus a turn seen, the report on screen, or `--quiet` at the prompt.
- **T3 live check.** With this build on herdr 0.9.1 and Claude Code 2.1.289, a claude worker launched with `--brief` answered with no keystroke typed; `wait` settled in 16 s; `read --report` found the report; a typed `brief` came back `sent` with count 2; the second `wait` saw the turn and `read --report` found the new marker. Two stale ledger rows (`t3-probe` failed, `t3-probe2` launched) remain in sjomba's forgectl ledger for T4's `list --orphans` to show; their workspaces and worktrees were removed by hand.
- **T4: `pr` is not in `surface list` (T4).** `list` is what the foreman pane polls, and a PR lookup there would call GitHub every few seconds per repo. The PR number and state move to T10's `surface status`, which caches `gh` per head SHA. The other T4 fields (`session_id`, `transcript`, `pane_id`, `workspace_id`, `branch`, `repo`, `stage`) are in.
- **T4: forgectl picks the claude session id (T4).** A claude worker is launched with a forgectl-generated `--session-id`, and the row records the transcript path computed from the environment the harness got. Codex has no such flag, so a codex row carries neither field.
- **T4: an identity mismatch is `unreadable`, not `gone` (T4).** That matches `Probe`'s rule that a mismatch is not conclusive. The cost is that rows taken before a herdr restart stay `unreadable` and `close` refuses them; forgectl#1077 tracks telling a restart apart.
- **T4: a kept worktree keeps the row at a new stage `closed` (T4).** The row is removed only when the workspace is closed or gone and no worktree remains, so a later `close` can retry the removal.
- **T4: close runs no repository-selected program in the worker's worktree (T4 security review).** The plan named the four removal checks but not how git runs them. The worktree is the worker's to write, so `status` runs through `gitenv.RunUnfiltered` with hooks pinned off, `worktree remove` through `RunUnfilteredAlso`, and close refuses a `.git` that is not a gitfile naming the common git dir's `worktrees/`. The same hook gap in `clean` and `projects`, and two narrower gitfile races, are forgectl#1079.
- **T4: close refuses a launch less than ten minutes old at `pending` or `worktree` (T4 review).** Such a row may be a launch still running, so `list --orphans` leaves it out too.
- **T5, slice 1: settings isolation lands before the rest of T5 (forgectl#1050).** A claude worker now starts with `--setting-sources ""`, `--strict-mcp-config` and an empty `--mcp-config`, and `--no-chrome`, without `--ide`, and with `SendMessage` and `RemoteTrigger` denied. A live worker showed the claude-in-chrome MCP server despite strict MCP (it is enabled from `~/.claude.json`), and could message other sessions, so both items joined the floor. The branch's `CLAUDE.md` still loads: `claudeMdExcludes` is read only from the settings layers the floor turns off, so the CLAUDE.md check moves to forgectl#1061's trusted base. Built-in Claude Code plugins stay enabled; naming them would be a denylist. Codex workers are not covered yet. The polish security review found the worker inherited the launcher's whole environment (a coordinator session's messaging token, herdr and cmux sockets, and its user settings' `env`), so a worker's base environment is now an allowlist. Still open for the `allow_auto` switch: the deny list names tools (a new cross-session tool would be allowed), and ADR-0010's behavioral test that a herdr-driving tool is unreachable is a live measurement, not a test.
- **T4 live check.** On sjomba with herdr 0.9.1 and Claude Code 2.1.289: `list --orphans` showed the two T3 rows as `gone` orphans, and `close` removed both. A fresh claude worker listed `present` with its `session_id`, and its transcript file existed at the recorded path with that `sessionId`. `close` with an untracked file kept the worktree and named the check; after the file was removed, a second `close` removed the worktree and the row, and the branch stayed.
- **T1 verification beyond the plan's list.** A live `surface launch --worktree` against herdr 0.9.1, with a stub harness, showed the worktree, the ledger row (0600, hashed name, full `Ref`), the worker seeing its own `HERDR_PANE_ID` rather than the launcher's, no danger flag in the argv, and a second launch under the same name refused.

## Panel

Panel: plan-reviewer + red-team + security-posture — revised 2026-10-04 against forgectl source; security-posture found the first revision weakened (brief could answer a dialog, unrestricted pi, posture override, worktree hooks), all four fixed here.

### Panel review findings declined

none declined

## Orchestrator

**Driver:** opus
