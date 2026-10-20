## MODIFIED Requirements

### Requirement: Worker posture and environment
A worker MUST inherit only an allowlisted environment (such as `PATH`, `HOME`, `TERM`, locale, `CLAUDE_CONFIG_DIR`, and herdr pane ids). `SSH_AUTH_SOCK` SHALL be in the allowlist only when no `[surface.identity]` table is configured. Its permission mode SHALL come from `[launch.worker]`, `acceptEdits` by default with `auto` allowed; `dontAsk`, `bypassPermissions`, and dangerous-skip flags MUST be refused. `--model` MUST be a plain token of at most 64 characters not starting with `-`; an unknown `--profile` is refused.

#### Scenario: Launcher secrets withheld
- **WHEN** the launching shell exports an unrelated API token variable
- **THEN** the worker's environment does not contain it

#### Scenario: SSH agent withheld when the worker has its own identity
- **WHEN** `[surface.identity]` is configured and the launching shell has `SSH_AUTH_SOCK` set
- **THEN** the worker's environment does not contain `SSH_AUTH_SOCK`

## ADDED Requirements

### Requirement: Worker GitHub identity
When `[surface.identity]` is configured, each worker launch SHALL mint an App installation token for the worker's one repository and hand it over only through a private `GH_CONFIG_DIR`. The worker's `git` MUST push over HTTPS with it and commit as `bot_login`. The token MUST NOT reach argv, the ledger, `--json`, a log line, or the brief. A mint failure MUST refuse the launch, never fall back to the operator.

#### Scenario: Worker writes as the App
- **WHEN** a worker launches with `[surface.identity]` configured and pushes a branch and opens a PR
- **THEN** GitHub records the push, the commits and the PR as `bot_login`

#### Scenario: Token scope
- **WHEN** a worker's token is minted
- **THEN** it covers only that repository, with Contents and Pull requests write and Issues and Metadata read

#### Scenario: Mint failure refuses the launch
- **WHEN** the App key cannot be read from the keychain
- **THEN** the launch is refused with a message naming the failed step, and no worker starts

### Requirement: Worker token lifecycle
The drain SHALL refresh a live worker's App token before it expires. `surface close` and the drain's reap SHALL revoke the token and remove the worker's identity directory. `surface launch --json` SHALL report `identity` (`operator` or `app`) and, for `app`, `token_expires_at`. With no `[surface.identity]` table, nothing is minted and `identity` is `operator`.

#### Scenario: Close revokes the token
- **WHEN** `surface close` closes a worker that holds an App token
- **THEN** the token is revoked and the worker's identity directory is removed

#### Scenario: No identity configured
- **WHEN** no `[surface.identity]` table exists
- **THEN** the worker's environment matches the allowlist, including `SSH_AUTH_SOCK`, and `--json` reports `identity: "operator"`

### Requirement: Intake refuses worker-applied labels
`surface intake gh` MUST refuse a candidate whose issue author or whose queue-label labeler is `bot_login` or has id `bot_user_id`, with reason `worker-labeled` in `--json` `reasons`, before the candidate is shown for confirmation. The terminal confirmation SHALL still apply to every other candidate.

#### Scenario: Bot-labeled issue refused
- **WHEN** an issue's `queue:drain` label was applied by `bot_login`
- **THEN** intake does not show or queue it, and reports `worker-labeled`
