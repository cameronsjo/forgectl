# forgectl

Personal dev-experience CLI for a headless macOS workbench driven over SSH — from laptops, phones, and Termius. What began as a tmux helper (superseding the ad-hoc bash `s` script; smart session-naming stays with `sesh`) is growing into the **workbench forge**: composable modules — tmux, projects, launch, workflow — with a declarative workflow DSL as the composition layer.

Built for two hands and one thumb:

- **Power mode** — typed verbs (`forgectl tmux ls`, `forgectl tmux pick`). Full keyboard, full control.
- **Thumb mode** — bare `forgectl` opens a TUI menu. Number-key select. Narrow-screen. Forgiving input. Works fine in Termius over mosh.

## Install

```sh
brew install cameronsjo/tap/forgectl
```

Requires `sesh` on `$PATH` for `tmux pick`/`tmux ls` (session smarts — path discovery, named sessions, zoxide integration). Optional, per feature: `gh` (`pr`, `review`, `projects`), `tea` (`projects` against the self-hosted Gitea, and `review` when `[review.gitea]` is enabled), `docker` (`bench status`/`up`, `docker build/run/shell`, and `clean --docker`), `npm`/`pnpm`/`pip`/`go`/`brew` (`clean --caches` — each is independently opt-in and skipped, not required, when absent). `update`'s roster shares that same `brew`/`go`/`npm` dependency (`softwareupdate` is a macOS built-in) — each step is independently scoped, so a missing tool fails only its own step, never the others.

Commands that actually launch a PR review — `forgectl pr <ref>`, `forgectl pr local`, and `forgectl pr pick` once admission establishes there is at least one ref to prepare — require **tmux 2.2 or newer**, for the dispatch-identity reasons in the `[pr]` section below. Read-only PR commands, any `--dry-run`, and empty, all-reviewed, or cap-full selections do not acquire that floor.

Reading a local clone's git state — `projects list`, `projects pick`, the project inventory, and `projects pull-all` — requires **Git 2.11.0 or newer**. That release introduced `git status --porcelain=v2 --branch`, which reports the working-tree state and the ahead/behind counts in a single command; forgectl reads both from that one call rather than spawning a second `rev-list` per clean repository. On older Git the command fails, the repository's status reads as unknown, and `pull-all` skips it rather than rebasing a tree whose state it could not establish.

## Usage

```sh
forgectl                   # open TUI menu (thumb mode)
forgectl --help            # list every command group (non-interactive entrypoint)
forgectl tmux ls           # list sessions
forgectl tmux pick [name]  # connect/smart-create via sesh (no name → list)
forgectl tmux kill <name>  # kill a session (--others keeps only it)
forgectl tmux rename <old> <new>
forgectl tmux windows      # list windows across all sessions
forgectl tmux tree         # session → window → pane tree
forgectl tmux last         # jump to the last-used session
forgectl tmux cheat        # tmux terms + the keys that matter
forgectl config            # show every config section, per-key set/default (alias: cfg)
forgectl config --json     # the same, machine-readable (stable surface)

# projects — cross-host project inventory (alias: proj)
forgectl projects list [query]           # list all projects: local clones + your GitHub.com repos + git.sjo.lol/cameron
forgectl projects list --json            # machine-readable JSON (safe to pipe; degradation notes go to stderr)
forgectl projects list --host github     # filter to one host: github | gitea
forgectl projects list --host gitea forge  # host filter + name substring
forgectl projects pick [query]           # picker with both descriptors TTY; otherwise sanitized candidates on stdout + exit 1 (aliases: p, open)
forgectl projects                        # shorthand for pick; same headless candidate/exit-1 contract
forgectl projects clone [query]          # picker with both descriptors TTY; otherwise candidates + exit 1 (use sshUrl from list --json)
forgectl projects worktree <query> [branch] # same ambiguity contract as clone; use sshUrl from list --json

# pr — clean-room pull-request review (the flagship review family)
forgectl pr <ref>                        # prepare + launch an isolated, deny-by-default review (owner/repo#N, a PR URL, or a bare N)
forgectl pr <ref> --dry-run              # resolve + print the plan, create nothing
forgectl pr prs                          # cross-repo open PRs (authored, assigned, review-requested); reviewed rows dimmed
forgectl pr prs --json                   # machine-readable JSON (safe to pipe; notes go to stderr)
forgectl pr dash                         # dashboard: active reviews, PRs awaiting you, your open PRs
forgectl pr pick                         # multiselect with both descriptors TTY; otherwise sanitized owner/repo#N rows on stdout + exit 1
forgectl pr reviewed mark <ref>          # mark a PR reviewed (dims it until the PR sees new activity)
forgectl pr reviewed unmark <ref>        # clear a PR's reviewed mark
forgectl pr reviewed sync                # prune reviewed marks for PRs that are no longer open
forgectl pr list                         # list active clean-room review sessions
forgectl pr attach <breadcrumb>          # jump to a review window (also: open <b>, teardown <b>)
                                          #   <breadcrumb> is the session path `pr list` prints
forgectl pr keys                         # tmux cheatsheet for driving a review

When both stdin and stdout are terminals, these selectors keep their existing
pickers. With either descriptor non-TTY, `projects`/`projects pick` emit one
sanitized display identity per candidate on stdout and exit 1; narrow pick to a
unique project name when possible or inspect `projects list --json`. Ambiguous
`projects clone` and `projects worktree` use the same rows and exit 1; obtain a
candidate's `sshUrl` from `projects list --json` for an exact target, or rerun
interactively when it has none. Project display rows are not universal command
arguments. Headless `pr pick` similarly emits sanitized `owner/repo#N` rows and
exits 1; each printed ref is directly usable with `forgectl pr <ref>`, while `pr prs
--json` remains the stable inventory.

# launch — per-project Claude Code / Codex CLI / Pi launcher (alias: cl)
forgectl launch                    # drop straight into the resolved profile (no prompt)
forgectl launch <harness args…>    # apply the project profile, then exec the configured harness
forgectl launch agents --json      # pure passthrough (byte-clean); posture injected only when interactive
forgectl launch which              # show the profile resolved for the current directory (alias: config)
forgectl launch init               # scaffold the [launch] section into config.toml
forgectl launch migrate            # explicitly import an existing claunch.conf without retiring it
forgectl launch init --from-claunch # deprecated alias for `launch migrate`
forgectl launch edit               # open config.toml in $EDITOR
forgectl launch doctor             # check harness availability + launch config validity

# resume — get back into a Claude Code session after a terminal restart
forgectl resume                    # pick from recent sessions across every repo, then resume in place
forgectl resume forgectl           # filter by repo, name, cwd, or id; one hit resumes it, several list the candidates
forgectl resume --fork             # branch a new session off the transcript — the only way into a still-running one
forgectl resume --dry-run forge    # resolve and print the cwd + claude argv, exec nothing (never prompts)
forgectl resume ls                 # list without acting (the only subcommand that returns)
forgectl resume ls --json          # machine-readable JSON (safe to pipe; counts go to stderr; see `resume ls --help` for the field table)
forgectl resume snapshot           # capture what a live session's exit would destroy
forgectl resume snapshot --quiet   # same, silent — the form a Stop hook uses

