# surface Specification

## Purpose
The `surface` command group starts a harness (claude, codex, or pi) inside a terminal manager (tmux, cmux, or herdr) without exposing its invocation, and lets a coordinator drive herdr workers it launched: wait for them, brief them, read their reports, message them, close them, and merge their pull requests under policy.

## Requirements

### Requirement: Explicit backend
`surface launch [target]` MUST require `--surface` naming `tmux`, `cmux`, or `herdr`; there is no default and no detection. A bare target name SHALL resolve beneath the projects root by exact match and an ambiguous name MUST be refused; a path may be anywhere. `--harness` overrides the profile's harness, and `--name` overrides the display name.

#### Scenario: Missing backend
- **WHEN** the user runs `forgectl surface launch .` without `--surface`
- **THEN** the launch is refused and no surface is created

#### Scenario: Ambiguous project name
- **WHEN** a bare target name matches more than one project
- **THEN** the launch is refused rather than guessing

### Requirement: Hidden invocation
A launch MUST NOT pass the resolved harness path, its arguments, its environment, or a prompt to the terminal manager. The manager SHALL learn only the target directory and the command it types; the harness invocation is delivered to a private trampoline over a local socket after the workspace exists.

#### Scenario: tmux sees no harness argv
- **WHEN** a launch runs with `--surface tmux`
- **THEN** the tmux command line carries no harness path, arguments, environment values, or prompt

### Requirement: Worker launch
`--worktree <branch>` SHALL require `--surface herdr` and `--name`, create a git worktree at `<repo>/.claude/worktrees/<name>` with repository hooks disabled, give the worker its own herdr workspace labeled with forgectl's ownership marker, and record a ledger row. A new branch MUST start at the GitHub default branch head when origin is on github.com, else at `HEAD`; a failing GitHub call fails the launch. An existing branch is checked out as is.

#### Scenario: Worktree without herdr
- **WHEN** `--worktree` is given with `--surface tmux`
- **THEN** the launch is refused

### Requirement: Worker posture and environment
A worker MUST inherit only an allowlisted environment (such as `PATH`, `HOME`, `TERM`, locale, `CLAUDE_CONFIG_DIR`, `SSH_AUTH_SOCK`, and herdr pane ids). Its permission mode SHALL come from `[launch.worker]`, `acceptEdits` by default with `auto` allowed; `dontAsk`, `bypassPermissions`, and dangerous-skip flags MUST be refused. `--model` MUST be a plain token of at most 64 characters not starting with `-`; an unknown `--profile` is refused.

#### Scenario: Launcher secrets withheld
- **WHEN** the launching shell exports an unrelated API token variable
- **THEN** the worker's environment does not contain it

### Requirement: Launch brief
`--brief <text|@file>` (worktree only, files at most 64 KiB) SHALL pass the first brief as the harness's prompt argument after `--`, so nothing is typed into the pane. Each brief MUST get a random marker recorded in the ledger before sending, and ask the worker to end with a `REPORT <marker>:` line. A pi launch MUST refuse a brief starting with `@`.

#### Scenario: Brief recorded before start
- **WHEN** a worker launches with `--brief @task.md`
- **THEN** the ledger row holds the brief's marker and the harness receives the brief as its prompt argument

### Requirement: Folder trust preflight
A claude or codex launch MUST be refused before anything is created when its harness has not trusted the launch folder (for a worker, its repository root) for the account the session runs as (`CLAUDE_CONFIG_DIR` for claude, `CODEX_HOME` for codex), naming the `cd <repo root> && <harness>` fix. forgectl MUST NOT write trust itself. A missing or unreadable account config refuses nothing.

#### Scenario: Untrusted repository
- **WHEN** a claude worker launch targets a repository absent from the account's trusted folders
- **THEN** the launch exits non-zero before creating a worktree, branch, ledger row, or surface

#### Scenario: Claude trust inherits
- **WHEN** Claude Code trusts a parent folder of the launch folder
- **THEN** a claude launch into it is not refused

#### Scenario: Untrusted codex repository
- **WHEN** a codex launch, worker or `--dry-run`, targets a repository with no trust entry in `$CODEX_HOME/config.toml`
- **THEN** the launch exits non-zero before creating anything, naming `cd <repo root> && codex`

