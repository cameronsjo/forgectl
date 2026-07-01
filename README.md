# forgectl

Personal dev-experience CLI for a headless macOS workbench driven over SSH — from laptops, phones, and Termius. First module is tmux. Supersedes the ad-hoc bash `s` script. Delegates smart session-naming to `sesh`.

Built for two hands and one thumb:

- **Power mode** — typed verbs (`forgectl tmux ls`, `forgectl tmux pick`). Full keyboard, full control.
- **Thumb mode** — bare `forgectl` opens a TUI menu. Number-key select. Narrow-screen. Forgiving input. Works fine in Termius over mosh.

## Install

```sh
brew install cameronsjo/tap/forgectl
```

## Usage

```sh
forgectl                   # open TUI menu (thumb mode)
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

# launch — per-project Claude Code launcher (alias: cl)
forgectl launch                    # interactive launcher: pick Model + New/Resume/Fork, then exec claude
forgectl launch <claude args…>     # apply the project profile, then pass your args straight through
forgectl launch agents --json      # pure passthrough (byte-clean); posture injected only when interactive
forgectl launch which              # show the profile resolved for the current directory (alias: config)
forgectl launch init               # scaffold the [launch] section into config.toml
forgectl launch edit               # open config.toml in $EDITOR
forgectl launch doctor             # check claude availability + launch config validity
```

The `fx` alias is available after install:

```sh
fx                    # same as bare forgectl — opens the TUI
fx tmux ls
```

## How it fits together

```
forgectl tmux pick
    └── delegates session selection to sesh
            └── hands off to tmux

forgectl projects list / pick
    ├── local clone walk (git remote get-url)
    ├── gh repo list (github.com/cameronsjo) ─┐ concurrent
    └── tea repo ls  (git.sjo.lol/cameron)   ─┘
```

`sesh` handles the smarts — path discovery, named sessions, zoxide integration. `forgectl` provides the stable verbs and the thumb-friendly TUI on top.

`projects` builds a unified inventory across local clones, GitHub, and the self-hosted Gitea. A project that isn't checked out locally shows as `[uncloned]`; picking it clones from the right host before opening the tmux session. `list --json` emits structured records to stdout — degradation notes (e.g. a host that's unreachable) go to stderr so the pipe stays clean.

## Configuration

Optional. forgectl runs with sensible defaults and no config file. To persist preferences, drop a TOML file at `config.toml` in your OS config dir:

- macOS: `~/Library/Application Support/forgectl/config.toml`
- Linux: `~/.config/forgectl/config.toml`

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
