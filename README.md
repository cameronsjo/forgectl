# forgectl

Personal dev-experience CLI for a headless macOS workbench driven over SSH — from laptops, phones, and Termius. What began as a tmux helper (superseding the ad-hoc bash `s` script; smart session-naming stays with `sesh`) has grown into the **workbench forge**: 28 composable command-group modules (see the table below) with a declarative workflow DSL as the composition layer.

Built for two hands and one thumb:

- **Power mode** — typed verbs (`forgectl tmux ls`, `forgectl tmux pick`). Full keyboard, full control.
- **Thumb mode** — bare `forgectl` opens a TUI menu. Number-key select. Narrow-screen. Forgiving input. Works fine in Termius over mosh.

## Install

```sh
brew install cameronsjo/tap/forgectl
```

Requires `sesh` on `$PATH` for `tmux pick`/`tmux ls` (session smarts — path discovery, named sessions, zoxide integration). Optional, per feature: `gh` (`pr`, `review`, `projects`), `tea` (`projects` against whichever Gitea instance `tea` is logged into, and `review` when `[review.gitea]` is enabled), `kubectl` (`k8s logs`), `docker` (`bench status`/`up`, `docker build/run/shell`, and `clean --docker`), `npm`/`pnpm`/`pip`/`go`/`brew` (`clean --caches` — each is independently opt-in and skipped, not required, when absent). `update`'s roster shares that same `brew`/`go`/`npm` dependency (`softwareupdate` is a macOS built-in) — each step is independently scoped, so a missing tool fails only its own step, never the others.

Commands that actually launch a PR review — `forgectl pr <ref>`, `forgectl pr local`, and `forgectl pr pick` once admission establishes there is at least one ref to prepare — require **tmux 2.2 or newer**, for the dispatch-identity reasons in [docs/commands/pr.md](docs/commands/pr.md). Read-only PR commands, any `--dry-run`, and empty, all-reviewed, or cap-full selections do not acquire that floor.

Reading a local clone's git state — `projects list`, `projects pick`, the project inventory, and `projects pull-all` — requires **Git 2.11.0 or newer**. That release introduced `git status --porcelain=v2 --branch`, which reports the working-tree state and the ahead/behind counts in a single command; forgectl reads both from that one call rather than spawning a second `rev-list` per clean repository. On older Git the command fails, the repository's status reads as unknown, and `pull-all` skips it rather than rebasing a tree whose state it could not establish.

## Command groups

28 command groups, at a glance. `forgectl --help` lists them from the binary
itself; this table is the scannable index — full verbs and flags for every
group are in the `## Usage` roster below, and the groups with a dedicated
deep-dive get a link here.

