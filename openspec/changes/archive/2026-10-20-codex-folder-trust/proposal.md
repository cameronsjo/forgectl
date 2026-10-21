## Why

A codex worker launched in a repository codex has never trusted starts, then sits at "Trust this folder?" in a pane nobody may be watching, and `surface launch` exits 0. The claude half of the preflight shipped in #1262; the codex half was left open until a live check showed explicit `--sandbox` and `--ask-for-approval` flags do not suppress the dialog (cameronsjo/forgectl#1240, codex-cli 0.163.0). It also blocks cameronsjo/cadence#1799, which moves chief-of-staff's worker launches onto `surface launch` so it can stop answering trust dialogs by keypress.

## What Changes

- `surface launch` (workers and plain launches, `--dry-run` included) refuses a codex launch when `$CODEX_HOME/config.toml` (else `~/.codex/config.toml`) has no `[projects."<path>"]` trust entry for the launch folder or its repository root, before anything is created.
- The refusal names the fix: `cd <repo root> && codex`, accept, `/quit`, with `CODEX_HOME=` when the account is not the default.
- Codex trust does not inherit from a parent folder, so no parent counts. Any recorded `trust_level`, `untrusted` included, counts as decided: codex does not ask again.
- A missing or unreadable codex config refuses nothing, as for claude.

## Capabilities

### Modified Capabilities

- `surface`: the folder trust preflight covers codex as well as claude.