# workflow — run declarative workflows composing forgectl's other verbs (alias: flow)
forgectl workflow run <name>              # run a workflow by name
forgectl workflow run <name> --param k=v  # override a workflow param (repeatable)
forgectl workflow run <name> --dry-run    # print the resolved plan, run nothing
forgectl workflow run <name> --resume     # resume the last run from its first incomplete step
forgectl workflow status <name>           # show the last run's per-step checkpoint state
forgectl workflow list                    # show resolvable workflow names
forgectl workflow bless <name>            # approve a user workflow's exact bytes (Touch ID; macOS only)
forgectl workflow verify <name>           # check a workflow's blessing without running it
forgectl workflow trust init              # install the trust anchor (one-time)
forgectl workflow trust list              # show the enrolled keys

# bench — discover, health-check, and wire the local dev bench:
#   hearth (telemetry stack), chronicle (transcript-retention layer)
forgectl bench status                     # aggregate health card across all components
forgectl bench status --json              # machine-readable JSON (safe to pipe)
forgectl bench up                         # bring up the configured services via their own entrypoints
forgectl bench open [target]              # open a bench UI (hearth | grafana; default hearth)

# sessions — drain local session ledgers into the concordance (a shared Postgres index
# of every machine's session history, queried from any of them)
forgectl sessions sync --dry-run          # read + count the local JSONL WAL; no DB connection
forgectl sessions sync                    # idempotent upsert into the concordance + rebuild the runbook index
forgectl sessions sync --full             # bypass the lastMessageId watermark, re-upsert everything
forgectl sessions search "<query>"        # full-text search the concordance's runbook index from any machine
forgectl sessions why "<path|topic>"      # recent sessions whose runbooks explain a path or topic, newest first
forgectl sessions last <repo>             # the newest session in a repo + the artifacts it left behind

# env — safe .env management: key names visible, values never (see § env below)
forgectl env keys [--file .env]                             # list KEY names only — never values
forgectl env set KEY [--file .env] [--clipboard]             # value from piped stdin, no-echo prompt, or clipboard — never argv
forgectl env get KEY --clipboard [--file .env]               # value to clipboard only; no print path exists
forgectl env check [--file .env] [--example .env.example]    # missing/extra keys, names only (see § env below for exit codes)
forgectl env redact [--file .env]                            # print file with values masked ****
#   --file must name an env file (.env, .env.*, *.env); --any-file overrides, TTY-confirmed only

# branch — prune stale/orphaned git branches (alias: br)
forgectl branch                          # dry-run report: local + remote branches, classified against
                                          #   server-side PR truth (safe-to-delete | blocked | needs-attention)
forgectl branch --include-gone           # also surface upstream-gone branches with no server-confirmed merge
forgectl branch --apply                  # DESTRUCTIVE: delete everything classified safe-to-delete,
                                          #   after a confirmation prompt

# clean — reclaim dep/build directories under a project root (alias: cln)
forgectl clean                           # dry-run report against ~/Projects (node_modules, .venv, target, …)
forgectl clean --type node               # only one type: node|python|go|build
forgectl clean --apply                   # DESTRUCTIVE: delete everything reclaimable, after a confirmation
                                          #   prompt (skips dirty git trees unless --force)
forgectl clean --caches --apply          # DESTRUCTIVE, opt-in: also clear detected package-manager
                                          #   caches (npm/pnpm/pip/go/brew) behind one confirmation;
                                          #   brew ALSO removes old formula versions from the Cellar.
                                          #   Not combinable with --type (it only filters dirs)
forgectl clean --docker --apply          # DESTRUCTIVE, opt-in: also prune docker (containers/images/
                                          #   volumes/build cache), its own confirmation

# docker — build/run/shell images tagged from git repo/branch/sha
forgectl docker build [context]          # build, tagging {repo}:{branch}-{sha} and :dev
forgectl docker run [-- args...]         # run the built (or --tag) image
forgectl docker shell                    # open a shell in the built (or --tag) image

# docs — local markdown reader: render + serve an indexed doc set over loopback HTTP
forgectl docs serve [dir|file ...]       # render + serve, loopback-only (DNS-rebinding-safe)
forgectl docs serve --open               # also open the system browser
forgectl docs list [dir|file ...]        # list the indexed docs, no server (--json for scripting)

# net — check cached reachability of the configured probe endpoint
forgectl net                             # show the cached (or freshly probed) answer
forgectl net --refresh                   # force a new probe, bypassing the cache
forgectl net --json                      # machine-readable output for scripting

# ghostty — theme + keybind reporting, parsed live from the ghostty CLI
forgectl ghostty themes                  # custom themes, active one marked
forgectl ghostty themes --all            # also list the themes bundled with ghostty
forgectl ghostty cheat                   # keybind cheatsheet, parsed from +list-keybinds

# pip — comment- and whitespace-preserving pip.conf editor
forgectl pip remove                      # comment out [global] index-url (reversible)
forgectl pip restore                     # un-comment whatever remove last tagged
forgectl pip show                        # print the effective pip.conf

# quarantine — reversibly hide AI-instruction files (CLAUDE.md, AGENTS.md, …) from a workspace
forgectl quarantine                      # hide the default targets in cwd (same as `quarantine hide`)
forgectl quarantine restore              # rename quarantined targets back
forgectl quarantine status               # show which targets are hidden

# review — cross-project work inventory: open issues and PRs across your repos
forgectl review                          # unified table (reviewed rows dimmed)
forgectl review --kind issue             # issues only (or: pr)
forgectl review mark owner/repo#42       # mark an item reviewed

# update — weekly package-manager + OS maintenance, independently-scoped steps
forgectl update check                    # report-only for every step (brew/softwareupdate/go/npm), no mutation —
                                          #   each step's captured output (the actual finding) goes to stderr +
                                          #   the log, and to --json's own per-step field
forgectl update run                      # DESTRUCTIVE, one confirmation: brew (upgrade --formula/cleanup), go
                                          #   (cache clean), npm (update) are SKIPPED without --yes; softwareupdate
                                          #   (check-only, never installs) still runs. brew's cleanup ALSO removes
                                          #   the Cellar versions upgrade just superseded (the rollback path), and
                                          #   go's clean wipes the module cache machine-wide, for every project —
                                          #   the prompt names both when the relevant step is selected. brew is
                                          #   --formula-scoped in BOTH check and run: casks are out of scope
                                          #   entirely (a cask upgrade can hang on a sudo/GUI prompt this
                                          #   unattended tool has no stdin to answer) — upgrade a cask yourself
                                          #   with `brew upgrade --cask`
forgectl update run --yes                # skip the prompt, apply the same effects non-interactively (cron/CI)
forgectl update run --only brew,go       # restrict to a subset of the roster
forgectl update run --json               # machine-readable summary (with each step's output) to stdout ONLY;
                                          #   the full per-step transcript goes to stderr + a timestamped log
                                          #   (update-logs/, auto-pruned after 7 days)

# doctor — ecosystem health check: claude, tmux/ghostty/cmux, gh auth, config,
#   the local bench (hearth/chronicle), the trust store, forgectl's own currency
forgectl doctor                          # one line per check, with a remediation hint on any warn/fail
forgectl doctor --json                   # machine-readable report for scripting

