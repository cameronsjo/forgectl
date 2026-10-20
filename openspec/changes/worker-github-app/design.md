## Context

A worker is a full harness session running as the operator (ADR-0010, 2026-10-06 and 2026-10-08 amendments). It inherits an environment allowlist that includes `SSH_AUTH_SOCK`, and it reaches the operator's keychain `gh` login because it shares the operator's `HOME`. cameronsjo/forgectl#1134 sketched a worker GitHub App and a sandbox; this change builds the App half and leaves the sandbox (§4) and Codex isolation (§6) in #1134.

## Goals / Non-Goals

**Goals:**

- A worker's GitHub writes (push, PR, comment) are made by the App, so GitHub records them as `bot_login`.
- Intake can refuse a label a worker applied.
- A worker cannot, by accident, use the operator's `gh` login or SSH keys.
- No change at all for a machine with no `[surface.identity]` table.

**Non-Goals:**

- Containment. Same uid means a deliberate worker can read the operator's keychain and the App key. The sandbox in #1134 §4 is the path to that, and stays deferred.
- Signing the queue. A worker can still write `queue.json` directly (ADR-0010, 2026-10-09). Terminal confirmation stays.
- Codex-specific isolation (#1092).

## Setup (Cameron, once)

1. Create the App at <https://github.com/settings/apps/new>:
   - Name `forgectl-worker`. Homepage: the forgectl repository URL. Webhook: off.
   - Repository permissions: Contents read and write, Pull requests read and write, Issues read-only, Metadata read-only. Nothing else. No Workflows, so a worker cannot push a change under `.github/workflows/`; that is intended.
   - "Where can this GitHub App be installed?": Only on this account.
2. Install it on the repositories the drain works in, by "Only select repositories". Note the installation id from the installation page URL.
3. Generate a private key. Store it base64-encoded, because `security find-generic-password -w` prints a multi-line secret as hex. A trailing `-w` with no value makes `security` prompt for it, so the key never sits in argv:

   ```bash
   base64 -i forgectl-worker.pem | tr -d '\n' | pbcopy
   security add-generic-password -U -s forgectl-worker-app -a forgectl -w
   # paste at the prompt, twice
   pbcopy < /dev/null
   rm -P forgectl-worker.pem
   ```

4. Read the bot's user id: `gh api '/users/forgectl-worker%5Bbot%5D' --jq .id`.
5. Add to the forgectl config:

   ```toml
   [surface.identity]
   app_id = 123456
   installation_id = 7890123
   bot_login = "forgectl-worker[bot]"
   bot_user_id = 987654321
   ```

6. Apply the `update` rule from #1134 §5 so the App cannot move `main`, and run its proof: the App token's merge of a test PR fails, and an admin merge still works. Until this is done, Pull requests write lets the App merge through the API.
7. Run `forgectl doctor` and check the `identity` line reads `ok`.

## Decisions

**Mint per launch, scoped to one repository.** `POST /app/installations/{id}/access_tokens` with `repositories: [<repo>]` and `permissions: {contents: write, pull_requests: write, issues: read, metadata: read}`. The JWT is RS256, `iat` 60 s in the past, `exp` 9 minutes out, signed with the stdlib. Requesting fewer permissions than the installation holds is allowed, so a later App permission added by hand does not widen workers.

**Deliver through a private `gh` config directory, not `GH_TOKEN`.** An environment variable cannot change inside a running session, and the token expires in an hour. `gh` reads `hosts.yml` on every invocation, so the drain can refresh the file under a live worker. Layout: `$XDG_STATE_HOME/forgectl/surface/identity/<worker>/` (0700), holding `hosts.yml` (0600):

```yaml
github.com:
    user: forgectl-worker[bot]
    oauth_token: <token>
    git_protocol: https
```

The worker gets `GH_CONFIG_DIR` pointing there. The keyring lookup is keyed by user, and no keyring entry exists for `bot_login`, so `gh` falls back to the file. Task 1.2 verifies this on the M5 before anything else is built on it.

**git over HTTPS through `gh`.** The worker env carries `GIT_CONFIG_COUNT` entries: an empty `credential.https://github.com.helper` (clears inherited helpers), then `!gh auth git-credential`; `url.https://github.com/.insteadOf` for `git@github.com:` and `ssh://git@github.com/`. `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` name the bot, the email being `<bot_user_id>+<bot_login>@users.noreply.github.com`, so GitHub links commits to the App. `SSH_AUTH_SOCK` leaves the allowlist when the identity is configured.

**Refresh in the drain.** `_drain` already loops on `interval`. Each pass re-mints for any live worker whose token is older than 45 minutes and replaces `hosts.yml` by write-then-rename. A worker started by `surface launch --worktree` outside a running drain gets no refresh; `--json` reports `token_expires_at`, and the launch prints the expiry.

**Revoke on close and reap.** `DELETE /installation/token` with the worker's own token, then remove the directory. A failed revoke is logged and the directory still goes; the token dies within the hour.

**Intake trusts labels by author.** A candidate whose issue author or `queue:drain` labeler equals `bot_login` (or whose user id equals `bot_user_id`) is refused with reason `worker-labeled`, before the confirmation prompt shows it. With Issues read-only, the App cannot label at all, so this check is the backstop for a later permission change.

**Fail closed for workers, open for the operator.** With `[surface.identity]` set, a mint failure refuses the launch (exit 1, message naming the failed step). Falling back to the operator identity would silently undo the change. With no table, nothing is minted.

## Risks / Trade-offs

- **Required signed commits.** If a ruleset on a target repository requires signed commits, bot commits pushed over git are unsigned and the push is refused. Task 1.3 checks the rulesets first. If one requires signatures, commits go through the REST API instead, which GitHub signs.
- **Workflows permission is absent.** A brief that edits a workflow file fails at push. That is the intended scope; the brief or the operator handles those.
- **Operator credentials stay reachable.** Same uid. Stated in the ADR amendment, `docs/herdr.md`, and the doctor line.
- **Key on disk via keychain.** The App key lives in the login keychain. Reading it at launch can prompt if the keychain is locked; the read uses the existing 15 s timeout pattern from `internal/tasks`.

## Migration Plan

1. Land the code with no config change: behavior is unchanged.
2. Cameron runs the Setup steps.
3. Run one worker on a test issue; confirm its PR and commits show `bot_login`, and an intake run refuses a label it applied.
4. Rollback: remove `[surface.identity]`. Workers return to the operator identity on their next launch.

## Open Questions

- Which repositories to install on. Default: the ones the drain's launch profiles name today.