#### Scenario: Codex trust does not inherit
- **WHEN** codex trusts a parent folder but has no entry for the repository root
- **THEN** a codex launch into the repository is refused

#### Scenario: Any recorded codex decision counts
- **WHEN** codex has an entry for the repository root, by exact or physical path, with any `trust_level`, `untrusted` included
- **THEN** a codex launch into it is not refused

### Requirement: Launch dry run and JSON
`surface launch --dry-run` SHALL run the launch's pre-write checks and print what it would create, creating no worktree, branch, ledger row, or surface and calling no backend, and MUST NOT print the harness path, arguments, environment, or brief. With `--json`, a real launch prints `{"surface","name","target","harness","ref","workspace_id","tab_id","pane_id","terminal_id","worker"}`, and a worker whose ledger row cannot be completed still prints the object and exits 1.

#### Scenario: Preview creates nothing
- **WHEN** the user runs a worker launch with `--dry-run --json`
- **THEN** a preview object with `dry_run: true` prints and no worktree, branch, or ledger row exists afterwards

### Requirement: Ownership-checked pane access
`ready`, `wait`, `read`, `brief`, and `tag` MUST read or write only the root pane of a workspace that carries forgectl's ownership marker, on the same herdr server the worker was launched on. A provably absent workspace is reported gone; any other failure is unreadable, never a verdict about the worker.

#### Scenario: Restarted herdr server
- **WHEN** the herdr server changed since launch
- **THEN** `surface brief` refuses to type into the restored workspace

### Requirement: Readiness never answers dialogs
`surface ready <name>` SHALL report ready only when no blocking screen matches, herdr names the expected agent `idle` or `done`, and the harness prompt is visible. A blocking screen (folder trust, plan approval, permission prompt) MUST end the wait with exit 1 naming it, and forgectl MUST NOT answer it. Predicates are built in, replaceable only by `<config dir>/forgectl/surface-ready.toml`, never read from a repo. Exit 0 ready, 1 blocked/gone/unreadable/timeout, 2 usage.

#### Scenario: Permission prompt showing
- **WHEN** the worker's pane shows a permission prompt
- **THEN** `surface ready` exits 1 naming the screen and sends no keys

### Requirement: Settled-turn wait
`surface wait <name>` SHALL exit 0 only when the ready verdict holds for `--settle` and either herdr reported `working` during the wait, the last brief's report is on screen, or the worker sat at its prompt for `--quiet`. It MUST NOT judge settledness by screen-text stability. A blocking screen ends the wait with exit 1; exit 2 is a usage error.

#### Scenario: Turn never started
- **WHEN** the worker sits at its prompt with no working status and no report until `--quiet` elapses
- **THEN** `surface wait` exits 0

### Requirement: Typed follow-up briefs
`surface brief <name> <text|@file>` SHALL type the text without Enter, read the screen back until it sits in the input box with no dialog, record the brief and a new marker, then send Enter separately. A typed brief MUST be one line of at most 600 characters including the report instruction and MUST NOT start with `- / ! # ? @ &`. It refuses inside a drain worker. Exit 1 `refused` sent nothing; exit 1 `unconfirmed` already pressed Enter.

#### Scenario: Read-back mismatch
- **WHEN** the input box does not show the typed text
- **THEN** Enter is never sent and `--json` reports outcome `refused`

### Requirement: Report reading
`surface read <name>` SHALL print the last `--lines` screen rows with control characters escaped. With `--report` it MUST print only the text after `REPORT <marker>:` on a line below the last echo of the latest brief's marker, so the brief's own instruction never counts. Exit 0 printed, 1 no report yet or unreadable, 2 usage (including `--report` with no recorded brief).

#### Scenario: Only the echo on screen
- **WHEN** the screen shows the brief's instruction but no later REPORT line
- **THEN** `surface read --report` exits 1

### Requirement: Worker listing
`surface list` SHALL show each ledger row in the repo for the current herdr session as `present`, `gone`, or `unreadable`, where a herdr error is unreadable, never gone. `--orphans` MUST show only rows whose workspace is gone or whose stage is not `launched`, excluding launches at `pending` or `worktree` under ten minutes old. `--json` includes session id, terminal id, and transcript path where known. Exit 0 listed, 2 setup error.

