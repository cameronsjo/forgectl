## Why

Drain workers act on GitHub with the operator's credentials: the keychain `gh` login and the operator's SSH agent. Every label, comment, push and PR a worker makes reads as the operator's, so intake cannot tell a label the operator applied from one a worker applied, and a worker's accidental irreversible write lands as the operator's. The `FORGECTL_DRAIN_WORKER=1` marker that fences intake, close and dequeue is a courtesy a worker can unset (ADR-0010, 2026-10-09 amendment).

Cameron decided on 2026-10-20 that workers get their own identity, and that it is a GitHub App rather than a machine user (cameronsjo/forgectl#1309). A machine user's fine-grained token cannot reach repositories owned by another personal account, so it would need a classic `repo` token: full access to every repository it joins, valid for up to a year. An App token is scoped per repository and per permission and expires in an hour. cameronsjo/forgectl#1134 already sketched the App; its trigger ("when a worker needs `auto`") has fired.

Implements cameronsjo/forgectl#1309. Takes over items 1-3 and 5 of cameronsjo/forgectl#1134. Part of cameronsjo/cadence-ecosystem#649.

## What Changes

- New config table `[surface.identity]`: `app_id`, `installation_id`, `bot_login`, `bot_user_id`, `keychain_service` (default `forgectl-worker-app`). With no table, workers run as the operator, exactly as today.
- With the table, every worker launch mints an installation token scoped to the worker's one repository, with Contents and Pull requests write and Issues read. The token reaches the worker only through a private `gh` config directory, never argv, the ledger, `--json` output, a log line or the brief.
- The worker's `gh` and `git` use that token. `git` pushes over HTTPS through `gh auth git-credential`, commits carry the bot as author and committer, and `SSH_AUTH_SOCK` leaves the worker environment allowlist.
- The drain refreshes each live worker's token before it expires. `surface close` and the drain's reap revoke it and remove the directory.
- `surface intake gh` refuses a candidate whose author or labeler is `bot_login`, with reason `worker-labeled`. The terminal confirmation stays.
- `forgectl doctor` reports whether the identity is configured, the key is readable, and a token can be minted.
- **BREAKING** for a configured identity only: a worker can no longer push over SSH or use the operator's `gh` login by default.

Surfaces touched: config `[surface.identity]` (five keys); `surface launch` and `surface drain` worker environment (`GH_CONFIG_DIR`, `GIT_CONFIG_COUNT` entries, `GIT_AUTHOR_*`, `GIT_COMMITTER_*`; `SSH_AUTH_SOCK` dropped); `surface launch --json` gains `identity` (`operator` or `app`) and `token_expires_at`; `surface intake gh` gains the `worker-labeled` refusal reason in `--json` `reasons`; `doctor` gains an `identity` check. No new verb, no new exit code.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `surface`: worker environment and posture, a new worker GitHub identity requirement, and an intake label-trust requirement.

## Impact

- Code: `internal/launch/invocation.go` (allowlist, env), a new `internal/githubapp` package (JWT, mint, revoke, redacting token type), `internal/surface/drain` (refresh, revoke on reap), `internal/cli/surface_*.go` (close, intake), `internal/config` (table), `internal/doctor`.
- Docs: `docs/herdr.md` (worker environment, intake), `docs/configuration.md`, ADR-0010 amendment 2026-10-20.
- Operator setup (Cameron): create and install the App, store its key, apply the `update` rule from cameronsjo/forgectl#1134 §5. Listed in `design.md` § Setup.
- Not a boundary. A worker runs as the operator's uid and can still read the operator's keychain `gh` login and the App key on purpose. This stops accidents and fixes attribution, not adversaries (cameronsjo/cadence-ecosystem ADR-0046).
