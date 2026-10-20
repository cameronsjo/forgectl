# pr-review Specification

## Purpose
`forgectl pr` prepares and launches isolated, deny-by-default clean-room reviews of pull requests and local commits in tmux windows, and manages their lifecycle: the concurrency cap, the queue and drainer, teardown, repair, and the durable findings of local reviews.

## Requirements

### Requirement: Ref resolution and dry run
`forgectl pr <ref>` SHALL accept `owner/repo#N`, a PR URL, or a bare `N` resolved from the checkout's remote. `--dry-run` SHALL resolve the PR and print the plan (ref, head, agent) and MUST create no workspace, window, or breadcrumb.

#### Scenario: Dry run of a PR
- **WHEN** the operator runs `forgectl pr owner/repo#42 --dry-run`
- **THEN** the head ref, head oid, and agent are printed
- **AND** no workspace, tmux window, or session record exists afterwards

### Requirement: Clean-room workspace
A review SHALL run in a fresh workspace outside the operator's checkout, with every agent-instruction carrier from the quarantine target list (`CLAUDE.md`, `AGENTS.md`, `.claude/`, `.mcp.json`, and the rest) hidden before the agent starts. forgectl MUST write nothing else into the workspace.

#### Scenario: PR head carries a CLAUDE.md
- **WHEN** the PR head contains `CLAUDE.md` and `.claude/settings.json`
- **THEN** both are quarantined in the workspace before the reviewer is dispatched

### Requirement: Fixed reviewer posture
The Claude reviewer MUST never run with `--allow-dangerously-skip-permissions`, MUST always run with `--strict-mcp-config`, without the ambient `add_dir`, and with `--setting-sources ""` so no user, project, or local settings file or hook loads. Its permission profile, sandbox, and `disableAllHooks` SHALL come from a read-only settings file outside the workspace. `[pr] model` and `effort` SHALL be the only tunables; a value starting with `-` MUST be refused.

#### Scenario: PR author ships hooks
- **WHEN** the PR head defines hooks in a `.claude/settings.json`
- **THEN** the reviewer loads no settings file from the workspace and runs no hook

#### Scenario: Option-shaped model value
- **WHEN** `[pr] model = "--dangerously-skip-permissions"`
- **THEN** the review is refused before dispatch

### Requirement: Sandbox preflight refuses rather than degrades
Before every Claude dispatch, forgectl MUST verify the installed `claude` is 2.1.284 or later and that `claude doctor` accepts the exact settings document, and on Linux that `bwrap` and `socat` are installed. If any check fails the review MUST be refused rather than run without its sandbox. The sandbox SHALL deny writes to the workspace, limit network to the PR's host (none for `pr local`), and deny reads of common credential stores.

#### Scenario: Missing bubblewrap on Linux
- **WHEN** `bwrap` is not on PATH on a Linux host
- **THEN** `forgectl pr` refuses to dispatch the Claude reviewer and names the missing tool

### Requirement: The reviewer cannot post or push
The reviewer's permission profile MUST deny GitHub write commands (`gh pr review`, `gh pr comment`, `gh pr merge`, `gh api`, `gh issue`, `gh repo` and similar), `git push`, `git commit`, `curl`, and `wget`, and its prompt SHALL instruct it to output the review only. forgectl's launch commands SHALL NOT post anything to the PR.

#### Scenario: Injected instruction to approve
- **WHEN** a diff instructs the reviewer to run `gh pr review --approve`
- **THEN** the permission profile refuses the command

### Requirement: Codex is refused for untrusted code
`--agent codex` MUST be refused for every PR ref, because the code is third-party by construction and Codex cannot confine which commands run. For `pr local`, Codex MUST be refused unless `--operator-authored` asserts the operator wrote the code. The refusal SHALL run before any slot is reserved, leaving nothing on disk.

#### Scenario: Codex on a PR
- **WHEN** the operator runs `forgectl pr owner/repo#42 --agent codex`
- **THEN** the command refuses, recommends `--agent claude`, and creates no record

