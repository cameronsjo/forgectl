---
name: launch-auto-exit
date: 2026-07-28
status: awaiting-ruling
issue: cameronsjo/forgectl#153
---

# `forgectl launch --auto-exit` — design space and recommendation (#153)

**Status: awaiting ruling. No implementation is authorized by this document.** It verifies
what forgectl does today, lays out three concrete designs, and names the decisions Cameron
must make before a build is dispatched.

## The ask (issue #153, verbatim intent)

An opt-in `forgectl launch` flag that lets an *interactive* Claude Code session end itself
when its work is done, so unattended launches stay on subscription billing instead of
falling back to `claude -p`. The issue proposes loom's shape: inject a "touch this marker
file as your final act" instruction via `--append-system-prompt`, poll for the marker,
SIGTERM the session when it appears, and let "the existing timeout" reap a session that
never touches the marker. The issue flags its own confidence as *likely approach*, and asks
for a per-session (tmpdir-keyed) marker path so concurrent launches never collide.

## Verified current state

Read against `origin/main` at `bb2e681` in a clean worktree.

**`forgectl launch` execs — it does not supervise.** `launchExec` ends in
`launch.Exec(claudePath, claudeArgs, env)` (`internal/cli/launch.go:165`), and `Exec` is
`syscall.Exec` (`internal/launch/launch.go:140-146`). The comment there is explicit that
this is deliberate: "On success it never returns, so Ctrl-C, the TTY, and the exit code pass
through untouched. This is the one documented exception to routing process execution through
internal/exec.Runner — Runner spawns a child, whereas the launcher must *become* claude."
The README repeats the guarantee (`README.md:255`).

**Consequence: there is no forgectl process left alive to poll a marker or send a signal.**
The issue's "poll + SIGTERM" step has no host. Any design that keeps a poller must either
abandon `syscall.Exec` for the auto-exit path or move the poller somewhere outside forgectl.
This is the single largest thing the issue does not account for.

**There is no "existing timeout" on the launch path.** A repo-wide grep for `Timeout` outside
tests returns only `internal/bench/probe.go` (a 2s HTTP reachability probe),
`internal/net/net.go` (dial timeout), `internal/config/config.go` (the net section's
`timeout_ms`), `internal/cli/docs_serve.go` (HTTP shutdown grace), and
`internal/sessions/concordance.go` (a 5s connect timeout). None of them is on
`forgectl launch`. The nearest reaper is `pr.Client.Cleanup(ctx, date)`
(`internal/pr/teardown.go:105`) — a **manual, date-keyed** sweep of review breadcrumbs, not a
timer, and it belongs to the `pr` module, not `launch`. **The issue's backstop premise is
false as written**: the reap timeout does not exist and would have to be built.

**No marker, system-prompt, or signal machinery exists in launch.** Greps for `marker`,
`append-system-prompt` / `AppendSystemPrompt`, and `SIGTERM` / `syscall.Kill` find nothing in
`internal/launch` or `internal/cli/launch*.go`. Argv assembly is three pure functions —
`SessionArgs`, `BuilderArgs`, `AgentsArgs` (`internal/launch/launch.go:28-71`) — and there is
today no seam for injecting a flag pair into `SessionArgs`.

