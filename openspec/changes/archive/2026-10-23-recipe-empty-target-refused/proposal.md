## Why

`recipe afk --target ""` was treated as no flag at all, so `--target "$PANE"` with `PANE` unset fell back to `HERDR_ACTIVE_PANE_ID`. That is the workspace's focused pane, which may be someone else's session, and the recipe then ran an irreversible `/compact` there.

## What Changes

- An explicitly empty `--target` exits 2 before any herdr call, the same way an explicitly empty `--prompt` already does.

## Impact

- `internal/cli/recipe.go` (`newRecipeAfkCmd`) and its tests.
- `docs/commands/recipe.md`.
