## Purpose
`forgectl audit` runs read-only security posture scans across the projects root: an inventory of every agent-instruction carrier (the prompt-injection surface) and a secret-hygiene scan for stray env files, private keys, and gitleaks findings.

## ADDED Requirements

### Requirement: Read-only scans of the projects root
Every `audit` verb MUST be read-only: it SHALL write nothing to the scanned tree. The scans SHALL walk the projects root (`$PROJECTS_DIR`, else `~/Projects`), skip `.git` directories, and never follow a symlink. Bare `forgectl audit` SHALL list the available scans (`injection`, `secrets`).

#### Scenario: A scan leaves the tree untouched
- **WHEN** the operator runs `forgectl audit injection` or `forgectl audit secrets`
- **THEN** no file or directory under the projects root is created, modified, or removed

#### Scenario: A symlinked directory is reported, not walked
- **WHEN** a directory under the projects root is a symlink
- **THEN** the scan does not descend through it

### Requirement: Confinement to the projects root
The scans SHALL open the projects root once and route every listing and stat below it through that root handle, so no listing or stat can resolve outside the root even if a symlink is swapped in mid-scan. Each directory MUST be opened non-blocking so a directory swapped for a FIFO fails at once instead of hanging the scan.

#### Scenario: A symlink escaping the root
- **WHEN** an entry under the projects root is a symlink whose target lies outside the root
- **THEN** the scan never reads or stats the outside target through it

### Requirement: Walk caps and truncation reporting
The walk SHALL stop at 1,000,000 entries or 10,000 findings, and SHALL skip directories deeper than 32 levels while continuing. Any cap MUST set `truncated` and add its name (`entries`, `findings`, `depth`) to `capped_by`; `depth_skipped` SHALL count skipped directories, and unlistable directories SHALL be counted in `unreadable_dirs`. The text form SHALL end with one note per cap hit.

#### Scenario: Depth cap reached
- **WHEN** the tree holds directories more than 32 levels below the root
- **THEN** those directories are not scanned and the scan continues elsewhere
- **AND** the report has `truncated: true`, `depth` in `capped_by`, and a non-zero `depth_skipped`

### Requirement: Injection inventory classes
`audit injection` SHALL report every agent-instruction carrier from the same target list quarantine uses: `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md` in any directory; `.claude/`, `.mcp.json`, `.cursor/`, `.cursorrules`, `.github/instructions/` at a repo root; and MCP config directly in a dot-directory. Matching MUST ignore ASCII case. It SHALL report paths, types and mtimes only and MUST NOT read file contents.

#### Scenario: A nested CLAUDE.md
- **WHEN** a repo holds `docs/CLAUDE.md`
- **THEN** it is reported as a carrier with target `CLAUDE.md` and its enclosing repo

#### Scenario: A matched directory is not descended
- **WHEN** a repo has a `.claude/` directory containing worktrees
- **THEN** `.claude/` is reported as one carrier and nothing beneath it is scanned separately

### Requirement: Injection anomaly flags
Each carrier SHALL carry anomaly flags: `vendored` inside a dependency directory (`node_modules`, `vendor`, `third_party`, `.venv`, `site-packages` and similar), `off-root` for a root-only carrier not at a git working-tree root, `recent` when its own mtime is within the last 7 days, and `symlink` for a symlink at a repo root that starts a multi-segment carrier and is not confirmed to point at a file or nothing inside the root.

#### Scenario: A vendored carrier
- **WHEN** an `AGENTS.md` sits under `node_modules/`
- **THEN** its row carries the `vendored` flag

#### Scenario: A root-only carrier in a subdirectory
- **WHEN** a `.mcp.json` sits in a repo subdirectory
- **THEN** its row carries the `off-root` flag

### Requirement: Injection JSON shape and exit codes
`audit injection --json` SHALL write one object with `root`, `repos_scanned`, `entries_scanned`, `unreadable_dirs`, `truncated`, `capped_by`, `depth_skipped`, and `carriers` (each `path`, `repo`, `target`, `type`, `modified`, `anomalies`) through the terminal-safe encoder. Arrays MUST never be `null`. The verb SHALL exit 0 whenever the scan ran, and 1 when the root cannot be resolved or opened.

#### Scenario: Findings do not fail the scan
- **WHEN** the scan finds carriers or hits a cap
- **THEN** it exits 0

#### Scenario: Missing projects root under --json
- **WHEN** the projects root cannot be opened and `--json` is set
- **THEN** the verb exits 1 and writes one `{"error","code","path"}` object to stderr

