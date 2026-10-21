## MODIFIED Requirements

### Requirement: Folder trust preflight
A claude or codex launch MUST be refused before anything is created when its harness has not trusted the launch folder (for a worker, its repository root) for the account the session runs as, naming the fix: `cd <repo root> && claude` for claude, `cd <repo root> && codex` for codex. For claude, trust of the folder or any parent counts. For codex, only an entry for the folder or its repository root counts, by exact or physical path, and any recorded `trust_level` counts as decided. The account is `CLAUDE_CONFIG_DIR` for claude and `CODEX_HOME` for codex. forgectl MUST NOT write trust itself. A missing or unreadable account config refuses nothing.

#### Scenario: Untrusted repository
- **WHEN** a claude worker launch targets a repository absent from the account's trusted folders
- **THEN** the launch exits non-zero before creating a worktree, branch, ledger row, or surface

#### Scenario: Untrusted codex repository
- **WHEN** a codex launch, worker or `--dry-run`, targets a repository with no trust entry in `$CODEX_HOME/config.toml`
- **THEN** the launch exits non-zero before creating anything, naming `cd <repo root> && codex`

#### Scenario: Codex trust does not inherit
- **WHEN** codex trusts a parent folder but has no entry for the repository root
- **THEN** a codex launch into the repository is refused