### Requirement: PR host pin in the review window
Each PR SHALL be viewed, cloned, and reviewed on its own host: the configured `[github] host` for `owner/repo#N` or a listed row, a pasted URL's host, or the checkout remote's host for a bare `N`. The review window MUST carry `GH_ENTERPRISE_TOKEN` and `GITHUB_ENTERPRISE_TOKEN` empty, and on a non-github.com host MUST also empty `GH_TOKEN`, `GITHUB_TOKEN` and set `GH_HOST` to that host. A launch env URL carrying userinfo or a query string MUST be refused.

#### Scenario: Pasted enterprise URL
- **WHEN** the operator reviews `https://ghe.example.com/o/r/pull/7` with `GH_TOKEN` exported
- **THEN** the window targets `ghe.example.com` with every gh token variable empty

### Requirement: Concurrency cap on every launch path
`[pr] max_concurrent` (default 4; `<= 0` means 4) SHALL govern `pr <ref>`, `pr local`, `pr pick`, and `pr drain`. Records in `preparing`, `prepared`, `launching`, and `active` SHALL hold a slot; `queued` and `needs-repair` MUST NOT. At the cap, `pr <ref>` and `pr local` MUST refuse with nothing prepared. `pr <ref> --queue` SHALL write a `queued` record and exit 0; `pr pick` SHALL queue PRs past the cap and exit 0.

#### Scenario: Cap reached
- **WHEN** 4 reviews hold slots and the operator runs `forgectl pr owner/repo#42`
- **THEN** it prints `review cap reached (max 4, 4 running)` with the queue and config hints and creates no workspace

### Requirement: Dispatch capability floor and verification
Before any workspace exists, forgectl MUST check `tmux -V` is 2.2 or newer and refuse otherwise, creating no clean room, record, or tmux session. After dispatch it SHALL wait eight seconds, list windows once, and report any review whose window already vanished; an unreadable tmux MUST NOT be reported as a gone review. `--no-verify` SHALL skip only that wait and check.

#### Scenario: Old tmux
- **WHEN** `tmux -V` reports `tmux 2.1`
- **THEN** the launch is refused with `tmux 2.2 or newer is required` and the found version, and nothing is created

### Requirement: Session listing reflects observed state
`pr list` SHALL print tab-separated rows of ref, creation time, breadcrumb path, observed status (`live`, `window gone`, `workspace missing`, `no tmux server`, `?`), recorded phase, and repair reason, appending new fields so the breadcrumb stays field 3. `--json` SHALL carry `phase` and `repair_reason`. Records the build cannot read SHALL produce a stderr count with exit 0, and any command that counts records MUST refuse while one exists.

#### Scenario: Torn session record
- **WHEN** one session record is unreadable
- **THEN** `pr list` exits 0 and prints `1 record(s) could not be read` on stderr
- **AND** `forgectl pr <ref>` refuses to launch until it is settled

### Requirement: Teardown refuses rather than guesses
`pr teardown <breadcrumb>` and `pr cleanup <date>` SHALL kill the review window under the lifecycle lock with a 3-second tmux budget, then restore and remove the workspace. When tmux times out, errors, or shows duplicate same-named windows, teardown MUST remove nothing, park the record in `needs-repair` with a reason, keep the workspace, and exit non-zero. A record whose bytes or identity changed underneath it MUST be refused and left in place.

#### Scenario: Wedged tmux
- **WHEN** tmux does not answer within the budget during teardown
- **THEN** the record is parked in `needs-repair` with a reason starting `window kill timed out (tmux unresponsive)` and the workspace is kept

### Requirement: Audit trail before every destructive verb
`pr teardown`, `pr cleanup`, `pr findings cleanup --apply`, and every `pr repair --apply` or `--prune` MUST append an intent row to `<sessions dir>/repair.jsonl` before mutating anything and complete it afterwards; a refusal MUST write no row. `pr history [--json]` SHALL show the newest 2000 rows with the writing verb, keeping notes on stderr so `--json` stays a bare array.

