## Why

The review of the hc worker surface (#1384) and `surface watch` (#1385) left follow-ups in #1386. Three of them are about what reaches hc and a launched surface from the launcher's environment, and one is that `surface watch` reads hc through its own client, without the worker surface's environment scrub.

## What Changes

- The drift test between the hc calls' environment scrub and the surface launch strip checks both directions. A key the hc scrub names must be stripped by a launch or listed, with its reason, as one a launch keeps.
- `HC_SCOPE`, a token that differs per hc terminal, joins `HC_TERMINAL_ID` as hc's pane identity, and a launch strips every `HC_TERMINAL_` variable: the trampoline puts back the new terminal's own address, socket and scope.
- A launch strips the launcher's `CLAUDE_CODE_SESSION_ID`.
- herdr's `HERDR_ACTIVE_*` variables, which herdr sets only for a keybinding's custom command and which name the focused pane, are stripped from launches and dropped from hc calls.
- `FORGECTL_SURFACE_PANE` stays out of both lists: it names a backend, and the trampoline reads it only from the sealed invocation.
- `surface watch` reads hc through the hc adapter the worker verbs use, and its `hc events` subscription runs with the same scrub.
- `add-hc-surface` and `add-surface-watch` are archived, and the surface spec's Purpose names hc and the watch.

## Impact

- `internal/exec/sensitive.go`, `internal/exec/streaming.go`, `internal/cli/surface.go`, `internal/cli/surface_hc.go`, `internal/cli/surface_watch.go`, `internal/workerwatch/hc.go`, `internal/surface/hcadapter/query.go`.
- Issue: #1386.
