## Why

When the `[[resume.on_update]]` restart hook fires, every restarted session comes back in the launch posture's `--permission-mode` (plan by default), whatever mode it was in before, and no notice is given before a pane restarts. A session in `bypassPermissions` or `acceptEdits` silently drops to plan, and the restart looks like a session that died and came back on its own (cameronsjo/forgectl#1332, Linear CAM-192).

## What Changes

- New `forgectl resume --keep-permission-mode` flag: resume in the permission mode the session's transcript last recorded instead of the posture's. The value must be a mode Claude Code accepts; `bypassPermissions` carries only when the posture can reach it (`allow_danger` on and stdout a terminal, or the posture's own mode). Otherwise, or when no mode is recorded, the posture's mode applies and a stderr note says why.
- `resume restart --outdated` and the update watcher relaunch with `forgectl resume --keep-permission-mode <id>`, and the by-hand command they print carries the flag too.
- A plain `forgectl resume` is unchanged: it still starts in the posture's mode.
- The update watcher posts a desktop notification before each restart, naming the session and pane, and one summary notification when the run ends. `resume restart` and the watcher log print a `summary` line.

## Capabilities

### Modified Capabilities

- `resume`: restarts keep each session's permission mode and announce themselves.

## Impact

- CLI flag: `forgectl resume --keep-permission-mode`.
- Relaunch line typed into herdr panes, and the by-hand command in progress lines.
- Docs: `docs/commands/resume.md`.
