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
forgectl config            # show active config + resolved paths (alias: cfg)

# projects — cross-host project inventory (alias: proj)
forgectl projects list [query]           # list all projects: local clones + github.com/cameronsjo + git.sjo.lol/cameron
forgectl projects list --json            # machine-readable JSON (safe to pipe; degradation notes go to stderr)
forgectl projects list --host github     # filter to one host: github | gitea
forgectl projects list --host gitea forge  # host filter + name substring
forgectl projects pick [query]           # interactive picker across the full inventory; clones uncloned repos before opening (aliases: p, open)
forgectl projects                        # shorthand for pick (no args → TUI selector)

# pr — clean-room pull-request review (the flagship review family)
forgectl pr <ref>                        # prepare + launch an isolated, deny-by-default review (owner/repo#N, a PR URL, or a bare N)
forgectl pr <ref> --dry-run              # resolve + print the plan, create nothing
forgectl pr prs                          # cross-repo open PRs (authored, assigned, review-requested); reviewed rows dimmed
forgectl pr prs --json                   # machine-readable JSON (safe to pipe; notes go to stderr)
forgectl pr dash                         # dashboard: active reviews, PRs awaiting you, your open PRs
forgectl pr pick                         # multiselect open PRs → spin up reviews in bulk (reviewed PRs skipped)
forgectl pr reviewed mark <ref>          # mark a PR reviewed (dims it until the PR sees new activity)
forgectl pr reviewed unmark <ref>        # clear a PR's reviewed mark
forgectl pr reviewed sync                # prune reviewed marks for PRs that are no longer open
forgectl pr list                         # list active clean-room review sessions
forgectl pr attach <breadcrumb>          # jump to a review window (also: open <b>, teardown <b>)
                                          #   <breadcrumb> is the session path `pr list` prints
forgectl pr keys                         # tmux cheatsheet for driving a review

# launch — per-project Claude Code / Codex CLI launcher (alias: cl)
forgectl launch                    # interactive launcher: pick Model + New/Resume/Fork
forgectl launch <harness args…>    # apply the project profile, then exec the configured harness
forgectl launch agents --json      # pure passthrough (byte-clean); posture injected only when interactive
forgectl launch which              # show the profile resolved for the current directory (alias: config)
forgectl launch init               # scaffold the [launch] section into config.toml
forgectl launch init --from-claunch # import an existing ~/.config/claunch/claunch.conf into config.toml
forgectl launch edit               # open config.toml in $EDITOR
forgectl launch doctor             # check harness availability + launch config validity

# resume — get back into a Claude Code session after a terminal restart
forgectl resume                    # pick from recent sessions across every repo, then resume in place
forgectl resume forgectl           # filter by repo, name, cwd, or id; exactly one hit resumes it
forgectl resume --fork             # branch a new session off the transcript — the only way into a still-running one
forgectl resume --dry-run forge    # resolve and print the cwd + claude argv, exec nothing
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
#   hearth (telemetry stack), chronicle (transcript-retention layer), flux (task board)
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

# net — check cached internal-network reachability
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
#   the local bench (hearth/chronicle/flux), the trust store, forgectl's own currency
forgectl doctor                          # one line per check, with a remediation hint on any warn/fail
forgectl doctor --json                   # machine-readable report for scripting

# upgrade — update forgectl itself via the Homebrew tap (never `go build` over the brew-linked binary)
forgectl upgrade                         # brew update + brew upgrade --cask forgectl; brew owns the checksum + atomic install
forgectl upgrade --check                 # report whether an update is available, no mutation
                                          #   a source build (go build/go run) WARNS instead of attempting anything —
                                          #   there's no cask install to manage

# y — read/write the system clipboard (macOS only)
echo hi | forgectl y copy                # copy stdin to the clipboard
forgectl y paste                         # print the clipboard's current contents
```

The cask doesn't stage an `fx` command — it's a shell alias you add yourself:

```sh
alias fx=forgectl     # add to your shell rc

fx                    # same as bare forgectl — opens the TUI
fx tmux ls
```

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

- **A running session is refused, not continued** — with **exit 2**, distinct from the exit 1 used for "no session matched", because this is the one refusal a caller can act on. Two Claude Code processes on one transcript corrupts it, so `resume` errors out and names the live pid. `--fork` is the way in anyway: it branches a new session off the transcript, which only reads it. Pre-check with `resume ls --json`, which exposes `live` and `pid`.
- **`--fork` always forks, including on a session that is *not* running.** It is not a no-op safety flag: a fork starts a *new* session rather than continuing the old one, and a new session reads its own empty task list, so snapshotted tasks are reported rather than restored (its task directory is named after a session id that does not exist until after the exec). Pass it in response to the live-session error, not defensively.
- **The task store never shrinks to follow Claude Code.** Snapshots merge by task id and retain what they have already captured, so a later pass seeing fewer tasks never discards the earlier ones — dropping to the live set would throw away exactly what the feature exists to rescue. This is a property of *repeated* snapshots taken while the session is alive: `snapshot` walks the live-process registry, so it cannot discover a session that has already exited. Without a prior snapshot, running it after a crash recovers nothing — which is why the `Stop` hook, not manual invocation, is the intended wiring.
- **Restore never overwrites.** A task file the live session owns always wins, and `.highwatermark` is raised but never lowered, so a resumed session is never handed an id already on disk. Running it repeatedly is a no-op.
- **The resumed session gets its own project's posture.** `[launch]` profile resolution is a pure function of the config and a directory, so resuming into another repo picks up that repo's model, permission mode, and `--add-dir` set for free.
- `forgectl doctor` carries a `resume tasks` check. Task rescue depends on Claude Code naming per-session task directories after the session id — verified behavior, not a guarantee — and the check warns if that ever stops being true, so rescue cannot silently degrade to writing where nothing reads.

## How it fits together

```
forgectl tmux pick
    └── delegates session selection to sesh
            └── hands off to tmux

