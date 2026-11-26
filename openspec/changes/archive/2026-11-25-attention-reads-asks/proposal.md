## Why

The attention queue read a per-session spool that `attention put` fed from each Claude Code reply's `What's next:` block. Nothing in this repository calls `put` or `rm`; the writer was an external session hook that is no longer used, so the spool sits empty and its parser, pruning and threat model cost upkeep for no rows. Meanwhile the requests that really wait on the operator now live on the desk as asks (`forgectl desk ask`), which the queue did not show.

## What Changes

- **BREAKING** `attention put` and `attention rm` are removed, with the spool read, its pruning, the `What's next:` parser and `[attention] ttl_hours`. `sources.spool` and the `q`/`a` row kinds, with `actor`, `rec`, `turn` and `notOnDesk`, are gone from `list --json`.
- `attention list` shows each open desk ask as a row: kind `ask`, delay cost 4, tier `count`, ties oldest first. Answered asks are absent; snoozed asks are hidden until the snooze ends.
- The asks log is read through the asks store's read-only open: no lock, no write, no chmod. `ask.OpenReadOnly` no longer narrows a broad asks directory (writers still do).
- Ask text is untrusted: titles are cleaned and cut, malformed log lines are counted in `skipped`, a malformed id gives no row.
- With `FORGECTL_DESK_ASKS_HOST` set and no local desk named, the asks are not read: `sources.asks` is `remote`.

## Capabilities

### Modified Capabilities

- `attention`: the spool is retired and desk asks become rows.

## Impact

- `internal/attention` (spool, record and parser removed; ask rows added), `internal/cli/attention*.go`, `internal/config/attention.go`, `internal/desk/ask/store.go`, `docs/commands/attention.md`, `docs/configuration.md`, `README.md`.
- Consumers of `attention list --json` that read `sources.spool` see it absent; the notch helper already treats a missing source as `absent`.