# upgrade — update forgectl itself via the Homebrew tap (never `go build` over the brew-linked binary)
forgectl upgrade                         # brew update + brew upgrade --cask forgectl; brew owns the checksum + atomic install
forgectl upgrade --check                 # report whether an update is available, no mutation
                                          #   a source build (go build/go run) WARNS instead of attempting anything —
                                          #   there's no cask install to manage

# y — clipboard (macOS only) + read-only zsh history recall
echo hi | forgectl y copy                # copy stdin to the clipboard
forgectl y paste                         # print the clipboard's current contents
forgectl y last 5                        # print the 5 most recent zsh commands, oldest first
                                          #   reads $HISTFILE (default ~/.zsh_history); no shell shim, so it sees
                                          #   only what zsh has flushed — set INC_APPEND_HISTORY or SHARE_HISTORY
                                          #   in .zshrc for the current shell's commands to appear. zsh only.
                                          #   prints inline secrets verbatim if your history holds any — treat
                                          #   the output as sensitive. Interactive stdout is allowed by default;
                                          #   piping or redirecting requires --allow-sensitive-output, an explicit
                                          #   acknowledgement only: forgectl does not scan or redact the history
```

The cask doesn't stage an `fx` command — it's a shell alias you add yourself:

```sh
alias fx=forgectl     # add to your shell rc

fx                    # same as bare forgectl — opens the TUI
fx tmux ls
```

### External commands

An eligible unknown top-level verb can dispatch an external command by one
convention only: `forgectl <verb> ...` looks for `forgectl-<verb>` on `PATH`.
This is external-command dispatch, not an extension API: registered commands,
aliases, and built-in Cobra verbs always win. Exact workflow-name dispatch is a
future earlier rung, so it will also win before an external command.

Before the verb, only `--no-icons` or `--no-icons=<value>` is eligible; forgectl
consumes those host flags. Every token after the verb is forwarded exactly as
received, including empty, spaced, Unicode, flag-looking, and `--` arguments.
The external command inherits forgectl's stdin, stdout, stderr, and environment,
and replaces the forgectl process, so signals and exit status belong directly to
that command.

Resolution uses Go's `exec.LookPath`, then independently requires the result to
be an absolute path. A lookup error, a relative result, a verb containing `/` or
`\`, or any other miss preserves the existing behavior: an interactive unknown
verb opens the TUI, while a headless invocation reaches Cobra's unknown-command
error. Current-directory execution is refused even when `PATH` contains `.` or
Go's `execerrdot` compatibility switch is disabled.

forgectl performs **no authenticity, ownership, permission, blessing, sandbox,
capability, discovery, or registration check** on an external command. A match
is arbitrary code run with the invoking user's authority; `PATH` contents are
the user's trust boundary.

### env — safe .env management

`forgectl env` touches `.env` files without ever putting a secret value in argv, terminal output, or a session transcript: key names are always visible, values never print. It's built for agent-driven workflows — an agent can be trusted with the tool even though it can't be trusted to keep a value out of its own transcript, because the tool structurally never hands one back.

**`env check`'s exit codes are part of its contract, not incidental:** exit `1` means the file and its example both exist but disagree — missing and/or extra keys (drift); exit `2` means either the env file or the `--example` file is absent, so no comparison could run at all. `env check --json` emits the drift as a single object on stdout, `{"missing":[...],"extra":[...]}`, for scripted callers.

**Blessed value producers** for `env set`, non-inline patterns first:

```sh
op read op://vault/item/field | forgectl env set API_KEY   # 1Password by composition
forgectl env set API_KEY < value.txt                       # from a file
forgectl env set API_KEY --clipboard                       # from the clipboard
forgectl env set API_KEY                                   # interactive, no echo
```

**Never inline the secret in the producing command itself** — `printf 'secret' | forgectl env set KEY` puts the value in *that command's own* argv and shell history/transcript. forgectl can't close a channel it doesn't own; the pipe's left-hand side is your responsibility, not `env set`'s.

**Residual risk — read before relying on `--clipboard`:**

- Clipboard contents are readable by every local process, and clipboard managers (Raycast, Maccy, Alfred, Paste) persist history to disk by default — a `get --clipboard`'d secret can outlive the command that copied it. Clear it: paste over the clipboard with something innocuous, or purge the specific entry from your clipboard manager's history (each has its own delete/clear-history command).
- **Accepted, not fixed, in v1:** a hardlink read (`ln /outside/secret ./x.env`) can read a file outside the intended tree — but creating the hardlink already implies filesystem access, so this adds nothing an attacker with that access didn't already have; the *write* path is neutralized (`writeAtomic` renames a fresh inode, so a pre-existing hardlink to the target never receives the new content). A TOCTOU window exists between `Locate` and the write — accepted for a local, single-operator CLI; openat-style hardening is overkill here.
- **Agent-write threat model, one line:** running `env set`/`env get` under an agent grants that agent write authority over repo-contained **env files** for the duration of the session — containment (refuses outside the git repo), the env-file-name rule (below), 0600 permissions, and atomic writes bound the blast radius, but they don't remove the authority itself. The two subcommands grant distinct authorities: `env set` is **write** authority (the agent can create or overwrite a key in the file); `env get --clipboard` is **read/exfil** authority (the agent can copy an existing secret to the clipboard, where — see the residual-risk note above — any local process or clipboard manager can then read it too). Granting one does not imply granting the other.

**Safety notes:**

- Values never appear in argv, stdout, or log output — every value-bearing operation lives inside the domain package, not the CLI layer.
- Every write lands at `0600`; a looser pre-existing mode is tightened and reported (`tightened <file> to 0600`) rather than silently left alone.
- `--file` is refused unless it resolves inside the current git repository (walk-up `.git` detection, symlink-escape checked) — no editing a `.env` outside the repo you're working in.
- **`--file` must also name an env file** — `.env`, `.env.*` (`.env.local`, `.env.prod`, `.env.staging`, `.env.example`), or `*.env`. Repo-containment alone is not a bound worth having: `.git/config` is inside the repo, and `KEY=value` is valid git-config syntax, so an unconstrained `--file` turns `env set` into `core.sshCommand` — arbitrary code execution on the next `git fetch`. `.envrc` (direnv executes it) and `Makefile` (`KEY=value` is valid make) are the same shape. A blocklist would be whack-a-mole against every future execute-on-read format, so the allowlist is the bound. The point of this tool is to be the thing you hand an agent *instead of* raw shell; it must not be a shell in a trench coat.
- **`--any-file` overrides that rule, and only a human can use it.** It requires an interactive confirmation on a real terminal; with no TTY — every agent, every CI job, every piped invocation — it refuses outright. A flag an agent can type is not a bound on an agent; the TTY gate is the bound, and the flag is just how a human reaches it.
- `--clipboard` is macOS-only (shells out to `pbcopy`/`pbpaste`); it errors clearly on other platforms rather than silently no-op'ing.
- Secret **lengths** stay out of the logs too: `env` builds its clipboard client with `clip.WithSensitive()`, which drops the byte-count the clipboard layer otherwise logs at `info`. A length is signal — it distinguishes key types and tracks rotations — which is the same reason `redact` masks to a fixed `****` rather than revealing length.

### resume — getting back in after a terminal restart

A terminal restart costs three steps otherwise: find the folder, run `claude --resume`, then recognize the session in a picker that shows neither repo nor branch. `forgectl resume` collapses that to one command from a cold terminal — it lists recent sessions across *every* repo with name, repo, branch, and last activity, and lands you back inside the one you pick, in the right directory, with its task list restored.

Like `launch`, it **execs `claude` in place** (via `syscall.Exec`) and never returns; the resumed session is interactive, so there is no `-p`/`--print` form. From a script or an agent tool call, reach for `resume --dry-run` (prints the resolved cwd and argv, execs nothing) or `resume ls --json`. `resume ls` is the only member of the group that returns.

Against `launch`: `launch` starts or resumes a session in the *current* directory. `resume` is the cross-repo one — it finds a session anywhere on the machine and moves you to it.

**Claude Code stays the source of truth for identity.** Its prompt history and per-project transcripts already carry the session id, cwd, branch, and title indefinitely — so there is no daemon and no poller here, just a reader over artifacts that already exist. Two things genuinely do not survive a session's exit, and `resume snapshot` is what captures them:

- the `/rename` name, which lives only in the live-process registry;
- the task bodies, which Claude Code deletes when a session ends.

The snapshot record it writes does also carry recovery metadata — session id, cwd, version, timestamps, and the task-directory association — but that is a *cache with a known authority above it*, not a second source of truth: everything except the name, the tasks, and the task-directory pairing is re-derivable from Claude Code's own files.

Wire the capture to a `Stop` hook so every turn refreshes it. It is cheap, idempotent, and **always exits 0** — a failed snapshot must never become a failed turn.

**Merge this into the `hooks` object of `~/.claude/settings.json`** — user scope, since the whole point is cross-repo. **Do not replace that file**; it holds unrelated settings, and the fragment below is a fragment, not a document:

```json
"Stop": [
  { "hooks": [{ "type": "command", "command": "forgectl resume snapshot --quiet" }] }
]
```

`forgectl` must be on the hook's `PATH` — hooks do not inherit an interactive shell's environment, so use an absolute path if yours is not in the system `PATH`.

Because `snapshot` always exits 0, a hook that is missing, misspelled, or wired into the wrong settings file looks exactly like one that works — until a session exits and its tasks are gone. `forgectl doctor`'s `resume tasks` check is the detector: it warns when live sessions exist and the snapshot store is empty.

Snapshots live one JSON file per session in forgectl's config directory — `~/Library/Application Support/forgectl/resume-sessions/` on macOS, `~/.config/forgectl/resume-sessions/` on Linux — alongside every other forgectl store. The store self-prunes at most once a day, retiring a record when Claude Code's transcript for that session is gone — transcript existence, not a fixed age, because a snapshot is worth keeping exactly as long as `claude --resume` can still open the session, and transcript retention is operator-configurable. A record whose transcript survives is kept however old it is; the 180-day ceiling applies only to records whose transcript is already gone, so that a machine whose transcript lookup is broken still retires dead records eventually. A pass that would retire most of the store refuses and reports why, since that is the signature of a broken lookup rather than of that many dead sessions — and `FORGECTL_RESUME_NO_PRUNE` (any non-empty value) disables pruning entirely.

**Notes:**

- **An ambiguous filter lists the candidates instead of prompting** — with **exit 1**, the same code as "no session matched", because both recover the same way: change the filter. When more than one session matches and nobody can answer the picker — `--dry-run`, or any run without a terminal — `resume` prints one candidate row per line on stdout and exits 1 rather than opening a selector no one can see. That list is also the answer to "is this filter unique?", which a caller otherwise has no way to know before running the command: candidates on stdout means ambiguous, empty stdout means no match, exit 0 means it resumed.
- **A running session is refused, not continued** — with **exit 2**, distinct from the exit 1 used for "no session matched" or an ambiguous filter, because this is the one refusal a caller can act on. Two Claude Code processes on one transcript corrupts it, so `resume` errors out and names the live pid. `--fork` is the way in anyway: it branches a new session off the transcript, which only reads it. Pre-check with `resume ls --json`, which exposes `live` and `pid`.
- **`--fork` always forks, including on a session that is *not* running.** It is not a no-op safety flag: a fork starts a *new* session rather than continuing the old one, and a new session reads its own empty task list, so snapshotted tasks are reported rather than restored (its task directory is named after a session id that does not exist until after the exec). Pass it in response to the live-session error, not defensively.
- **The task store never shrinks to follow Claude Code.** Snapshots merge by task id and retain what they have already captured, so a later pass seeing fewer tasks never discards the earlier ones — dropping to the live set would throw away exactly what the feature exists to rescue. This is a property of *repeated* snapshots taken while the session is alive: `snapshot` walks the live-process registry, so it cannot discover a session that has already exited. Without a prior snapshot, running it after a crash recovers nothing — which is why the `Stop` hook, not manual invocation, is the intended wiring.
- **Restore never overwrites.** A task file the live session owns always wins, and `.highwatermark` is raised but never lowered, so a resumed session is never handed an id already on disk. Running it repeatedly is a no-op.
- **The resumed session gets its own project's posture.** `[launch]` profile resolution is a pure function of the config and a directory, so resuming into another repo picks up that repo's model, effort, permission mode, and `--add-dir` set for free.
- `forgectl doctor` carries a `resume tasks` check. Task rescue depends on Claude Code naming per-session task directories after the session id — verified behavior, not a guarantee — and the check warns if that ever stops being true, so rescue cannot silently degrade to writing where nothing reads.

## How it fits together

```
forgectl tmux pick
    └── delegates session selection to sesh
            └── hands off to tmux

