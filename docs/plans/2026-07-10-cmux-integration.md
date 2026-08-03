# Optional cmux Integration — Launch Surface, Session UX, Fleet Seam

**Status:** planned · **Feeds:** roadmap issue #8 (`forgectl cmux` — workspace domain + session orchestration) · **Depends on:** #2 (launch, shipped)

Panel: 2× plan-reviewer (internal-conflict/drift lens, underspecification lens) + owner-lens review — 25 findings, 20 folded, 3 declined (recorded below), 2 owner rulings applied.

## Context

cmux is a macOS terminal app with a Unix-socket CLI (`cmux <verb>`), a native Claude Code wrapper (hooks, session restore, Feed approvals), a reconnectable events stream, and workspace/tab/pane orchestration verbs. forgectl already anticipates it in three places:

- Roadmap issue #8 reserves "forgectl cmux — workspace domain + session orchestration" (phase Next, unblocked since launch shipped).
- ADR-0002 names "hand to tmux/cmux" as a workflow execution surface and rejects a synchronous-only model on exactly those grounds.
- `internal/launch/claudepath.go:19` anticipates a cmux-wrapped or non-PATH `claude` (the #289 fold), and the resolution chain already handles it.

A six-agent build fan-out (2026-07-10) exercised `forgectl launch` headlessly under tmux and confirmed the launcher composes cleanly with cmux's Claude wrapper (`forgectl launch` → cmux shim → `claude`). Every hand-rolled piece of that fan-out — window-per-agent, boot verification, stall detection, completion signaling — maps 1:1 onto cmux CLI verbs. That fan-out is the manual prototype this plan mechanizes.

Two facts were verified empirically on a live cmux install (2026-07-10) and are load-bearing below:

1. `cmux workspace create --command` runs the command under the **user's login zsh** (probe: `$0` = `-/bin/zsh`, parent `/usr/bin/login`) — not `/bin/sh`. Quoting targets zsh-with-login-dotfiles semantics (fatal unmatched globs, `=word` expansion), so the re-entrant command is fully single-quoted, no unquoted metacharacters ever.
2. Workspace-command processes receive `CMUX_SURFACE_ID`, `CMUX_WORKSPACE_ID`, `CMUX_TAB_ID` (and more) in env — the in-cmux detection contract holds inside created workspaces.

**The optional contract is the spine of this plan:** with no cmux config key and no cmux flag present, no cmux codepath executes — not even a `ping` — and forgectl behaves exactly as today. When a profile *does* opt in but cmux is absent, behavior degrades to today's exec plus one stderr warning. Optionality is enforced by runtime detection plus config gating, never by config discipline alone.

## Goals

1. forgectl detects cmux and reports it (`launch doctor`, `launch which`) with zero behavior change.
2. A profile can opt a directory into launching **interactive** claude sessions as cmux workspaces instead of exec-in-place.
3. Sessions launched inside cmux get free UX: workspace named for the session, posture visible via status, and a stored resume binding that re-enters through the profile.
4. The seam is shaped so issue #8's fleet/workspace domain and #32's pr-poll daemon can build on it without rework.
5. Document the **inbound** seam: cmux's `automation.claudeBinaryPath` pointed at forgectl, so every claude session cmux itself starts flows through per-directory profiles.

## Non-goals (explicit cuts)

- **No socket-protocol client.** Exec the `cmux` CLI, exactly as `internal/tmux` execs `tmux`. The V2 socket verbs stay cmux's problem.
- **No Feed/permission integration.** cmux's Claude wrapper does this natively; forgectl touching it would double-wire.
- **No `surface = "tmux"` value.** `forgectl tmux` already owns that posture; one door per room.
- **No fleet verbs in this plan.** T3 below is seam-shaping only; the fleet domain ships under issue #8 proper.
- **No `[cmux].binary_path` config key** (owner ruling): cmux's install location is deterministic (PATH or the fixed app bundle path); `$FORGECTL_CMUX_BIN` covers the exotic case. Add the key later only if a real non-deterministic install shows up.
- **No GUI app auto-launch** (owner ruling): when `surface=cmux` resolves but cmux is unreachable, forgectl warns and degrades to local exec. It never launches the app (`open -a cmux`) as a side effect — surprise-starting a GUI app is worse than losing the workspace.

## Design

### Detection: two independent predicates

| Predicate | Signal | Meaning |
|---|---|---|
| `InsideCmux()` | `CMUX_SURFACE_ID` **or** `CMUX_WORKSPACE_ID` set and non-empty | this process runs in a cmux terminal |
| `Reachable()` | `cmux ping` exits 0 (250ms timeout) | the app is up and drivable |

`CMUX_SOCKET_PATH` is deliberately **not** a detection signal — it is a user-settable socket *override*, exportable in any plain terminal, and keying on it produces false positives that would skip workspace creation and fire UX calls with no workspace context.

`InsideCmux()` reads env only. `Reachable()` needs a binary path, which is config-free by the ruling above: `$FORGECTL_CMUX_BIN` → `PATH` lookup → `/Applications/cmux.app/Contents/Resources/bin/cmux`. Resolution lives in the client package; nothing in detection reads config.toml.

### New package: `internal/cmux`

House ops-layer pattern, **including the test seam**: like `internal/tmux.Client` (which wraps tmux + sesh behind the `exec.Runner` seam, tmux.go:13), the cmux Client takes an `exec.Runner` at construction — `New(run exec.Runner) *Client` — and every verb goes through it. Unit tests use Runner fakes, the house pattern; stub-script fakes are reserved for the quoting helper's integration check only. This also satisfies issue #8's own acceptance criterion verbatim ("All cmux calls go through `internal/exec` `Runner` so they're faked in tests").

Method set sized to this plan's callers — nothing speculative:

```
Ping() error
WorkspaceCreate(o WorkspaceOpts) (ref string, err error)
RenameWorkspace(workspaceID, title string) error   // explicit --workspace target
SetStatus(key, value string) error                  // targets current surface via CMUX_SURFACE_ID env
ResumeSet(shell string) error                       // cmux surface resume set --shell

type WorkspaceOpts struct {
    Name    string
    Cwd     string
    Command string
    Focus   bool   // true only for a user-initiated single launch; fleet callers pass false
}
```

`Notify`/`ClearStatus` are cut (no caller in T0–T2; they join the client when #8/#32 call them). Per-call timeouts: 250ms default, `WorkspaceCreate` 3s. Verbs that target "the current surface" rely on the cmux CLI inferring it from `CMUX_SURFACE_ID` (verified present in-workspace); `RenameWorkspace` passes `--workspace "$CMUX_WORKSPACE_ID"` explicitly.

### Config

```toml
[launch.defaults]
surface = "inherit"        # "inherit" (default) | "cmux"

[[launch.project]]
match = "/path/to/agents/dir"
name = "agents"            # optional: workspace/tab title; default = basename(match), or basename(cwd) when defaults-only
surface = "cmux"

[cmux]                     # optional section
session_ux = true          # default true; false disables all T2 UX calls
```

Two new profile keys, both riding the existing longest-prefix merge (internal/launch/profile.go:54-88), added to `LaunchDefaults` and `LaunchProject` (internal/config/config.go:74-92):

- `surface` — missing key is `"inherit"`; every existing config keeps working unchanged.
- `name` — the session's display name, used for `WorkspaceCreate.Name` and `RenameWorkspace`. Default: `basename(match)` for a project profile, `basename(cwd)` under defaults-only resolution. This is the single source of truth the T1/T2 naming story hangs on.

CLI: a `--surface <inherit|cmux>` flag (overrides the profile key, the established flag-beats-profile launch pattern). It is hand-parsed and **stripped in the launch arg intercept** (the same pre-Cobra capture that keeps passthrough byte-clean, internal/cli/launch.go:46) *before* mode dispatch, in every mode — it never reaches claude argv on any row of the decision table. Flag form is `--surface=X` or `--surface X` recognized only before the first positional argument, so a prompt string containing the literal text is never eaten. `--surface inherit` is the per-invocation escape hatch from a cmux-profiled directory.

### T0 — detect & report

- `forgectl launch doctor`: two independent lines — `cmux reachable: yes/no (<resolved path>)` and `inside cmux: yes/no` — because the predicates form a 2×2 (inside-but-app-gone over SSH; outside-but-reachable), not a tri-state.
- `forgectl launch which`: one new label/value row in the existing styled block (launch_which.go:40-72): label `surface`, value = resolved value plus origin, e.g. `cmux (project)`, `inherit (default)`, `cmux (flag)`.

### T1 — launch surface (interactive sessions only)

**Mode precondition (the carve-out):** the surface decision applies **only to the interactive session path** (the interview/`SessionArgs` path — a TTY session with no passthrough prompt args). Agents mode (`launch agents …`, including its byte-clean `--json` pipe), print/one-shot usage (`-p`/`--print`), and any passthrough invocation carrying user args resolve `surface` as `inherit` unconditionally. Scripted callers and piped consumers can never be hijacked into a workspace ref on stdout.

Decision table, evaluated in `launchExec` (internal/cli/launch.go:109-151) after the mode precondition, before arg building:

| surface | InsideCmux | Reachable | Behavior |
|---|---|---|---|
| inherit | any | — (never probed) | today's exec, unchanged |
| cmux | true | — (never probed) | exec in place (already surfaced; do not re-wrap) |
| cmux | false | true | `WorkspaceCreate`, verify, print ref, exit 0 |
| cmux | false | false | one stderr warning, fall through to today's exec |

`Reachable()` (and any cmux exec at all) is only evaluated on the `surface=cmux, InsideCmux=false` rows — the optional-contract spine holds: unconfigured directories never pay even a ping.

**Failure semantics:** unreachable (ping fails) degrades to local exec — that's the pre-check row. But once `WorkspaceCreate` is *attempted*, an error or timeout **fails the launch with a clear error**; it does not fall through to local exec, because a create that actually succeeded after a client-side timeout would leave one command owning two live sessions. Degrade-before-attempt, fail-after-attempt.

**Post-create verification:** the outer invocation confirms the workspace exists (`cmux list-workspaces` contains the returned ref, bounded ~2s) before printing the ref and exiting 0. It does not verify the inner session booted (declined — see below).

**Re-entrancy is the mechanism:** the workspace command is forgectl again — built by `reentrantCommand()` with these pinned properties:

- The executable is the **absolute path from `os.Executable()`**, never bare `forgectl` (the workspace's login shell PATH is not assumed).
- Original passthrough args are reproduced minus any `--surface` flag, with `--surface inherit` prepended — the inner invocation's posture is decided by the outer one, structurally, so the no-loop guarantee does **not** depend on cmux's env contract (belt) even though `InsideCmux()` is also true in the workspace, verified empirically (suspenders).
- Every token is fully single-quoted for the verified target shell (login zsh): `'…'` with embedded quotes as `'\''`. No unquoted metacharacters, ever — this sidesteps zsh's fatal unmatched-glob and `=word` behaviors wholesale.
- The quoting helper has table-driven tests (spaces, single/double quotes, `$`, `!`, globs, `--` separators, empty args) asserted by executing the produced string under `zsh -c` via the Runner seam's integration check.

Inside the workspace, the inner invocation is interactive-with-TTY, so the interview (internal/launch/interview.go) runs there, and `claude` resolves through cmux's shim — which the claudepath chain already handles.

### T2 — session UX (inside cmux, pre-exec)

In `launchExec`, immediately before the `Exec()` call (the hook point is the caller, where profile and effective args are in scope — not inside `launch.Exec` itself), when `InsideCmux()` and `[cmux].session_ux` is not false:

1. `RenameWorkspace($CMUX_WORKSPACE_ID, profile name)` — tabs named by intent (the `name` key / its basename default), not cwd basename.
2. `SetStatus("forgectl", "<model>·<permission_mode>")` — where `<model>` is the **effective** model: the value actually placed in claude argv (the interview's `choice.Model` on the interactive path; the profile model on builder paths, or the user's `--model` override when present, last-flag-wins). Never blindly `profile.Model`. The status key is not cleared afterward — `syscall.Exec` replaces the process, so no forgectl code exists post-launch; the status dies with the workspace, and docs/cmux.md says so in one line.
3. `ResumeSet("<abs-path-forgectl> launch")` — **interactive sessions only** (skipped for one-shot/`-p`/passthrough-with-prompt launches, where a resume binding is meaningless). The binding is registered for cmux's *manual restore* flow (that is the CLI's documented contract: bindings are "stored for inspection and manual restore"); on restore, bare `forgectl launch` runs the interview, whose Resume path re-enters with full `SessionArgs` posture. Whether cmux agent-hibernation auto-resume also drives this binding for terminal surfaces is **unestablished** — the Verification section carries a live probe, and no claim beyond manual restore ships until it passes.

All three are fire-and-forget: failures log at debug level and never block the exec. Skipped entirely when `InsideCmux()` is false.

Note on scope: T2 fires for sessions launched inside cmux **today**, without any `surface` config — that is a deliberate, documented behavior addition (cosmetic, reversible, opt-out via `session_ux = false`), accepted because it is the entire "free UX" value of the tier.

### Inbound recipe — cmux `automation.claudeBinaryPath` → forgectl (docs only, zero code)

cmux can invoke a custom binary for every claude session it starts (`automation.claudeBinaryPath`), appending its own `--session-id`/`--settings` args. Pointing it at a two-line shim that execs `forgectl launch <cmux's args>` puts **every cmux-initiated session** through per-directory profile resolution — model, permission mode, env, add-dirs — replacing any hand-rolled static-posture wrapper. Two requirements the doc must state:

1. forgectl's claude resolution must target the **real** claude binary by absolute path (`FORGECTL_CLAUDE_BIN` or `[launch.defaults] binary_path`) so the exec chain cannot loop through cmux's PATH shim.
2. cmux's appended args arrive after forgectl's injected profile flags and win on conflict (claude is last-flag-wins) — which is the correct precedence: cmux owns session identity, the profile owns posture defaults.

This is the *inbound* half of the integration (cmux → forgectl → claude); T1 is the *outbound* half (forgectl → cmux workspace → forgectl → claude). They compose: an inbound-launched session is `InsideCmux()` and gets T2 for free. Ships as `docs/cmux.md` § Inbound recipe + a sample shim in `docs/examples/`.

### T3 — seam shaping only (ships with #8/#32, not this plan)

Recorded here so T0–T2 don't paint the later work into a corner:

- The fleet domain (`forgectl cmux ls|pick|…`) reuses `internal/cmux.Client` — the method set grows, the shape (Runner-seamed, one-shot exec verbs) doesn't change.
- The pr-poll daemon (#32) consumes `cmux events --category agent --reconnect` for Claude lifecycle events; the events consumer is a *new* long-lived subscription API and deliberately NOT part of this client.
- The workflow DSL (#9 spike, ADR-0002) gains `surface = cmux` as a second executor behind the same seam as tmux; the DSL executor calls `WorkspaceCreate` directly rather than routing through launch.
- **Issue #8 hygiene (deliverable of this plan):** amend #8 to strike its scope item 3 ("`forgectl cmux launch <repo|path>`") in favor of `surface=cmux` on the existing launch door — otherwise #8 later builds a second door to the same room — and note that item 4's context-awareness is now `InsideCmux()`.

## Implementation steps

Sliced so the first slice ships nothing unread:

**Slice 1 — T0 + T2:**
1. `internal/cmux`: Runner-seamed client, detection predicates, binary resolution; unit tests via Runner fakes.
2. Config: `name` key + `[cmux] session_ux`; schema/merge tests.
3. T0 doctor (2×2) + which (surface row).
4. T2 pre-exec UX calls with the interactive-only ResumeSet gate and effective-model status value.

**Slice 2 — T1:**
5. Config: `surface` key (defaults + project) — lands with its consumer.
6. `--surface` flag parse/strip in the launch intercept; mode precondition.
7. Decision table in `launchExec`; `reentrantCommand()` + quoting tests (zsh target); post-create existence verification; degrade-before-attempt/fail-after-attempt semantics. Testability: the `syscall.Exec` terminus is behind the existing exec seam exception — decision-table unit tests inject a fake exec function and a Runner fake, asserting which terminus each row reaches.

**Slice 3 — docs + hygiene:**
8. `docs/cmux.md`: detection, config keys, degradation matrix, status-lifecycle note, inbound recipe + example shim.
9. Amend issue #8 (strike item 3, update item 4).

## Verification

- Unit: detection env matrix — "present" means set AND non-empty; cases: `SURFACE_ID` only, `WORKSPACE_ID` only, both, empty-string values, neither, `SOCKET_PATH`-only (must be false). Binary resolution precedence. Quoting table (executed under `zsh -c`). Decision table: 4 rows plus the mode-precondition carve-outs (agents `--json`, `-p`, passthrough-with-args), via injected exec seam + Runner fake — including "never probes cmux" assertions on the inherit and inside-cmux rows.
- Integration (manual, cmux installed): doctor 2×2 correct in and out of a cmux terminal; `surface=cmux` from a plain terminal opens a named workspace, session lands in the right cwd with the right model, interview runs inside; the same command inside cmux execs in place; `launch agents --json` under a cmux-profiled directory stays byte-clean; killing the cmux app degrades to a warned local exec; `--surface inherit` escapes a cmux-profiled directory.
- **Hibernation probe (gates any auto-resume claim):** hibernate a forgectl-launched workspace (over `maxLiveTerminals`, idle, background), then revisit — record whether the stored `forgectl launch` binding is offered/run. Outcome updates docs/cmux.md; no plan change either way.
- Regression: full existing launch suite green with no cmux config present — proving the optional contract.

## Risks

- **Re-entrant quoting** remains the concentrated risk; mitigated by the pinned target shell (verified login zsh), full single-quoting, the dedicated helper, and `zsh -c` round-trip tests.
- **cmux CLI drift**: verbs are exec'd by name; a rename degrades to the no-op path (errors logged, launch proceeds). No version pin — proportionate for a best-effort local integration.
- **Double-surfacing**: prevented structurally (`--surface inherit` injected into the re-entrant command) and environmentally (`InsideCmux()` short-circuit row); either alone suffices.

## Panel review — findings declined

- **Full boot verification of the inner workspace session (underspec lens):** declined beyond the workspace-existence check. Sentinel-polling `read-screen` for session boot is fleet machinery; for a single user-initiated launch the failure is visible in the tab cmux just opened (focused by default), and the absolute-path + `--surface inherit` re-entrant command removes the two likeliest silent-death causes. #8's fleet work owns real boot verification.
- **T2 firing for existing inside-cmux sessions without opt-in (underspec lens, advisory):** declined as a defect; accepted and documented as the tier's purpose. Cosmetic, reversible, `session_ux = false` opt-out ships in the same slice.
- **`[cmux].binary_path` for claudepath symmetry (conflict lens implied; owner lens ruled):** declined — trimmed per owner ruling. `$FORGECTL_CMUX_BIN` + PATH + the fixed app-bundle path cover a deterministic install; symmetry alone doesn't buy a config key.