#### Scenario: Teardown of a missing workspace
- **WHEN** the operator tears down a session whose workspace is gone
- **THEN** only the breadcrumb is unlinked, and `pr history` shows an intent row and its completion for it

### Requirement: Repair inspects and settles stuck sessions
`pr repair` with no arguments SHALL list unsettled and unreadable records and exit 1 when anything needs settling, 0 otherwise, in both output shapes. `--apply` MUST take exactly one of `--adopt-window`, `--rollback`, `--forget-if-absent`; `--rollback` MUST refuse while the window is live or the window list is unreadable; off a terminal, destructive modes MUST require `--yes`.

#### Scenario: Rollback with a live window
- **WHEN** the operator runs `pr repair <breadcrumb> --apply --rollback` while the review window is live
- **THEN** the command refuses and writes no audit row

#### Scenario: Nothing to settle
- **WHEN** no record is unsettled or unreadable
- **THEN** `pr repair` exits 0

### Requirement: Unreadable records are set aside, not deleted
`pr repair <breadcrumb> --apply --forget-if-absent` on an unreadable record SHALL rename it to `<name>.json.unreadable-<timestamp>` rather than delete it, and MUST refuse while a ref read from its bytes names a live window. Only `pr repair --prune` SHALL unlink set-aside files, aging them by the name stamp (never mtime) against `--older-than` (default 30d, minimum 1h); a file whose stamp is unreadable or before 2025 MUST be kept.

#### Scenario: Set-aside file with an untrusted stamp
- **WHEN** a set-aside record's name stamp dates to 1970
- **THEN** `pr repair --prune` lists it as undated and keeps it

### Requirement: Queue drainer
`pr queue` SHALL list queued records oldest first. `pr drain` SHALL run one pass by default, claim the oldest queued records up to the free slots under the lifecycle lock, and launch each through the same prepare and launch path as `pr <ref>`. A failed launch SHALL return the record to `queued`; at 3 failed attempts it MUST be parked in `needs-repair`. A pass SHALL exit 1 on refusal or any failed launch; `--watch` SHALL exit only after three consecutive whole-pass refusals.

#### Scenario: Unreadable record blocks a pass
- **WHEN** any session record cannot be read
- **THEN** `pr drain` refuses the whole pass and exits 1

#### Scenario: Interval without watch
- **WHEN** the operator passes `--interval 30s` without `--watch`
- **THEN** the command refuses

### Requirement: Offline local review and findings reclaim
`pr local [path]` SHALL review the committed HEAD of a local repo in a clean-room worktree with no network CLI and one writable findings dir under the findings store; it SHALL have no `--queue`. `pr findings cleanup` SHALL be a dry run unless `--apply`, default `--older-than` to 720h, never offer a dir whose owning session record still exists, and MUST refuse when the store is not owned by the operator or is group- or world-writable.

#### Scenario: Dry-run cleanup
- **WHEN** the operator runs `forgectl pr findings cleanup`
- **THEN** reclaimable findings dirs are listed and none is removed

#### Scenario: Running review's findings
- **WHEN** a findings dir's owner marker names a session record that still exists
- **THEN** cleanup does not offer it, even at `--older-than 0`

### Requirement: PR inbox verbs and headless pick
`pr prs` and `pr dash` SHALL search as `@me` on the configured `[github] host`. When every gh query fails, `pr prs` MUST exit 1 with each note in the failure object under `--json`, and `pr dash` SHALL show active reviews then exit 1; one failed query of several SHALL be a note with exit 0. `pr pick` SHALL show a picker only when stdin and stdout are terminals, and otherwise print sanitized `owner/repo#N` rows and exit 1.

#### Scenario: Headless pick
- **WHEN** `forgectl pr pick` runs with stdout piped and several open PRs
- **THEN** each PR is printed as `owner/repo#N` on stdout and the command exits 1