forgectl projects list / pick
    ├── local clone walk (git remote get-url)
    ├── gh repo list (github.com/cameronsjo) ─┐ concurrent
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

`projects` builds a unified inventory across local clones, GitHub, and the self-hosted Gitea. A project that isn't checked out locally shows as `[uncloned]`; picking it clones from the right host before opening the tmux session. `list --json` emits structured records to stdout — degradation notes (e.g. a host that's unreachable) go to stderr so the pipe stays clean.

`workflow` is the composition layer — a declarative TOML step list forgectl parses, plans, and executes through the same seams the hand-run verbs use. `--dry-run` prints the fully resolved plan without running a step. `--resume` picks a failed run back up from its first incomplete step, skipping the checkpointed steps whose inputs haven't changed, and `workflow status <name>` shows that checkpoint state; a resume re-verifies the blessing and refuses to replay across an edited definition. It also refuses when a step still to run needs an export that only a skipped step produced — a step's outputs aren't reconstructed from the sidecar, so those workflows must be run fresh. User workflows live in `workflows/` under the config dir (paths below), overriding shipped built-ins of the same name.

A user workflow must be **blessed** before `workflow run` will execute it. `forgectl workflow bless <name>` signs the file's exact bytes behind a Touch ID (or account-password) presence ceremony, writing a `*.blessing` sidecar next to it; one changed byte invalidates the signature, so re-bless after every edit — that is the point. Built-in workflows are compiled into the binary and never need blessing.

The ceremony holds its key in the Secure Enclave, so **blessing is macOS-only**. The `forgectl-bless-helper` binary that performs it ships alongside `forgectl` in the Homebrew cask, and forgectl finds it as a sibling of its own executable. Linux builds still *verify* blessings — that path is pure Go — but cannot create them.

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

`forgectl config` (alias `cfg`) prints the active settings and the resolved config and log paths — including whether the config file was found.

### Logging

Logging is **off by default**. Set `log_level` to `debug` for the full narrative (every tmux/sesh subprocess, with timing) or `info` for just the success/failure story. Logs follow an action-oriented pattern — `Preparing to…` / `Successfully…` / `Failed to…` — so they read top-to-bottom when something goes sideways.

With `log_file = ""` (the default target once a level is set), forgectl writes to a daily file — `forgectl-YYYY-MM-DD.log` — in the config dir and prunes any such file older than 7 days on startup. Set `log_file = "-"` to log to stderr instead, or give an explicit path to opt out of rotation.

### launch — per-project Claude Code and Codex profiles

`forgectl launch` resolves a posture from the `[launch]` section of the same
`config.toml`, runs a short guided launch, then **execs** Claude Code or Codex
CLI in place (via `syscall.Exec`, so Ctrl-C, the TTY, and the exit code pass
through untouched). Claude remains the compatibility default.

```toml
[launch.defaults]
harness         = "claude"   # or "codex"
model           = "opus"     # remove or replace with a Codex model when harness = "codex"
permission_mode = "plan"     # launch always starts in plan
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this
# Codex settings when harness = "codex":
# approval_policy   = "on-request"
# sandbox           = "read-only"   # launch always starts non-writing; opt up to "workspace-write"
# codex_binary_path = ""      # $FORGECTL_CODEX_BIN overrides this

[[launch.project]]
match           = "~/Projects/minute"
model           = "sonnet"
env             = { OTEL_EXPORTER = "otlp" }
add_dir         = ["~/Projects/minute/shared"]
```

Resolution expands `~`, picks the `[[launch.project]]` whose `match` is the **longest path-prefix** of the real working directory, and merges it over `[launch.defaults]` — scalars: project wins when set; `env`: merged, project wins on collisions; `add_dir`: concatenated and de-duplicated. No match falls back to defaults alone. Inspect the result with `forgectl launch which`.

**Design invariants** (verified against `claude` v2.1.183):