forgectl projects list / pick
    ├── local clone walk (git remote get-url)
    ├── gh repo list (github.com, per owner) ─┐ concurrent
    └── tea repo ls  (git.sjo.lol/cameron)   ─┘

forgectl workflow run <name>
    └── parse a TOML step list → resolve params → plan (--dry-run stops here)
            └── execute: each step drives an existing seam (git, launch, tmux)

forgectl pr prs / dash / pick
    ├── gh search prs (authored / assigned / review-requested) ─┐ concurrent
    │   dimmed against a local reviewed-state store ────────────┘
    └── pick → PrepareMany (same-repo checkouts serialized) → clean-room launch
```

`sesh` handles the smarts — path discovery, named sessions, zoxide integration. `forgectl` provides the stable verbs and the thumb-friendly TUI on top.

**Session names are matched exactly.** Every command that names a session — `tmux kill`, `tmux rename`, `projects open`, the TUI's actions — compares your argument to the session list with plain string equality, then acts on the session's native tmux id. tmux's own `-t` resolution does not run, so an abbreviation no longer finds a session: with only `forge-review` running, `forgectl tmux kill forge` reports `no such session: forge` instead of killing `forge-review`. Names containing spaces, punctuation, `*`, or a leading `=` work fine, because none of them are interpreted. If you relied on abbreviating, type the full name (`forgectl tmux ls` lists them). The one exception is `tmux pick`, which hands the name to `sesh connect` — matching there is sesh's smart naming, by design, and is unaffected by this.

`projects` builds a unified inventory across local clones, GitHub, and the self-hosted Gitea. A project that isn't checked out locally shows as `[uncloned]`; picking it clones from the right host before opening the tmux session. `list --json` emits structured records to stdout — degradation notes (e.g. a host that's unreachable) go to stderr so the pipe stays clean.

`workflow` is the composition layer — a declarative TOML step list forgectl parses, plans, and executes through the same seams the hand-run verbs use. `--dry-run` prints the fully resolved plan without running a step. `--resume` picks a failed run back up from its first incomplete step, skipping the checkpointed steps whose inputs haven't changed, and `workflow status <name>` shows that checkpoint state; a resume re-verifies the blessing and refuses to replay across an edited definition. It also refuses when a step still to run needs an export that only a skipped step produced — a step's outputs aren't reconstructed from the sidecar, so those workflows must be run fresh. User workflows live in `workflows/` under the config dir (paths below), overriding shipped built-ins of the same name.

A user workflow must be **blessed** before `workflow run` will execute it. `forgectl workflow bless <name>` signs the file's exact bytes behind a Touch ID (or account-password) presence ceremony, writing a `*.blessing` sidecar next to it; one changed byte invalidates the signature, so re-bless after every edit — that is the point. Built-in workflows are compiled into the binary and never need blessing.

The ceremony holds its key in the Secure Enclave, so **blessing is macOS-only**. The `forgectl-bless-helper` binary that performs it ships alongside `forgectl` in the Homebrew cask, and forgectl finds it as a sibling of its own executable. Linux builds still *verify* blessings — that path is pure Go — but cannot create them.

`quarantine` and the clean-room workflow share one default carrier list. The
standalone command reversibly renames matches; clean-room `strip` permanently
deletes the same matches only inside its throwaway sandbox. Coverage is
intentional, not universal-editor protection: in addition to the Claude/Codex
and MCP execution carriers used by forgectl's review harnesses, the defaults
hide Cursor's `.cursor/` and `.cursorrules` plus Copilot's
`.github/instructions/`. Other editor-specific configuration remains visible
until a concrete forgectl workflow justifies the extra deletion and
fail-on-existing-`.quarantined` surface. The vendor-neutral MCP pattern remains
separate, so a matching `.vscode/mcp.json` is still covered as executable MCP
configuration even though VS Code settings are not quarantined wholesale.

## Configuration

Optional. forgectl runs with sensible defaults and no config file. To persist preferences, drop a TOML file at `config.toml` in your OS config dir:

- macOS: `~/Library/Application Support/forgectl/config.toml`
- Linux: `~/.config/forgectl/config.toml`

User workflow files share the same base: `<config dir>/workflows/<name>.workflow.toml`.

```toml
no_icons  = false   # use ASCII markers instead of Nerd Font glyphs
log_level = "off"   # off | debug | info | warn | error
log_file  = ""      # "" = auto (daily-rotated file); "-" = stderr; or an explicit path
```

`forgectl config` (alias `cfg`) prints **every** section of the config — the host scalars, `[launch]` and its `[launch.defaults]`, and every domain section down to the nested `[review.gitea]` — one dotted key per line, each marked `(set)` when the config file supplied it or `(default)` when it came from a built-in fallback.

That marker is the point. Every section's zero value means "absent, built-in defaults apply", so a value alone cannot tell a key you never set from a key you misspelled. The command also lists **unrecognized keys** — anything in the file that bound to no field, which catches `probe_hostt` and a key filed under the wrong section — and surfaces the decode error from a malformed file instead of silently defaulting past it. Resolved values that differ from the stored ones (the `off` log level, the dated log path, the launch binary chosen by `FORGECTL_CLAUDE_BIN` > `binary_path` > `PATH`) print in their own labeled blocks.

`sessions.dsn` renders as `(redacted)`; its `(set)`/`(default)` marker is the useful signal and a connection string can carry a password. `launch.defaults.env` renders its **key names only** for the same reason — it is arbitrary environment injected into the launched harness, so it is where an `ANTHROPIC_API_KEY` or `GH_TOKEN` would sit. `forgectl launch which` applies the same policy to the same map, so no forgectl command prints those values — to read one, open the config file itself with `forgectl launch edit`.

`--json` emits the same information machine-readably and is the stable surface — the human rendering may reflow.

### projects and review — whose repos get enumerated

Both inventories are scoped to GitHub accounts you name, and both default to the
account `gh` is already authenticated as:

```toml
[projects]
# owners = ["your-login"] # gh repo list scope; unset or [] = authenticated GitHub.com login
[review]
# owners = ["your-login"] # gh search --owner scope; unset or [] = authenticated GitHub.com login
```

The two lists are **independent**. Neither inherits from the other — which repos
you jump between and which repos you triage work in are different questions, and
a shared list would force one answer on both. Set one, the other, both, or
neither.

Leaving a list unset (or writing `owners = []`) resolves the authenticated login
once per run, so the same binary follows whichever account you are logged in as
on that machine. A configured list is authoritative and makes no discovery call
at all. Owner values are validated against GitHub's owner charset, capped in
count and length, and deduplicated case-insensitively before any of them becomes
a `gh` argument; a malformed entry anywhere in the list means zero queries rather
than a quietly narrowed inventory.

**Every GitHub call is pinned to `github.com`**, whatever `GH_HOST` says in the
surrounding shell. GitHub Enterprise is not supported: an ambient enterprise host
is overridden rather than queried, because listing an enterprise instance's repos
and labeling them `github` would put wrong data in the inventory. The Gitea
source is unchanged and keeps its existing `git.sjo.lol` assumptions — forgectl
does not claim arbitrary-Gitea portability either.

If `forgectl init` already wrote an active `owners = ["…"]` into your
config.toml, it stays exactly as written: init never rewrites a section that is
already present. Delete or comment the line to move to the authenticated default.

### Logging

Logging is **off by default**. Set `log_level` to `debug` for the full narrative (every tmux/sesh subprocess, with timing) or `info` for just the success/failure story. Logs follow an action-oriented pattern — `Preparing to…` / `Successfully…` / `Failed to…` — so they read top-to-bottom when something goes sideways.

With `log_file = ""` (the default target once a level is set), forgectl writes to a daily file — `forgectl-YYYY-MM-DD.log` — in the config dir and prunes any such file older than 7 days on startup. Set `log_file = "-"` to log to stderr instead, or give an explicit path to opt out of rotation.

### launch — per-project Claude Code, Codex, and Pi profiles

`forgectl launch` resolves a posture from the `[launch]` section of the same
`config.toml`, then **execs** Claude Code, Codex CLI, or Pi in place (via
`syscall.Exec`, so Ctrl-C, the TTY, and the exit code pass through untouched).
There is no prompt — a bare `forgectl launch` drops straight into the resolved
profile. To resume or fork an earlier session, use `forgectl resume`, which
discovers sessions across every repo, flags the live ones, and restores their
tasks. Claude remains the compatibility default.

```toml
[launch.defaults]
harness         = "claude"   # or "codex" / "pi"
model           = "opus"     # remove or replace for Codex/Pi
# effort        = "medium"   # low|medium|high|xhigh|max; unset = derived from model
permission_mode = "plan"     # Claude starts in plan
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this
# Codex settings when harness = "codex":
# approval_policy   = "on-request"
# sandbox           = "read-only"   # launch always starts non-writing; opt up to "workspace-write"
# codex_binary_path = ""      # $FORGECTL_CODEX_BIN overrides this
# Pi settings when harness = "pi":
# provider       = "lm-studio" # optional; unset = Pi's configured/default provider
# pi_binary_path = ""          # $FORGECTL_PI_BIN overrides this