| Group | One-liner | Docs |
| --- | --- | --- |
| `tmux` | List/pick/kill/rename tmux sessions, delegating smart naming to `sesh` | Usage below |
| `config` | Show every config section, per-key set/default (alias: `cfg`) | Usage below |
| `init` | Scaffold every `config.toml` section with commented, sensibly-defaulted templates | Usage below |
| `projects` | Cross-host project inventory: local clones + GitHub + Gitea (alias: `proj`) | [projects and review](docs/commands/projects-and-review.md) |
| `pr` | Clean-room pull-request review, the flagship review family | [pr](docs/commands/pr.md) |
| `launch` | Per-project Claude Code / Codex CLI / Pi launcher (alias: `cl`) | [launch](docs/commands/launch.md) |
| `resume` | Get back into a Claude Code session after a terminal restart | [resume](docs/commands/resume.md) |
| `surface` | Start a harness inside a terminal manager (tmux/cmux/herdr) without exposing its invocation | Usage below |
| `workflow` | Run declarative workflows composing forgectl's other verbs (alias: `flow`) | Usage below |
| `bench` | Discover, health-check, and wire the local dev bench (hearth, chronicle) | [bench](docs/commands/bench.md) |
| `sessions` | Drain local session ledgers into the cross-machine concordance | Usage below |
| `env` | Safe `.env` management: key names visible, values never | [env](docs/commands/env.md) |
| `branch` | Prune stale/orphaned git branches (alias: `br`) | Usage below |
| `clean` | Reclaim dep/build directories under a project root (alias: `cln`) | Usage below |
| `docker` | Build/run/shell images tagged from git repo/branch/sha | Usage below |
| `docs` | Local markdown reader: render + serve an indexed doc set over loopback HTTP | [docs](docs/commands/docs.md) |
| `net` | Check cached reachability of the configured probe endpoint | Usage below |
| `proxy` | Apply config-defined profiles to the current shell through an explicit wrapper | [proxy](docs/commands/proxy.md) |
| `k8s` | Safely stream ordinary kubectl logs, plus bounded namespace/exec/inspect helpers | [k8s](docs/commands/k8s.md) |
| `ghostty` | Theme + keybind reporting, parsed live from the ghostty CLI | Usage below |
| `pip` | Comment- and whitespace-preserving `pip.conf` editor | Usage below |
| `quarantine` | Reversibly hide AI-instruction files (`CLAUDE.md`, `AGENTS.md`, …) from a workspace | Usage below |
| `review` | Cross-project work inventory: open issues and PRs across your repos | [projects and review](docs/commands/projects-and-review.md) |
| `preflight` | Align enabled plugins to the skill catalog's core-tier default set | Usage below |
| `update` | Weekly package-manager + OS maintenance, independently-scoped steps | Usage below |
| `doctor` | Ecosystem health check: claude, tmux/ghostty/cmux, gh auth, config, the bench, the trust store | Usage below |
| `upgrade` | Update forgectl itself via the Homebrew tap | Usage below |
| `y` | Clipboard (macOS only) + read-only zsh history recall | Usage below |

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

# init — scaffold every config.toml section with commented, sensibly-defaulted templates
forgectl init               # append (or, for the host-scalar preamble, prepend) each
                             #   section's template iff that section is absent —
                             #   never overwrites or reflows what's already there

# projects — cross-host project inventory (alias: proj)
forgectl projects list [query]           # list all projects: local clones + your GitHub repos + your Gitea repos
forgectl projects list --json            # machine-readable JSON (safe to pipe; degradation notes go to stderr)
forgectl projects list --host github.com # filter to one hostname (or "local")
forgectl projects list --host git.example.com forge  # host filter + name substring
forgectl projects pick [query]           # picker with both descriptors TTY; otherwise sanitized candidates on stdout + exit 1 (aliases: p, open)
forgectl projects                        # shorthand for pick; same headless candidate/exit-1 contract
forgectl projects clone [query]          # picker with both descriptors TTY; otherwise candidates + exit 1 (use sshUrl from list --json)
forgectl projects clone --dry-run <target>  # print where it would land and exit, touching nothing
forgectl projects clone --wing mcp <target> # override the [[projects.wings]] table for this clone
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

# surface — start a harness inside a terminal manager without exposing its invocation
forgectl surface launch <target> --surface tmux           # tmux, cmux, or herdr — always explicit, never a default
forgectl surface launch . --surface tmux --name review     # override the display name (defaults to the target dir's name)

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

# env — safe .env management: key names visible, values never (see docs/commands/env.md)
forgectl env keys [--file .env]                             # list KEY names only — never values
forgectl env set KEY [--file .env] [--clipboard]             # value from piped stdin, no-echo prompt, or clipboard — never argv
forgectl env get KEY --clipboard [--file .env]               # value to clipboard only; no print path exists
forgectl env check [--file .env] [--example .env.example]    # missing/extra keys, names only (see docs/commands/env.md for exit codes)
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
forgectl docker build [context] -- --platform linux/arm64  # args after -- pass straight through to
                                          #   docker build; --platform there overrides the derived one
forgectl docker run [-- args...]         # run the built (or --tag) image
forgectl docker shell                    # open a shell in the built (or --tag) image

