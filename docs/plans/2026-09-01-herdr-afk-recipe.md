# Herdr AFK Recipe

## Goal

Add a short forgectl command for a named Herdr recipe that can prepare the current coding-agent pane before stepping away. The first recipe is `afk`: submit `/journal`, wait for the agent to settle, then submit `/compact` through Herdr's interactive type-submit primitive.

## Chosen approach

forgectl will add a `recipe` command group with alias `r`. The canonical command is `forgectl recipe afk`; the fast path is `forgectl r afk`.

The command will target the current Herdr pane by default, using `HERDR_PANE_ID` or `HERDR_ACTIVE_PANE_ID`, and will accept `--target` for an explicit agent name or pane id. It will shell out to the Herdr CLI rather than binding directly to Herdr internals.

## Requirements

- `forgectl recipe afk` and `forgectl r afk` MUST run the same recipe.
- The recipe MUST default to the current Herdr pane when Herdr provides pane identity in the environment.
- The recipe MUST accept `--target <agent-or-pane>` to override the default target.
- The recipe MUST run `/journal` through `herdr agent prompt <target> "/journal" --wait`.
- The recipe MUST run `/compact` through Herdr's new interactive type-submit primitive, not through `agent prompt`.
- The command MUST fail clearly before sending anything when no target can be resolved.
- The command SHOULD use forgectl's existing command-runner seams so tests can assert argv without requiring a live Herdr server.

## Alternatives declined

- Add top-level `forgectl afk`: declined because `afk` is one recipe, not a top-level command group.
- Use `forgectl workflow run afk`: declined because workflows carry blessing and arbitrary-command semantics that are heavier than this small built-in routine.
- Put recipes in Herdr core: declined because Herdr should remain the generic terminal/runtime substrate, while forgectl owns Cameron-specific workbench routines.

## Implementation checklist

- [x] Add a `recipe` module with alias `r` and `afk` subcommand.
- [x] Add target resolution from `--target`, `HERDR_PANE_ID`, and `HERDR_ACTIVE_PANE_ID`.
- [x] Invoke Herdr CLI commands through testable runner seams.
- [x] Add unit tests for command registration, target resolution, and argv sequencing.
- [x] Update forgectl README/docs with `forgectl recipe afk` and `forgectl r afk`.
- [x] Run focused Go tests, then the agreed broader validation.