- **Injected posture first, user args last** — a user-supplied flag (e.g. `--model`) overrides the profile because Claude Code is last-flag-wins.
- **`agents` is Claude-only** — Codex profiles reject the passthrough and point
  out that no Codex adapter ships. Claude retains its agents-valid injection
  and byte-clean `--json`/`--help` passthrough.
- **Launch always starts in a posture that cannot write** — `permission_mode =
  "plan"` for Claude, `sandbox = "read-only"` for Codex. Both are opt-ups:
  `allow_danger` makes bypass reachable, `sandbox = "workspace-write"` makes
  the checkout writable. Neither is on by default.

**Choosing the binary** uses env → config → PATH:
`FORGECTL_CLAUDE_BIN` / `binary_path` / `claude`, or
`FORGECTL_CODEX_BIN` / `codex_binary_path` / `codex`.

Codex modes translate to `codex`, `codex resume --last`, `codex fork --last`,
and `codex exec`. Clean-room reviews accept `--agent codex` for **local review
only** — `forgectl pr local`, where the reviewed tree is your own.

> **`--agent codex` is refused for a remote PR head, by design.** The Claude
> clean room confines the reviewer to a deny-by-default allowlist with no
> command-execution primitive. Codex's `--sandbox read-only` scopes writes and
> network egress but not *which commands run*, so a prompt injection in a
> third-party diff could reach a shell with read access to your whole home
> directory — and anything read reaches the model provider as tool output.
> `codex exec` exposes no way to confine that: it accepts no
> `--permission-profile`, and `shell_environment_policy.inherit=none` is
> honored by `codex sandbox` but ignored by `codex exec` (measured on
> codex-cli 0.146.0).
>
> So the boundary is drawn by **use**, not by sandbox. A remote head is someone
> else's content and the Codex agent is refused there — enforced in the
> dispatch path, before anything is fetched. Your own working tree cannot be
> hostile to you, so `forgectl pr local --agent codex` stays fully available.
> Use `--agent claude` (the default) for PR review.

**Zero-migration grace** — if `config.toml` has no `[launch]` section, forgectl still reads a legacy `~/.config/claunch/claunch.conf` (the `[launch]` section is the same `[defaults]` + `[[project]]` shape, just namespaced). `forgectl launch init` writes an empty native section for the one-time cutover; `forgectl launch init --from-claunch` migrates your existing legacy profiles into it, so `launch` stops falling back to the legacy file. Both refuse to overwrite an existing `[launch]` section.

> Absorbed from the standalone `claunch` tool. A `claunch='forgectl launch'` shell alias preserves the old muscle memory.

### bench — interop with the local dev services

`forgectl bench` is the interop spine across the local bench: the **hearth** telemetry stack, the **chronicle** transcript-retention layer, and the **flux** board. It orchestrates each system through its own frozen contract — it never reimplements one. Configure it in the `[bench]` section of the same `config.toml`:

```toml
[bench]
hearth_dir    = "~/Projects/hearth"      # else $HEARTH_DIR; unset ⇒ hearth reports not-configured
chronicle_dir = "~/Projects/chronicle"   # else $CHRONICLE_DIR
otlp_endpoint = "http://localhost:16317" # hearth's frozen OTLP transport (baked default)
otlp_protocol = "grpc"                    # baked default
telemetry     = false                     # opt-in: inject OTLP env into `forgectl launch` sessions
```

- **`bench status`** probes each component — `docker compose -p hearth ps` plus HTTP/OTLP reachability, `chronicle status --json` corroborated against `docker compose -p sessions ps` (chronicle's DB containers) plus the `local.chronicle-sync` LaunchAgent, and `flux ready`. Each resolves to `ok | degraded | unavailable | not-configured` with a human reason; a missing `docker`, an unloaded daemon, or an unconfigured dir is a graceful state, never an error, so `bench status` always exits 0. `--json` emits the report to stdout for scripting.
- **`telemetry = true`** injects the Claude-Code-tailored OpenTelemetry env block into launched sessions so their metrics and logs flow to the local collector. Opt-in: with it off, no session points at a collector. A profile `env` value wins over the injected default. `forgectl launch doctor` shows the current telemetry state.
- **`bench up`** brings the configured services up via their own entrypoints (hearth's `scripts/start.sh`, chronicle's `make sync`); an unconfigured service is skipped with a note. **`bench open`** opens a service UI in the browser (`open` on macOS, `xdg-open` elsewhere).

## License

[Apache-2.0](LICENSE) WITH [Commons Clause](https://commonsclause.com/) — source-available, not OSI open source.

Use it, modify it, fork it, share it — including commercially and in-house at your company. The one restriction: you may not **sell** the software, meaning you may not provide it to third parties for a fee or other consideration (including paid hosting or consulting/support offerings) where the value derives substantially from forgectl's own functionality.

Relicensing binds only future releases, not anything already published: `v0.1.0`–`v0.5.0` remain available under MIT, and `v0.6.0`, `v0.7.0`, and `v0.7.1` remain available under PolyForm Noncommercial 1.0.0. Apache-2.0 with Commons Clause applies from the next release onward.