**No in-repo caller launches unattended today.** The workflow step-plane registers a `launch`
verb but it is a stub returning `step.ErrNotYetWired` (`internal/launch/steps.go:15-32`). The
one place forgectl spawns claude non-interactively is `pr.launchInline`
(`internal/pr/launch.go:88-127`), which deliberately avoids `launch.Exec` ("which would
replace this process") and instead opens a `tmux new-window` running `claude -p <prompt>` —
i.e. the `-p` fallback #153 wants to escape. **So the feature has no consumer inside forgectl
yet.** Who it is for is a ruling, not a detail (see below).

**Session state on disk has a precedent to copy.** `pr` writes JSON breadcrumbs to a
`sessionsDir` with `0o700` (`internal/pr/breadcrumb.go:39-47`) and validates on load that the
path resolves *inside* that dir before trusting it (`loadBreadcrumb`, `breadcrumb.go:69-79`).
Any marker/state directory this feature introduces should reuse that containment discipline
rather than invent a second one.

**Config seam.** `[launch.defaults]` is `config.LaunchDefaults` and `[[launch.project]]` is
`config.LaunchProject` (`internal/config/config.go:88-113`), reduced to a `launch.Profile`
(`internal/launch/profile.go:14-21`). A per-project auto-exit default has an obvious home
here; a CLI flag does not, because bare/builder launches are intercepted **pre-Cobra**
(`launchIntercept`, `internal/cli/execute.go:64`, `219-229`) and passed to claude verbatim —
`--auto-exit` would be handed to claude as an unknown flag unless the intercept learns to
strip it first. That is a real, small piece of work the issue does not mention.

### Platform facts, probed on this machine (claude 2.1.220)

- `--append-system-prompt <prompt>` exists.
- `--session-id <uuid>` exists ("Use a specific session ID for the conversation").
- `--bg` / `--background` exists: "Start the session as a background agent and return
  immediately (manage with `claude agents`)".
- `claude agents --json` prints **active sessions (interactive and background)** as JSON and
  "does not require a TTY". A live probe returned objects with `pid`, `cwd`, `kind`
  (`"interactive"`), `startedAt`, `sessionId`, `name`, and `status` (observed: `"idle"`).
  `--all` additionally includes completed background sessions.
- `Stop`, `SessionEnd`, `SessionStart`, and `SubagentStop` are real hook events, and
  `--settings <file-or-json>` can inject hook wiring per launch.

The `agents --json` finding matters: **session identity is already solvable natively.** If
forgectl mints the UUID and passes `--session-id`, a supervisor can find the exact `pid` to
signal without any marker file. What `agents --json` cannot tell you is *completion* —
`status: "idle"` is what every session sitting at its prompt reports, done or not. Marker or
no marker, the completion signal has to be a **deliberate act by the session**; the marker
scheme's real job is that, not identity.

## Design space

### Option A — supervisor fork (loom's shape, adapted to forgectl)

`--auto-exit` switches the launch path off `syscall.Exec`: forgectl mints a UUID, injects
`--session-id <uuid> --append-system-prompt "<touch instruction>"`, spawns claude as a child
with the TTY inherited, then polls the per-session marker path and SIGTERMs the child when it
appears (or when the deadline passes).

- **Pro:** closest to the issue and to loom's proven behavior; one process tree; forgectl owns
  the exit code and can report *why* it ended (marker vs deadline), which is the deterministic
  completion signal callers actually want.
- **Con:** it breaks the documented `syscall.Exec` guarantee for this path. Ctrl-C, TTY
  ownership, terminal resize (SIGWINCH), suspend (SIGTSTP), and exit-code propagation all
  become forgectl's problem, and getting an interactive TUI child right is materially harder
  than the poll loop the issue describes. It also introduces the repo's first long-lived
  foreground supervisor.
- **Failure modes:** forgectl killed while claude runs → orphaned session, nothing ever reaps
  it; claude crashes → poller must notice child exit and not hang to the deadline; marker
  written but session busy in a tool call → SIGTERM lands mid-write.

### Option B — hook-driven marker plus native identity (recommended)

Same injected instruction, but the marker is written by a **`Stop` hook** that forgectl wires
in via a generated `--settings` JSON, not by the model reaching for a Bash tool. The
supervision half is decoupled: forgectl still mints `--session-id`, so *any* watcher —
forgectl's own optional `launch wait` verb, or an external orchestrator — can resolve the
session's `pid` from `claude agents --json` and signal it. Bare `forgectl launch --auto-exit`
keeps `syscall.Exec` when no watcher is requested; the marker simply becomes an artifact the
caller can wait on.

- **Pro:** the marker's *write* stops depending on model obedience — a hook fires
  deterministically, so "no marker" now means "the session never stopped", not "the model
  forgot". Identity comes from the platform instead of a filename convention. The
  `syscall.Exec` guarantee survives for the common case. Splits cleanly into two shippable
  PRs (emit the marker + session id; then the optional waiter).
- **Con:** `Stop` fires at the end of **every** assistant turn, not once at task end — so the
  hook alone signals *idle*, not *done*. Distinguishing them needs the injected instruction to
  write a sentinel the hook keys off (model does the small deliberate act; hook does the
  durable write), which is more moving parts than Option A. Also couples forgectl to Claude
  Code's hook schema and to `--settings` merge semantics, both of which move under it.
- **Failure modes:** a user's own settings already wire `Stop` → merge conflict or double-fire;
  `agents --json` shape changes → identity lookup breaks silently; session ends without ever
  going through `Stop` (hard kill) → no marker, deadline is the only backstop.

### Option C — don't build a supervisor; adopt `--bg` and `claude agents`

Treat #153 as already solved upstream. `--bg` starts the session as a background agent and
returns immediately; `claude agents --json --all` reports completion. `--auto-exit` becomes a
thin posture: forgectl injects `--bg --session-id <uuid>`, prints the id, and exits. Reaping
is `claude agents`' problem.

- **Pro:** zero new supervision code, zero signal handling, no marker scheme at all, and it
  rides a first-party mechanism that will keep improving. forgectl already has an `agents`
  passthrough posture (`internal/launch/launch.go:64-83`) to build on.
- **Con:** a background agent is **not** the interactive session the issue is trying to
  preserve. Whether `--bg` retains subscription billing and the full interactive posture
  (`--ide`, plan mode, the TUI) is *unverified* — and that premise is the entire point of
  #153. It also gives up forgectl's own deterministic "marker present = finished" contract in
  favor of whatever `agents --json` reports.
- **Failure modes:** if `--bg` degrades billing or capability, the feature is a regression
  disguised as a simplification; if it doesn't, most of #153 is dead weight.

### Cross-cutting failure modes (any option)

- **A session that outlives its marker.** The marker says "I'm done"; the process may still be
  finishing a tool call. Every design needs a grace window between marker-observed and
  SIGKILL, and needs to decide whether a session that ignores SIGTERM is escalated or left.
- **A session that never touches its marker.** With no launch timeout in the repo today, this
  is an *unbounded* hang unless a deadline ships in the same PR. The deadline is not optional
  polish — it is load-bearing, and it does not currently exist.
- **Crash before the marker.** Indistinguishable from "still working" to a pure marker poller.
  Watching the pid (Option A: child exit; Option B/C: `agents --json` disappearance) is what
  separates them; a marker-only design cannot.
- **Stale markers.** A marker path reused across runs makes a *previous* run's success look
  like this run's. The per-session UUID keying the issue asks for fixes this only if the
  marker is created fresh and the directory is swept; `pr`'s date-keyed `Cleanup` is the
  precedent for the sweep.
- **Marker path as a trust boundary.** Whatever writes the marker is instructed by an injected
  prompt or a hook. The path must be validated as *inside* forgectl's state dir before it is
  acted on — reuse `sandbox.WithinWorkspace` the way `loadBreadcrumb` does
  (`internal/pr/breadcrumb.go:78`), rather than trusting a path that came back from the
  session.
- **`--auto-exit` collides with Resume/Fork.** `SessionArgs` emits `--resume` /
  `--resume --fork-session` (`internal/launch/launch.go:34-39`); a caller-minted
  `--session-id` is incompatible with resuming an existing conversation. Auto-exit almost
  certainly has to force `New`, or refuse.
- **The pre-Cobra intercept.** `--auto-exit` must be stripped in `launchIntercept` before argv
  reaches claude, and must not disturb the byte-clean `agents --json` passthrough
  (`IsAgentsPassthrough`, `internal/launch/launch.go:75-83`).

## Recommendation

**Option B, sequenced as two PRs, and only after the ruling on who this is for.**

The reasoning is that #153 conflates two problems that have different best answers. *Identity
and reaping* are already solved by the platform — `--session-id` plus `claude agents --json`
gives an exact pid, no filename convention required — so forgectl should not build a bespoke
scheme for that half. *Completion* genuinely needs a deliberate act by the session, and there
the marker is the right primitive; making a `Stop` hook do the durable write (rather than
trusting the model to remember a Bash call as its final act) converts the weakest link from
model obedience into deterministic wiring.

Option A is rejected as the first move because it trades forgectl's cleanest architectural
guarantee — `launch` *becomes* claude, so the TTY story is free — for a supervision loop, and
it does so before any in-repo caller needs it. Option C is not rejected, it is **unverified**:
if `--bg` preserves subscription billing and the interactive posture, it is strictly better
than both, and that probe is cheap. It should be run *before* any build is dispatched.

Splitting the work keeps the risky half optional:

- **PR 1 (low risk, `syscall.Exec` preserved).** `--auto-exit` injects `--session-id <uuid>`
  and the `Stop`-hook `--settings` wiring, creates the per-session state dir, strips the flag
  in `launchIntercept`, forces `New` mode, and prints the session id and marker path to
  stderr. Exit behavior is unchanged; the launch simply becomes *observable*.
- **PR 2 (the supervision half, only if PR 1's signal proves insufficient).** A separate
  `forgectl launch wait --session <uuid> [--deadline <dur>]` that polls the marker and
  `agents --json`, then SIGTERMs with a grace window. Keeping it a distinct verb means the
  supervisor is a process the *caller* chose to run, and `forgectl launch` itself never stops
  being an exec.

## Awaiting ruling

Cameron must decide these before a build is dispatched. Items 1-3 are blocking; 4-7 shape the
build.

1. **Who is this for?** No in-repo caller launches unattended today (the workflow `launch`
   step is `ErrNotYetWired`). Is `--auto-exit` for (a) an external orchestrator, (b) the future
   workflow step-plane, or (c) `pr`'s review sessions — replacing the `claude -p` in
   `pr.launchInline`? The answer changes whether the supervisor belongs in forgectl at all.
2. **Probe `--bg` first, or skip it?** Option C collapses most of this feature if `--bg`
   preserves subscription billing and the interactive posture. That is a cheap, unverified
   premise sitting under the whole issue. Ruling: run the probe before building, or rule it
   out on other grounds.
3. **May the auto-exit path abandon `syscall.Exec`?** This is the architectural line. Option A
   requires yes; Option B's PR 1 requires no. Answering it decides whether forgectl grows its
   first foreground supervisor.
4. **Marker scheme: who writes it — model or hook?** Model-writes (issue's proposal) is simpler
   and matches loom, but depends on obedience. Hook-writes is deterministic but needs a
   sentinel to distinguish turn-end from task-end and couples forgectl to `--settings` merge
   behavior. Related: does the marker live under forgectl's state dir (validated with
   `sandbox.WithinWorkspace`, per `pr`'s breadcrumbs) or an OS tmpdir as #153 suggests?
5. **Reap timeout: default value, and what happens when it fires.** There is no existing
   timeout to lean on — one has to be built in the same PR or a marker-less session hangs
   unbounded. Decide the default duration, whether it is configurable per `[[launch.project]]`,
   and the escalation: SIGTERM only, or SIGTERM then SIGKILL after a grace window (and how
   long).
6. **What does the caller see?** Is "reaped by deadline" a nonzero exit code (deterministic
   "suspect" signal, per the issue), or the same exit as a clean finish with the distinction
   only in the marker's presence? This is the actual contract callers integrate against.
7. **Auto-exit vs Resume/Fork.** A caller-minted `--session-id` cannot coexist with `--resume`.
   Force `New` silently, or refuse the combination with an error?

## Provenance

Verified against `cameronsjo/forgectl@bb2e681` (`origin/main`) in worktree
`plan/153-launch-auto-exit`. Platform flags and the `claude agents --json` shape were probed
live against claude 2.1.220 on this machine on 2026-07-27; those are version-bounded
observations, not contracts — re-probe before building on them.