### Requirement: Secrets native checks
`audit secrets` SHALL always report `env` files (`.env`, `.env.*`, `.envrc`, skipping template suffixes and never matching a directory), `key` files (ssh private key names, `*.p12`, `*.pfx`, `*.keystore`, and a `*.pem` or `*.key` whose first 4 KiB contain `PRIVATE KEY-----`), and `scanner-config` files (`.gitleaks.toml`, `.gitleaksignore`). The header check MUST keep only a yes or no and MUST NOT read a FIFO or non-regular file.

#### Scenario: A template env file is skipped
- **WHEN** a repo holds `.env.example`
- **THEN** it is not reported

#### Scenario: A symlinked key is listed unread
- **WHEN** a `cert.pem` is a symlink
- **THEN** it is listed as kind `key`, type `symlink`, and its target is never read or followed

### Requirement: Secrets git status flags
Tracked and ignored status SHALL come from hardened `git ls-files` calls with literal pathspecs. A `.env` file git ignores SHALL be counted in `ignored_env_files`, not listed; keys SHALL be listed whether or not ignored. Findings SHALL carry flags `tracked`, `unignored`, `outside-repo`, `git-unknown`, `vendored`, `loose` (group or other permission bits on unix), and `foreign-owner` (a key owned by another uid). A file git cannot report on MUST be `git-unknown`, never assumed ignored.

#### Scenario: A committed env file
- **WHEN** git tracks a repo's `.env`
- **THEN** the finding carries the `tracked` flag

#### Scenario: An untracked env file with no ignore rule
- **WHEN** a `.env` is untracked and no ignore rule covers it
- **THEN** the finding carries the `unignored` flag

### Requirement: Hardened gitleaks invocation
`--gitleaks` SHALL accept `auto` (default), `off`, or `require`. A usable gitleaks MUST be an absolute PATH hit outside the projects root (resolved and as spelled), version 8.19.0 or later. It SHALL run `gitleaks dir` per repo with forgectl's own default-rules config, `--redact`, `--ignore-gitleaks-allow`, a private ignore path, and `GITLEAKS_CONFIG`/`GITLEAKS_CONFIG_TOML` removed. A repo whose root scanner config is not a regular file MUST be skipped.

#### Scenario: gitleaks inside the projects root
- **WHEN** the only gitleaks on PATH lives under the projects root
- **THEN** it is not run and `gitleaks.status` is `refused` with reason `under_scan_root`

#### Scenario: A repo cannot allowlist its own findings
- **WHEN** a scanned file carries a `gitleaks:allow` comment
- **THEN** the finding is still reported

### Requirement: Secret values never surface
From gitleaks reports, forgectl SHALL decode only rule id, file, start line, and fingerprint; secret, match, and line text MUST never be decoded or printed, and text rows SHALL pass through credential redaction. Findings whose file is outside the reported repo SHALL be dropped and counted in `findings_rejected`; at most 10,000 gitleaks findings SHALL be kept.

#### Scenario: A detected token
- **WHEN** gitleaks reports a GitHub token in a repo file
- **THEN** the output names the path, rule, line, and fingerprint and never the token text

### Requirement: Secrets scan budget
`--timeout` (default 10m) SHALL be one deadline for every child process the scan starts. Each repo SHALL get the smaller of its own cap (30 seconds for git, 5 minutes for gitleaks) and the remaining budget, and its process group MUST be killed when its slice ends. Once the budget is spent nothing more SHALL start: remaining findings are `git-unknown` and unfinished repos appear in `repos_skipped` with reason `budget_exhausted`.

#### Scenario: Budget runs out mid-scan
- **WHEN** the budget is spent before every repo's git status and gitleaks run finish
- **THEN** the remaining repos are reported as budget-exhausted and the verb exits 1

### Requirement: Secrets JSON and exit codes
`audit secrets --json` SHALL write the report (native counts, `timeout`, `findings`, and a `gitleaks` block with `mode`, `status`, `reason`, versions, counters, `repos_skipped`, `findings`) to stdout even when exiting 1. Exit SHALL be 0 when the scan ran; 1 on an unopenable root, a spent budget, a gitleaks failure, skip, or failed version probe, or `require` without a gitleaks run; 2 for a bad `--gitleaks` or `--timeout` value.

#### Scenario: gitleaks absent under auto
- **WHEN** gitleaks is not installed and `--gitleaks=auto`
- **THEN** the native scan runs, the first text line says gitleaks did not run, and the verb exits 0

#### Scenario: gitleaks required but absent
- **WHEN** gitleaks is not installed and `--gitleaks=require`
- **THEN** the verb exits 1

#### Scenario: Bad flag value
- **WHEN** the operator passes `--gitleaks=maybe`
- **THEN** the verb exits 2
