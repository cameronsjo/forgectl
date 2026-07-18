# forgectl

Personal dev-experience CLI for a headless macOS workbench driven over SSH — from laptops, phones, and Termius. What began as a tmux helper (superseding the ad-hoc bash `s` script; smart session-naming stays with `sesh`) is growing into the **workbench forge**: composable modules — tmux, projects, launch, workflow — with a declarative workflow DSL as the composition layer.

Built for two hands and one thumb:

- **Power mode** — typed verbs (`forgectl tmux ls`, `forgectl tmux pick`). Full keyboard, full control.
- **Thumb mode** — bare `forgectl` opens a TUI menu. Number-key select. Narrow-screen. Forgiving input. Works fine in Termius over mosh.

## Install

```sh
brew install cameronsjo/tap/forgectl
```

Requires `sesh` on `$PATH` for `tmux pick`/`tmux ls` (session smarts — path discovery, named sessions, zoxide integration). Optional, per feature: `gh` (`pr`, `review`, `projects`), `tea` (`projects` against the self-hosted Gitea), `docker` (`bench status`/`up`, `docker build/run/shell`).

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

# launch — per-project Claude Code launcher (alias: cl)
forgectl launch                    # interactive launcher: pick Model + New/Resume/Fork, then exec claude
forgectl launch <claude args…>     # apply the project profile, then pass your args straight through
forgectl launch agents --json      # pure passthrough (byte-clean); posture injected only when interactive
forgectl launch which              # show the profile resolved for the current directory (alias: config)
forgectl launch init               # scaffold the [launch] section into config.toml
forgectl launch edit               # open config.toml in $EDITOR
forgectl launch doctor             # check claude availability + launch config validity

# workflow — run declarative workflows composing forgectl's other verbs (alias: flow)
forgectl workflow run <name>              # run a workflow by name
forgectl workflow run <name> --param k=v  # override a workflow param (repeatable)
forgectl workflow run <name> --dry-run    # print the resolved plan, run nothing
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

# sessions — drain local session ledgers into the mart (a shared Postgres index
# of every machine's session history, queried from any of them)
forgectl sessions sync --dry-run          # read + count the local JSONL WAL; no DB connection
forgectl sessions sync                    # idempotent upsert into the mart + rebuild the runbook index
forgectl sessions sync --full             # bypass the lastMessageId watermark, re-upsert everything
forgectl sessions search "<query>"        # full-text search the mart's runbook index from any machine

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

# y — read/write the system clipboard (macOS only)
echo hi | forgectl y copy                # copy stdin to the clipboard
forgectl y paste                         # print the clipboard's current contents
```

The cask stages only the `forgectl` binary — `fx` is a shell alias you add yourself:

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

`workflow` is the composition layer — a declarative TOML step list forgectl parses, plans, and executes through the same seams the hand-run verbs use. `--dry-run` prints the fully resolved plan without running a step. User workflows live in `workflows/` under the config dir (paths below), overriding shipped built-ins of the same name.

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

### launch — per-project Claude Code profiles

`forgectl launch` resolves a posture from the `[launch]` section of the same `config.toml`, runs a short guided launch, then **execs** `claude` in place (via `syscall.Exec`, so Ctrl-C, the TTY, and the exit code pass through untouched). Scaffold the section with `forgectl launch init`.

```toml
[launch.defaults]
model           = "opus"     # claude --model value (alias or full id)
permission_mode = "plan"     # launch always starts in plan
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this

[[launch.project]]
match           = "~/Projects/minute"
model           = "sonnet"
env             = { OTEL_EXPORTER = "otlp" }
add_dir         = ["~/Projects/minute/shared"]
```

Resolution expands `~`, picks the `[[launch.project]]` whose `match` is the **longest path-prefix** of the real working directory, and merges it over `[launch.defaults]` — scalars: project wins when set; `env`: merged, project wins on collisions; `add_dir`: concatenated and de-duplicated. No match falls back to defaults alone. Inspect the result with `forgectl launch which`.

**Design invariants** (verified against `claude` v2.1.183):

- **Injected posture first, user args last** — a user-supplied flag (e.g. `--model`) overrides the profile because Claude Code is last-flag-wins.
- **`agents` is special** — only the agents-valid subset is injected; on `--json`/`--help`/`-h` it is pure passthrough (no banner on stdout) so `forgectl launch agents --json | jq` stays byte-clean.

**Choosing the `claude` binary** (precedence): `$FORGECTL_CLAUDE_BIN` → `[launch.defaults] binary_path` → `claude` on `$PATH`. An explicit path that is missing or non-executable is a clear error, not a silent PATH fallback.

**Zero-migration grace** — if `config.toml` has no `[launch]` section, forgectl still reads a legacy `~/.config/claunch/claunch.conf` (the `[launch]` section is the same `[defaults]` + `[[project]]` shape, just namespaced). `forgectl launch init` writes the native section for the one-time cutover.

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

- **`bench status`** probes each component — `docker compose -p hearth ps` plus HTTP/OTLP reachability, `chronicle status --json` plus the `local.chronicle-sync` LaunchAgent, and `flux ready`. Each resolves to `ok | degraded | unavailable | not-configured` with a human reason; a missing `docker`, an unloaded daemon, or an unconfigured dir is a graceful state, never an error, so `bench status` always exits 0. `--json` emits the report to stdout for scripting.
- **`telemetry = true`** injects the Claude-Code-tailored OpenTelemetry env block into launched sessions so their metrics and logs flow to the local collector. Opt-in: with it off, no session points at a collector. A profile `env` value wins over the injected default. `forgectl launch doctor` shows the current telemetry state.
- **`bench up`** brings the configured services up via their own entrypoints (hearth's `scripts/start.sh`, chronicle's `make sync`); an unconfigured service is skipped with a note. **`bench open`** opens a service UI in the browser (`open` on macOS, `xdg-open` elsewhere).

## License

[PolyForm Noncommercial License 1.0.0](https://polyformproject.org/licenses/noncommercial/1.0.0) — source-available, not OSI open source.

Noncommercial use is free: use it, modify it, fork it, share it. Commercial use — shipping it inside a product, redistribution for commercial gain, or any need for support or warranty — requires a commercial license. Reach out to Cameron Sjo to arrange one; all commercial rights are reserved.

Versions previously released under MIT remain available under MIT — relicensing binds only future releases, not anything already published.