[[launch.project]]
match           = "~/Projects/minute"
model           = "sonnet"
effort          = "xhigh"    # omit to take sonnet's derived "high"
env             = { OTEL_EXPORTER = "otlp" }
add_dir         = ["~/Projects/minute/shared"]
```

Resolution expands `~`, picks the `[[launch.project]]` whose `match` is the **longest path-prefix** of the real working directory, and merges it over `[launch.defaults]` — scalars: project wins when set; `env`: merged, project wins on collisions; `add_dir`: concatenated and de-duplicated. No match falls back to defaults alone. Inspect the result with `forgectl launch which`.

**Effort is the exception to "scalars merge like the others."** It resolves *last*, against the **final** model:

| Layer | Wins when |
|---|---|
| `effort` on the matching `[[launch.project]]` | set — beats everything below |
| `effort` in `[launch.defaults]` | set, and the project didn't set one |
| derived from the resolved model | neither layer set one |
| nothing — no `--effort` flag at all | the model is unmapped |

The derivation is `sonnet` → `high`, `opus`/`fable` → `medium`, with a `[1m]` context-window suffix (`opus[1m]`) treated as its base model. Anything else — `haiku`, `opusplan`, a full model id like `claude-opus-5` — emits **no flag**, leaving your `settings.json` `effortLevel` in charge. `opusplan` is excluded deliberately: it runs opus for planning and sonnet for execution, and those map to different levels.

Because derivation runs last, a project block overriding only `model` **re-derives** its level rather than inheriting one picked for a different model — unless `[launch.defaults] effort` is set, which is a deliberate global floor and survives the override.

A command-line `--model` does *not* re-derive: `forgectl launch --model sonnet` keeps the profile's effort, and last-flag-wins gives you sonnet at that level. Switch models mid-session with `/model` if you want the matching effort.

An `effort` outside the five accepted levels is rejected before anything is launched — by `forgectl launch`, `forgectl launch doctor`, and the `forgectl pr` review dispatch. That last one is why the check exists: a review runs in a *detached* tmux window, and `tmux new-window` returns 0 the instant the window exists — it never observes the child. When the agent then rejects a value and exits, tmux **destroys the window** (`remain-on-exit` is off by default), taking the error message with it. Nothing is left to read. `forgectl pr list` reports each session's window liveness for exactly this reason — see the `[pr]` section below.

**Design invariants** (verified against `claude` v2.1.183):

- **Injected posture first, user args last** — a user-supplied flag (e.g. `--model`) overrides the profile because Claude Code is last-flag-wins.
- **`agents` is Claude-only** — Codex and Pi profiles reject the passthrough and
  point out that no adapter ships. Claude retains its agents-valid injection
  and byte-clean `--json`/`--help` passthrough.
- **Claude and Codex start in postures that cannot write** — `permission_mode =
  "plan"` for Claude, `sandbox = "read-only"` for Codex. Both are opt-ups:
  `allow_danger` makes bypass reachable, `sandbox = "workspace-write"` makes
  the checkout writable. Neither is on by default.
- **Pi uses Pi's native tool posture** — forgectl injects only configured
  `provider`/`model` selectors and the resolved profile environment. Configure
  Cadence bridge variables such as `CADENCE_BRIEFS_DIR`,
  `CADENCE_METRICS_DIR`, and `GIT_GUARDRAILS_ALLOWED_OWNERS` in `env`; forgectl
  passes their values to Pi but shows only their names in `launch which`.

**Choosing the binary** uses env → config → PATH:
`FORGECTL_CLAUDE_BIN` / `binary_path` / `claude`, or
`FORGECTL_CODEX_BIN` / `codex_binary_path` / `codex`, or
`FORGECTL_PI_BIN` / `pi_binary_path` / `pi`.

Codex modes translate to `codex`, `codex resume --last`, `codex fork --last`,
and `codex exec`. Clean-room reviews accept `--agent codex` only for
`forgectl pr local --operator-authored` — code you state that you wrote.

Pi launches as `pi [--provider <name>] [--model <pattern>]`, with operator
arguments appended last so an explicit command-line selector wins. Surface
placement stays separate: use `forgectl surface launch <target> --surface
tmux|cmux|herdr`; forgectl does not add Pi-specific pane management or start,
probe, or supervise a local model server.

> **`--agent codex` requires you to assert authorship, by design.** The Claude
> clean room confines the reviewer to a deny-by-default allowlist with no
> command-execution primitive. Codex's `--sandbox read-only` scopes writes and
> network egress but not *which commands run*, so a prompt injection in a diff
> you did not write could reach a shell with read access to your whole home
> directory — and anything read reaches the model provider as tool output.
> `codex exec` exposes no way to confine that: it accepts no
> `--permission-profile`, and `shell_environment_policy.inherit=none` is
> honored by `codex sandbox` but ignored by `codex exec` (measured on
> codex-cli 0.146.0).
>
> So the boundary is drawn by **authorship**, not by sandbox and not by
> locality. A local path proves only that the *directory* is yours — `gh pr
> checkout` puts someone else's commit in your own repository, which is the
> ordinary way to review a PR. So `forgectl pr local --agent codex` refuses
> unless you pass `--operator-authored`, and `forgectl pr <ref>` refuses
> outright with no flag to override, since a fetched PR head is someone else's
> content by construction. Use `--agent claude` (the default) for anything you
> did not write.
>
> Refusal is enforced in the dispatch path before anything is fetched or
> created, and again at launch — the second check is what covers a session
> restored from a breadcrumb. Provenance is never inferred from a signal:
> not from HEAD being attached or detached, not from path or repository
> ownership, not from the Git author or a commit signature, and not from your
> profile, UID, or terminal.

Provenance is recorded in the session breadcrumb, which has one **downgrade
limitation** worth knowing: the breadcrumb decoder rejects unknown fields, so an
*older* forgectl refuses a breadcrumb written by a newer one. Upgrading is
seamless in the forward direction — legacy breadcrumbs read as unknown
provenance, so Claude, `pr list`, `pr attach`, and `pr teardown` keep working
and only Codex needs a fresh `--operator-authored` run. If you must roll back,
use the newer binary or recreate the session state; the field is never stripped
automatically, because doing so would silently change what the record asserts.

The decoder also requires a breadcrumb file to hold exactly one JSON record:
anything but whitespace after it is rejected, since forgectl writes one record
per file and trailing content means something else has written there. This
changes nothing for a file forgectl wrote — its own terminating newline is
whitespace — so there is no migration.

**Legacy `claunch.conf` migration** — on Darwin and Linux, a launch command captures the legacy file once, decodes that exact byte slice, and uses those same bytes for a no-clobber backup before retiring the named source. An existing `claunch.conf.bak` is never overwritten; forgectl allocates an exclusive `claunch.conf.bak.<random>` name instead. A hardlinked source is allowed, but retirement removes only the `claunch.conf` directory entry and leaves sibling links unchanged. `launch migrate` is the explicit import-only form: it refuses an existing `[launch]` table and does not back up or retire the source.

The path policy is lexical and fail-closed. An explicit `XDG_CONFIG_HOME` must be absolute, and both `<xdg>/forgectl/config.toml` and `<xdg>/claunch/claunch.conf` must be the exact cleaned children of that same root—prefix lookalikes and cross-root Darwin pairs are refused. With no explicit XDG value, Darwin keeps the historical native-config/`~/.config/claunch` pair. Symlinks, directories, FIFOs, sockets, devices, malformed TOML, unstable reads, and identity drift are never migration sources. A legacy leaf symlink may still be followed only by the nonblocking read-only compatibility fallback when it resolves to a regular file.

On an automatic refusal, a missing native `[launch]` uses a safely decoded read-only legacy profile for that invocation; a present `[launch]`—even an empty table—remains authoritative. `FORGECTL_SKIP_LEGACY_MIGRATE=1` always selects that non-mutating behavior. No pre-capture refusal creates the config directory, writer lock, config temp, config file, or backup.

Config replacement, backup creation, and source retirement are separate reported phases. The config temp remains pinned by an attempt-owned descriptor: its name is identity-checked immediately before rename, and the renamed identity, exact bytes, and private mode are checked again before directory durability can be claimed. Cleanup immediately identity-validates the name and live descriptor before removal, preserving any replacement already visible at that check. The backup allocation likewise retains an attempt-owned identity descriptor through cleanup, so immediate inode reuse cannot make a replacement appear owned. The config file and its directory entry must be durable in the current attempt before backup begins; an added-zero retry reopens the current bytes, syncs the file, and syncs its parent before retirement can resume. If a config rename is visible but its identity/content/mode proof or directory sync fails, the source is retained. Any later backup or identity failure likewise retains the source; a failure syncing the legacy directory after unlink is reported as durability-unknown and keeps the verified backup.

A captured legacy name disappearing while a process waits for the writer lock is a successful peer no-op only when the locked `[launch]` config already supersedes every captured legacy addition. Otherwise the disappearance is reported as drift/refusal, the source is not claimed as cooperatively retired, and fallback uses the captured profile when no native `[launch]` is authoritative.

New config and backup files are owner-only and no broader than `0600`; a restrictive umask may narrow them further, and an existing config mode such as `0400`, `0200`, or `0000` is not broadened. Every cooperating launch/top-level init writer shares the same Unix sibling lock and atomic replacement path. Secure legacy mutation and directory-durability claims apply to Unix builds (the shipped Darwin/Linux targets); non-Unix builds refuse automatic and explicit legacy mutation before writer activity. Their developer-only normal init may make a replacement visible, but reports that directory durability and cross-process serialization are unavailable.

> Absorbed from the standalone `claunch` tool. A `claunch='forgectl launch'` shell alias preserves the old muscle memory.

### Local launch statistics

`forgectl` can count the harness launches it attempts, locally and only when you
ask it to. Collection is off unless `config.toml` says otherwise:

```toml
[launch]
usage_stats = true
```

Nothing but that key turns it on — no upgrade, migration, or environment
variable does, and a `usage_stats` in a legacy `claunch.conf` is ignored.

When on, one line is appended to
`${XDG_STATE_HOME:-~/.local/state}/forgectl/launch-usage.jsonl` immediately
before each harness exec forgectl attempts:

```json
{"schema_version":1,"ts":"2026-08-13T19:42:17Z","event":"exec_attempt","harness":"claude","model":"opus","session_mode":"new","posture":"default"}
```

Those seven fields are the whole record. There is no directory, project,
repository, branch, session id, harness argument, prompt, environment variable,
task, host, user, or process id in it — and no hash of any of them. They are
still sensitive: exact timestamps describe when you work, a model label can name
an internal deployment, and the session and posture counts describe how you
work. Aggregating locally does not change that.

Nothing is uploaded. There is no network call, no device identifier, and no
import of the retired claunch wrapper's own log.

Read it back with `forgectl launch stats [days] [--json]`; `--json` emits an
aggregate only, while the JSONL file itself is your raw export. `forgectl launch
doctor` reports whether collection is on, where the file lives, and any
permission it had to tighten.

Rows are kept until you delete them. Setting `usage_stats = false` stops new
rows but neither hides nor removes old ones. Delete them yourself, while no
`forgectl launch`, `resume`, or `stats` is running:

```sh
rm -- "${XDG_STATE_HOME:-$HOME/.local/state}/forgectl/launch-usage.jsonl"
rm -- "${XDG_STATE_HOME:-$HOME/.local/state}/forgectl/launch-usage.jsonl.lock"
```

Deletion is permanent.

An `exec_attempt` means forgectl finished validating a profile and called
`exec`. It does not mean the harness started or that the session was useful:
nothing after `syscall.Exec` is observable from a process that replaced itself.

### pr — the clean-room reviewer's own posture

The `[pr]` section configures `forgectl pr` independently of whatever repo a review happens to land in:

```toml
[pr]
max_concurrent = 4         # live "pr-*" tmux windows allowed at once; <= 0 = default (4)
model  = "opus"            # reviewer model; unset = the ambient launch profile's
effort = "xhigh"           # reviewer effort; unset = derived from the reviewer model
```

`model` and `effort` exist because a review's posture should follow *the review*, not the checkout it is pointed at. Setting `model` **discards** the ambient repo's effort and re-derives from the new model — otherwise reviewing a sonnet-pinned repo with `[pr] model = "opus"` would ship `--model opus --effort high`, the exact mispairing the knob exists to prevent. Setting `effort` overrides everything.

**These two keys are the whole surface, by design.** The clean-room review always runs with `--permission-mode plan`, never `--allow-dangerously-skip-permissions`, always `--strict-mcp-config`, and with your ambient `add_dir` dropped — because the workspace under review may hold a third party's checkout. That posture is forgectl's control, not an operator preference, and no `[pr]` key can reach it. A value starting with `-` is refused before dispatch, since both land next to those flags in the argv.

**`model` is deliberately not checked against a list of known models.** `effort` is an enum of five levels, so a typo there is caught before dispatch; `model` is not, because the value space is effectively open — the aliases (`opus`, `sonnet`, `fable`), their undocumented context-window variants (`opus[1m]`), and arbitrary full ids (`claude-opus-5`) all have to keep working. An allowlist would buy little and would break legitimate pins the day a new model ships.

A mistyped `model` therefore reaches the agent, which rejects it a few seconds after dispatch and exits — and tmux destroys the review window along with the error. `forgectl pr list` is where that surfaces: it cross-checks each breadcrumb against the live tmux window list and renders a status column. The output is tab-separated — aligned below for readability.

```
cameronsjo/forgectl#42   2026-08-04T09:12:31Z  …/forgectl/pr-sessions/cameronsjo-forgectl-42-….json   live
cameronsjo/forgectl#41   2026-08-04T09:10:02Z  …/forgectl/pr-sessions/cameronsjo-forgectl-41-….json   window gone
cameronsjo/forgectl#39   2026-08-03T16:41:55Z  …/forgectl/pr-sessions/cameronsjo-forgectl-39-….json   workspace missing
```

The status is the last field, appended rather than inserted, so the breadcrumb stays field 3 for anything already parsing this output.

A breadcrumb's filename is the one field here that is read off disk rather than parsed, so a name carrying terminal control or bidi characters is escaped as a Go-quoted literal instead of being printed raw. The escaping is conditional: an ordinary path prints verbatim, so field 3 remains exactly the argument `pr teardown` takes. `pr dash` quotes the path unconditionally — it is a human view, not a parsing target.

`live` means the review window still exists. `window gone` means the agent is no longer running — a rejected `model` is one cause, but so is a finished review or a window you closed yourself; either way the session is stale and `pr teardown <breadcrumb>` reclaims it. `?` means tmux itself could not be read, which says nothing about any individual window; the command still succeeds.

`workspace missing` means the clean-room directory itself is gone — usually because you deleted it by hand, or the OS reclaimed its temp root. The breadcrumb outlived what it described. `pr teardown <breadcrumb>` removes the leftover record: on this branch it unlinks the breadcrumb and **nothing else** — no workspace removal, no quarantine restore, no tmux, no git — because there is nothing left to tear down. `pr cleanup <YYYY-MM-DD>` sweeps these up by date alongside live sessions.

These rows are also why a stale-only `pr list` makes no tmux calls at all: a missing workspace's window is not a question worth asking, and only live rows are batched into the liveness read.

Teardown refuses rather than guesses. It re-checks the breadcrumb's identity and exact contents, and re-confirms the workspace is still absent, immediately before unlinking; anything that changed underneath it — the file rewritten, replaced, or swapped for a symlink, the workspace reappearing — is a refusal that leaves the record in place. A record whose workspace exists but is not a forgectl sandbox is neither live nor cleanly missing, so it is left alone entirely: `pr list` skips it and teardown refuses it. That state means something unexpected wrote to the session-state dir, and deleting it on a guess would destroy the evidence.

**`pr list` is the after-the-fact view; the launch commands check the same thing at dispatch time.** Once every window is open, forgectl waits **eight seconds**, lists windows exactly once, and reports any review that has already vanished. Eight seconds is the observed window in which a rejected `model` gets rejected — long enough to catch it, short enough not to stall the command. It is a bounded observation, not a guarantee: an agent that dies at nine seconds still dispatches "successfully", and `pr list` remains the way to find it later.

Matching a dispatch back to a window has to survive two things that ordinary lookups do not — a second, older window carrying the same name, and a tmux server restart that reissues native window ids from `@0`. So forgectl captures the server PID, the server start time, and the native window id together, in the same `new-window` call that creates the window. Those three format fields are the reason for the floor: tmux documents all of them from **2.2** onward. `tmux -V` is checked before any review workspace exists, so an old, missing, or unreadable binary refuses up front:

```text
tmux 2.2 or newer is required to launch PR reviews with exact dispatch identity (found "tmux 2.1"); upgrade tmux and retry: unsupported tmux version
```

The stable sentence and the quoted version are the contract; the text after the final colon is the wrapped cause and varies with what failed. The quoted value is `"unknown"` only when `tmux -V` itself could not be run or parsed — a missing binary, or output in a shape forgectl refuses to guess at. A supported binary whose server state is unreadable still reports its real version there.

This capability check runs before any clean room or breadcrumb exists, so a refusal there leaves neither behind, and it creates no tmux server, session, or window — both probes (`tmux -V` and one `display-message -p`) only read. Nothing an earlier invocation left running is touched: a review dispatched from another terminal or another repository keeps its window, and `forgectl pr list` is how to see what is still live. What this does **not** cover is a failure after preparation — an invalid reviewer profile, a refused provenance, or tmux going away between this check and the launch: those deliberately leave the workspace and breadcrumb in place, because `pr attach` and `pr teardown` still need them. The one exception is tmux's own doing: a read-only probe against a machine with no server running can create the standard `tmux-<uid>` socket parent directory. That is a tmux artifact, not a review artifact.

When verification reports a review gone, recovery is the ordinary stale-session path — `forgectl pr list` to see which breadcrumbs lost their window, then `forgectl pr teardown <breadcrumb>` on each. When tmux could not be read at all, the command says exactly that instead of naming any review gone; an unreadable server is not evidence of a dead window.

`--no-verify` skips the eight-second wait and the window check, and nothing else. It is not an escape from the tmux floor, the capability check, the concurrency cap, or identity capture — it only trades post-dispatch confirmation for an immediate return.

### bench — interop with the local dev services

`forgectl bench` is the interop spine across the local bench: the **hearth** telemetry stack and the **chronicle** transcript-retention layer. It orchestrates each system through its own frozen contract — it never reimplements one. Configure it in the `[bench]` section of the same `config.toml`:

```toml
[bench]
hearth_dir    = "~/Projects/hearth"      # else $HEARTH_DIR; unset ⇒ hearth reports not-configured
chronicle_dir = "~/Projects/chronicle"   # else $CHRONICLE_DIR
otlp_endpoint = "http://localhost:16317" # hearth's frozen OTLP transport (baked default)
otlp_protocol = "grpc"                    # baked default
telemetry     = false                     # opt-in: inject OTLP env into `forgectl launch` sessions
```

- **`bench status`** probes each component — `docker compose -p hearth ps` plus HTTP/OTLP reachability, and `chronicle status --json` corroborated against `docker compose -p sessions ps` (chronicle's DB containers) plus the `local.chronicle-sync` LaunchAgent. Each resolves to `ok | degraded | unavailable | not-configured` with a human reason; a missing `docker`, an unloaded daemon, or an unconfigured dir is a graceful state, never an error, so `bench status` always exits 0. `--json` emits the report to stdout for scripting.
- **`telemetry = true`** injects the Claude-Code-tailored OpenTelemetry env block into launched sessions so their metrics and logs flow to the local collector. Opt-in: with it off, no session points at a collector. A profile `env` value wins over the injected default. `forgectl launch doctor` shows the current telemetry state.
- **`bench up`** brings the configured services up via their own entrypoints (hearth's `scripts/start.sh`, chronicle's `make sync`); an unconfigured service is skipped with a note. **`bench open`** opens a service UI in the browser (`open` on macOS, `xdg-open` elsewhere).

## License

[Apache-2.0](LICENSE) WITH [Commons Clause](https://commonsclause.com/) — source-available, not OSI open source.

Use it, modify it, fork it, share it — including commercially and in-house at your company. The one restriction: you may not **sell** the software, meaning you may not provide it to third parties for a fee or other consideration (including paid hosting or consulting/support offerings) where the value derives substantially from forgectl's own functionality.

Relicensing binds only future releases, not anything already published: `v0.1.0`–`v0.5.0` remain available under MIT, and `v0.6.0`, `v0.7.0`, and `v0.7.1` remain available under PolyForm Noncommercial 1.0.0. Apache-2.0 with Commons Clause applies from the next release onward.
