---
name: launch-auto-exit
date: 2026-07-28
status: probe-authorized
ruled: 2026-07-29
issue: cameronsjo/forgectl#153
---

# `forgectl launch --auto-exit` — probe first, build gated on the result (#153)

**Status: probe-authorized. No supervision or shim code is authorized by this document.** The
only currently-authorized step is the probe below. Everything past it is gated on the probe's
result, not a live design decision to pick from today.

## Architectural position (shared with #167 / #168)

forgectl remains an exec-and-replace tool. `forgectl launch` execs into claude and does not
return, and this plan does not change that. Neither this feature nor its companion changes it
either — no persistent foreground supervisor, no shell-integration shim, in
`plan/26-y-shell-history` (PR cameronsjo/forgectl#167) or `plan/153-launch-auto-exit` (PR
cameronsjo/forgectl#168). Any future change to that posture is a separate, deliberate decision,
made only after a probe demonstrates today's architecture cannot serve the need. This document
*is* that probe for the launch half.

## Unresolved scoping question (blocking, stated up front)

**No in-repo caller launches unattended today.** The workflow step-plane registers a `launch`
verb but it is a stub returning `step.ErrNotYetWired` (`internal/launch/steps.go:15-32`). The
one place forgectl spawns claude non-interactively is `pr.launchInline`
(`internal/pr/launch.go:88-127`), which deliberately avoids `launch.Exec` and instead opens a
`tmux new-window` running `claude -p <prompt>` — the `-p` fallback #153 wants to escape. So
**who this feature is for is not decided**: an external orchestrator, the future workflow
step-plane, or `pr`'s review sessions replacing `claude -p`. This is not a detail to fill in
during implementation — it changes whether any supervision code belongs in forgectl at all, and
it is unresolved independent of the probe below.

## The ask (issue #153, verbatim intent)

An opt-in `forgectl launch` flag that lets an *interactive* Claude Code session end itself
when its work is done, so unattended launches stay on subscription billing instead of
falling back to `claude -p`. The issue proposes loom's shape: inject a "touch this marker
file as your final act" instruction via `--append-system-prompt`, poll for the marker,
SIGTERM the session when it appears, and let "the existing timeout" reap a session that
never touches the marker. The issue flags its own confidence as *likely approach*, and asks
for a per-session (tmpdir-keyed) marker path so concurrent launches never collide.

## Verified current state (corrected premises)

Read against `origin/main` at `bb2e681` in a clean worktree.

**`forgectl launch` execs — it does not supervise.** `launchExec` ends in
`launch.Exec(claudePath, claudeArgs, env)` (`internal/cli/launch.go:165`), and `Exec` is
`syscall.Exec` (`internal/launch/launch.go:140-146`). The comment there is explicit that
this is deliberate: "On success it never returns, so Ctrl-C, the TTY, and the exit code pass
through untouched. This is the one documented exception to routing process execution through
internal/exec.Runner — Runner spawns a child, whereas the launcher must *become* claude."
The README repeats the guarantee (`README.md:255`).

**Consequence: no forgectl process survives to poll a marker or send a signal.** `syscall.Exec`
replaces the forgectl process image with claude's — there is no host left for the issue's
proposed "poll + SIGTERM" step. That mechanism cannot be bolted onto the current `launch` path;
it would require abandoning `syscall.Exec` for the auto-exit path, which is exactly the change
the architectural position above declines pending the probe.

**There is no "existing timeout" on the launch path — the issue's backstop premise is false as
written.** A repo-wide grep for `Timeout` outside tests returns only `internal/bench/probe.go`
(a 2s HTTP reachability probe), `internal/net/net.go` (dial timeout), `internal/config/config.go`
(the net section's `timeout_ms`), `internal/cli/docs_serve.go` (HTTP shutdown grace), and
`internal/sessions/concordance.go` (a 5s connect timeout). None of them is on `forgectl launch`.
The nearest reaper is `pr.Client.Cleanup(ctx, date)` (`internal/pr/teardown.go:105`) — a
**manual, date-keyed** sweep of review breadcrumbs, not a timer, and it belongs to the `pr`
module, not `launch`. If a build is ever authorized, the reap timeout has to be built from
scratch; nothing today backstops it.

**No marker, system-prompt, or signal machinery exists in launch.** Greps for `marker`,
`append-system-prompt` / `AppendSystemPrompt`, and `SIGTERM` / `syscall.Kill` find nothing in
`internal/launch` or `internal/cli/launch*.go`. Argv assembly is three pure functions —
`SessionArgs`, `BuilderArgs`, `AgentsArgs` (`internal/launch/launch.go:28-71`) — and there is
today no seam for injecting a flag pair into `SessionArgs`.

**Session state on disk has a precedent to copy, if a build is ever authorized.** `pr` writes
JSON breadcrumbs to a `sessionsDir` with `0o700` (`internal/pr/breadcrumb.go:39-47`) and
validates on load that the path resolves *inside* that dir before trusting it (`loadBreadcrumb`,
`breadcrumb.go:69-79`). Any marker/state directory this feature might introduce should reuse
that containment discipline rather than invent a second one.

**Config seam, if a build is ever authorized.** `[launch.defaults]` is `config.LaunchDefaults`
and `[[launch.project]]` is `config.LaunchProject` (`internal/config/config.go:88-113`), reduced
to a `launch.Profile` (`internal/launch/profile.go:14-21`). A per-project auto-exit default has
an obvious home here; a CLI flag does not, because bare/builder launches are intercepted
**pre-Cobra** (`launchIntercept`, `internal/cli/execute.go:64`, `219-229`) and passed to claude
verbatim — `--auto-exit` would be handed to claude as an unknown flag unless the intercept learns
to strip it first.

**Session *identity* is already solvable natively — only completion detection is an open
problem.** `claude agents --json` prints active sessions (interactive and background) with `pid`,
`sessionId`, and `status`, probed live on this machine (claude 2.1.220): `--all` additionally
includes completed background sessions. If forgectl mints the UUID and passes `--session-id`, a
watcher can find the exact `pid` without any marker file. What `agents --json` cannot tell you is
completion — `status: "idle"` is what every session sitting at its prompt reports, done or not.

## The probe (the only currently-authorized step)

**Question: does `claude --bg` already provide unattended launch while preserving subscription
billing and interactive posture?**

`--bg` / `--background` exists on this machine's claude (2.1.220): "Start the session as a
background agent and return immediately (manage with `claude agents`)." `claude agents --json
--all` reports completion for background sessions. If `--bg` preserves subscription billing and
the interactive posture (`--ide`, plan mode, the TUI) that #153 is trying to keep, most of this
feature collapses: forgectl injects `--bg --session-id <uuid>`, prints the id, and exits, and
`claude agents` owns reaping. No supervisor, no marker, no shim.

This probe gates everything below. Run it before any code is written. If `--bg` clears the bar,
the sections below are moot and #153 closes as "solved upstream, thin passthrough only." If it
does not — because billing degrades, or interactive posture is unavailable in `--bg` mode, or
both — the questions below become live, in the priority order given.

## Design space (reference only — none of this is authorized until the probe returns)

The three designs below were sketched before the probe was identified as the correct first step.
They remain useful as a map of what "if the probe fails" looks like, but none is a live choice
today.

### Option A — supervisor fork (loom's shape, adapted to forgectl)

`--auto-exit` switches the launch path off `syscall.Exec`: forgectl mints a UUID, injects
`--session-id <uuid> --append-system-prompt "<touch instruction>"`, spawns claude as a child
with the TTY inherited, then polls the per-session marker path and SIGTERMs the child when it
appears (or when the deadline passes). This is the option the architectural position forecloses
by default — it introduces the repo's first long-lived foreground supervisor and gives up the
`syscall.Exec` guarantee (Ctrl-C, TTY ownership, SIGWINCH, SIGTSTP, exit-code propagation all
become forgectl's problem). It would only be reconsidered if the probe fails *and* Option B is
also ruled out.

### Option B — hook-driven marker plus native identity

Same injected instruction, but the marker is written by a **`Stop` hook** wired in via a
generated `--settings` JSON, not by the model reaching for a Bash tool as its final act.
Supervision stays decoupled: forgectl mints `--session-id`, so any watcher — a
`forgectl launch wait` verb, or an external orchestrator — resolves the session's `pid` from
`claude agents --json` and signals it. Bare `forgectl launch --auto-exit` keeps `syscall.Exec`
when no watcher is requested. This does not introduce a foreground supervisor inside `launch`
itself, so it is the design most compatible with the architectural position if the probe fails —
but it is still gated behind the probe, since if `--bg` works, none of this is needed.

### Option C — the probe's own success path

Treat #153 as already solved upstream: `--auto-exit` becomes a thin posture that injects
`--bg --session-id <uuid>`, prints the id, and exits. This is Option C from the prior draft,
folded into "what the probe returning yes looks like" rather than a separate design to weigh
against A and B.

## Gated questions (not live until the probe returns)

These were previously presented as seven parallel open decisions. They are not independent —
each is downstream of the probe and of the unresolved scoping question above — so they are
listed here in gated order rather than as things to rule on today.

1. **Blocking, independent of the probe: who is this for?** See "Unresolved scoping question"
   above. Answer before anything else, regardless of the probe's result.
2. **The probe itself.** Run it. Its result determines whether anything below applies.
3. **If the probe fails — may the auto-exit path abandon `syscall.Exec`?** This is the
   architectural line the ruling holds by default. Option A requires yes; Option B does not.
   Reopening it requires the probe to have failed *and* a case that Option B is insufficient.
4. **If a build proceeds — marker scheme: model-writes or hook-writes?** Model-writes (the
   issue's proposal) is simpler but depends on obedience. Hook-writes is deterministic but needs
   a sentinel to distinguish turn-end from task-end (the `Stop` hook fires every turn, not once
   at task end) and couples forgectl to `--settings` merge semantics.
5. **If a build proceeds — reap timeout: default value and escalation.** No existing timeout
   exists to lean on; one has to be built in the same PR or a marker-less session hangs
   unbounded. Decide the default duration, whether it is configurable per `[[launch.project]]`,
   and whether escalation is SIGTERM-only or SIGTERM-then-SIGKILL after a grace window.
6. **If a build proceeds — what does the caller see?** Is "reaped by deadline" a nonzero exit
   code, or the same exit as a clean finish with the distinction only in the marker's presence?
7. **If a build proceeds — auto-exit vs Resume/Fork.** A caller-minted `--session-id` cannot
   coexist with `--resume` (`SessionArgs`, `internal/launch/launch.go:34-39`). Force `New`
   silently, or refuse the combination with an error?

### Cross-cutting failure modes (apply to any option, if a build ever proceeds)

- **A session that outlives its marker.** The marker says "I'm done"; the process may still be
  finishing a tool call. Any design needs a grace window between marker-observed and SIGKILL.
- **A session that never touches its marker.** With no launch timeout in the repo today, this
  is an *unbounded* hang unless a deadline ships in the same PR.
- **Crash before the marker.** Indistinguishable from "still working" to a pure marker poller.
  Watching the pid (child exit, or `agents --json` disappearance) is what separates them.
- **Stale markers.** A marker path reused across runs makes a *previous* run's success look
  like this run's. Per-session UUID keying plus a fresh-create-and-sweep pattern (`pr`'s
  date-keyed `Cleanup` is the precedent) fixes this.
- **Marker path as a trust boundary.** Whatever writes the marker must be validated as *inside*
  forgectl's state dir before it is acted on — reuse `sandbox.WithinWorkspace` the way
  `loadBreadcrumb` does (`internal/pr/breadcrumb.go:78`).
- **The pre-Cobra intercept.** `--auto-exit` must be stripped in `launchIntercept` before argv
  reaches claude, and must not disturb the byte-clean `agents --json` passthrough
  (`IsAgentsPassthrough`, `internal/launch/launch.go:75-83`).

## Ruling record

The owner ruled on the shared architectural fork between this plan and #26 on 2026-07-29:
**forgectl stays exec-replace; it does not grow a foreground supervisor or a shell shim until a
probe proves one necessary.** For this plan, that ruling both settles Decision 3 from the prior
draft (no, `syscall.Exec` may not be abandoned by default) and identifies the probe above as the
correct first move — it was already recommended in the prior draft, but this revision makes it
the *only* authorized step rather than one of three co-equal open questions.

## Provenance

Verified against `cameronsjo/forgectl@bb2e681` (`origin/main`) in worktree
`plan/153-launch-auto-exit`. Platform flags and the `claude agents --json` shape were probed
live against claude 2.1.220 on this machine on 2026-07-27; those are version-bounded
observations, not contracts — re-probe before building on them, and re-probe `--bg` specifically
as part of the probe step above.