# docs — local markdown reader: render + serve an indexed doc set over loopback HTTP
forgectl docs serve [dir|file ...]       # render + serve, loopback-only (DNS-rebinding-safe)
forgectl docs serve --open               # also open the system browser
forgectl docs open [path]                # point the browser at a doc on the already-running reader
forgectl docs list [dir|file ...]        # list the indexed docs, no server (--json for scripting)

# net — check cached reachability of the configured probe endpoint
forgectl net                             # show the cached (or freshly probed) answer
forgectl net --refresh                   # force a new probe, bypassing the cache
forgectl net --json                      # machine-readable output for scripting

# proxy — apply config-defined profiles to the current shell through an explicit wrapper
forgectl proxy use NAME                  # emit a fixed export/unset batch (does not mutate the parent shell)
forgectl proxy off                       # emit unsets for every supported upper/lower-case variable
forgectl proxy list                      # list configured profile names only — never a value
forgectl proxy status                    # matched profile + per-variable set/unset — never a value

# k8s — safely stream ordinary kubectl logs, plus small bounded namespace/exec/inspect helpers
forgectl k8s logs deployment/api -f                     # forward resource/follow args directly to kubectl logs
forgectl k8s logs -n prod -l app=api -f --log-level warn # keep WARN+ JSON logs plus every unrecognized line
forgectl k8s logs pod/api --color never                  # force color policy: auto | always | never
forgectl k8s ns                                          # print the current context's namespace (default when unset)
forgectl k8s ns staging                                  # switch the current context to the staging namespace
forgectl k8s exec -it pod/api -- sh                       # kubectl exec argv forwarded verbatim, real TTY wired through
forgectl k8s inspect deployment/api                       # describe + get -o wide + events, in that fixed order
forgectl k8s inspect pod/api-7f6c9 -n prod                # extra args forward to all three kubectl calls unchanged

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

# preflight — align enabled plugins to the skill catalog's core-tier default set
forgectl preflight                       # report the change-set, make no changes
forgectl preflight --apply               # write the COMPLETE aligned enabledPlugins set to
                                          #   .claude/settings.local.json (a delta, not a merge —
                                          #   see § preflight below before running this)
forgectl preflight --json                # machine-readable report for scripting

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
forgectl y file ./a.pdf                  # put a file reference on the clipboard (attaches, not text; macOS only)
forgectl y img ./a.png                   # decode + put image data on the clipboard (pastes as a picture; macOS only)
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

## How it fits together

```
forgectl tmux pick
    └── delegates session selection to sesh
            └── hands off to tmux

forgectl projects list / pick
    ├── local clone walk (git remote get-url)
    ├── gh repo list (github.com, per owner) ─┐ concurrent
    └── tea repo ls  (your Gitea host)       ─┘

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

`projects` builds a unified inventory across local clones, GitHub, and whichever Gitea instance `tea` is logged into. A project that isn't checked out locally shows as `[uncloned]`; picking it clones from the right host before opening the tmux session. `list --json` emits structured records to stdout — degradation notes (e.g. a host that's unreachable) go to stderr so the pipe stays clean.

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

Optional; forgectl runs with sensible defaults and no config file. Host-level settings (`no_icons`, `log_level`, `log_file`), per-command config sections (`[launch]`, `[pr]`, `[proxy.profiles]`, `[projects]`, `[review]`, `[github]`, `[bench]`, `[docs]`), and logging behavior are documented in [docs/configuration.md](docs/configuration.md).

## License

[Apache-2.0](LICENSE) WITH [Commons Clause](https://commonsclause.com/) — source-available, not OSI open source.

Use it, modify it, fork it, share it — including commercially and in-house at your company. The one restriction: you may not **sell** the software, meaning you may not provide it to third parties for a fee or other consideration (including paid hosting or consulting/support offerings) where the value derives substantially from forgectl's own functionality.

Relicensing binds only future releases, not anything already published: `v0.1.0`–`v0.5.0` remain available under MIT, and `v0.6.0`, `v0.7.0`, and `v0.7.1` remain available under PolyForm Noncommercial 1.0.0. Apache-2.0 with Commons Clause applies from the next release onward.
