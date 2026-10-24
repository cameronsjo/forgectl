## 1. Verify the premises (before any code)

- [ ] 1.1 Cameron completes `design.md` § Setup steps 1-5, then `forgectl doctor` shows `worker identity` `ok` (proves the keychain recipe round-trips the key).
- [ ] 1.2 On the M5: with a hand-minted token in a scratch `GH_CONFIG_DIR/hosts.yml` and `bot_login` as user, `gh api user` and `gh auth git-credential` use the file token, not the operator's keyring login. If not, revise the delivery decision before task 3.
- [ ] 1.3 Read the rulesets on every installed repository. If any requires signed commits, switch the push decision to REST-API commits and amend `design.md`.

## 2. Mint and revoke (`internal/githubapp`)

- [x] 2.1 Token type that renders `[redacted]` under every format, marshal and log path (mirror `internal/tasks` `Token`), with expiry.
- [x] 2.2 Read the base64 key from the keychain service (`/usr/bin/security`, 15 s timeout), decode, parse PKCS#1 or PKCS#8.
- [x] 2.3 RS256 JWT (`iat` -60 s, `exp` +9 min) with the stdlib; mint for one repository with the fixed permission set; revoke.
- [x] 2.4 `httptest` tests: request shape, scope, error text naming the failed step, no token in any error string.

## 3. Worker launch

- [x] 3.1 `[surface.identity]` config table: all keys but `keychain_service` required, unknown key or bad value makes the file invalid, and the kept section still refuses to resolve.
- [ ] 3.2 With the table: create the per-worker 0700 dir, write `hosts.yml` 0600, set `GH_CONFIG_DIR`, the `GIT_CONFIG_*` entries and bot author/committer; drop `SSH_AUTH_SOCK`. Mint failure refuses the launch.
- [ ] 3.3 `surface launch --json` reports `identity` and `token_expires_at`; dry run reports what it would mint, mints nothing.
- [ ] 3.4 Extend the allowlist test: with the table, no `SSH_AUTH_SOCK`, no `GH_TOKEN`, `GH_CONFIG_DIR` under the state dir; without it, today's env exactly.

## 4. Lifecycle

- [ ] 4.1 `_drain` refreshes tokens older than 45 minutes by write-then-rename.
- [ ] 4.2 `surface close` and the drain's reap revoke and remove the dir; a failed revoke is logged and the dir still goes.

## 5. Intake

- [x] 5.1 Confirmed intake already refuses any bot author or labeler (`intake.Admit`, tested in `internal/surface/intake/intake_test.go`); no code change.

## 6. Doctor

- [x] 6.1 `worker identity` doctor check: skip (no table) / fail (bad table, config did not load, key unreadable, App or installation refused, bot login or id mismatch) / warn (GitHub unreachable or 5xx) / ok. It reads the installation and the bot user, mints no token, and never prints key or token material.

## 7. Docs and ADR

- [ ] 7.1 ADR-0010 amendment 2026-10-20 (in this PR).
- [ ] 7.2 `docs/herdr.md` worker environment and intake sections, `docs/configuration.md` `[surface.identity]`, the forgectl skill if it names the env.
- [ ] 7.3 Update the matching `docs/commands/` page and the README Usage roster for the new `--json` fields and doctor check.
- [ ] 7.4 Comment on cameronsjo/forgectl#1134 that §1-3 and §5 moved here.

## 8. Roll out

- [ ] 8.1 Cameron applies the #1134 §5 `update` rule and its proof.
- [ ] 8.2 One worker on a test issue: PR and commits show `bot_login`; an intake run refuses a label the bot applied (applied by hand with the bot token for the test).