#### Scenario: Young launch not an orphan
- **WHEN** a row has been at stage `pending` for two minutes
- **THEN** `surface list --orphans` does not list it

### Requirement: Safe close
`surface close <name>` SHALL close the worker's workspace first, then remove its worktree only when `git status --porcelain --ignored` is empty, HEAD is on a branch, the branch has nothing beyond its upstream (or start commit), and no stash names the branch, using `git worktree remove` without `--force`. It MUST NOT delete the branch.

#### Scenario: Unpushed commits
- **WHEN** the worker branch has commits not on its upstream
- **THEN** the workspace closes, the worktree is kept with the failed check named, and the row stays at stage `closed`

### Requirement: Close refusals and preview
`surface close` MUST refuse, touching nothing and exiting 1, when herdr cannot be read, the workspace cannot be proven forgectl's, or the row is a launch at `pending` or `worktree` under ten minutes old. `--dry-run` SHALL run only the checks that need no write, ask nothing of herdr, change nothing, and exit 1 where close would refuse.

#### Scenario: Preview of a refusal
- **WHEN** `surface close x --dry-run` runs on a launch started one minute ago
- **THEN** nothing changes and the exit is 1, so `close --dry-run && close` stops

### Requirement: Cross-harness messaging
`surface send <to> <text|@file|->` SHALL deliver a message to the coordinator or a ledger worker through that harness's channel, marked as coming from another agent rather than the operator. It MUST exit 0 sent, 75 queued for later retry, 1 failed, 2 usage, and 4 refused when worker-to-worker messages are off.

#### Scenario: Recipient not ready
- **WHEN** the recipient's harness cannot take the message yet
- **THEN** the message stays queued and `surface send` exits 75

### Requirement: Policy-gated merge
`surface merge <name>` SHALL merge a worker's PR only when the `[surface.merge]` policy, which is `off` unless set, passes at one head, and only after a person types `yes` at `/dev/tty` with stdin also a terminal; no flag skips this. It MUST refuse with exit 2 inside a drain worker (`FORGECTL_DRAIN_WORKER` set). Exit 0 merged or would-merge, 1 refused, unreadable, or not confirmed merged, 2 usage or setup.

#### Scenario: No terminal
- **WHEN** `surface merge` runs with stdin piped and no terminal
- **THEN** nothing merges, a refusal is audited, and the exit is 2

#### Scenario: Dry run
- **WHEN** `surface merge <name> --dry-run` runs and the policy passes
- **THEN** `--json` reports `result: would-merge`, nothing merges, and no audit line is written

### Requirement: Merge execution guards
Before merging, `surface merge` MUST re-read the PR and required check runs and refuse on any change since the verdict, MUST write the audit attempt line to `merge-audit.jsonl` and merge nothing if it cannot, and SHALL merge with `gh pr merge --squash --match-head-commit <head>`, never `--admin` or `--auto`, using a forgectl-written body and a subject taken only from a conventional PR title.

#### Scenario: Head moved during confirmation
- **WHEN** a commit is pushed to the PR between the verdict and the merge
- **THEN** the merge is refused and nothing merges

### Requirement: Drain events by run
`surface drain events` SHALL print the events file oldest first. `seq` counts from 1 in each drain process. With `--since <n>` above 0 it MUST print only the latest run's events with `seq` above `n`. With no `--since`, or `--since 0`, it SHALL print every run. `--latest-run` MUST limit the output to the latest run, from its last `seq` 1 event on.

#### Scenario: Latest run only
- **WHEN** the events file holds two runs and `surface drain events --latest-run --json` runs
- **THEN** only the second run's events print, starting at its `seq` 1

#### Scenario: Zero cursor reads every run
- **WHEN** the events file holds two runs and `surface drain events --since 0 --json` runs
- **THEN** both runs' events print

### Requirement: Worker settings deny board closes
A claude worker's `--settings` MUST deny `Bash(forgectl tasks done:*)` in every permission mode, so a worker cannot close a board task through that shell command even where the operator's own allow rules would let it. Board MCP tools are not denied.

#### Scenario: Worker cannot run the board-close shell command
- **WHEN** a claude worker launches in any permission mode
- **THEN** its `--settings` deny list holds `Bash(forgectl tasks done:*)`
