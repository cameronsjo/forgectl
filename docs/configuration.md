# forgectl configuration

> Part of [forgectl](../README.md) — see the [command roster](../README.md#command-groups).

Optional. forgectl runs with sensible defaults and no config file. To persist preferences, drop a TOML file at `config.toml` in your OS config dir:

- macOS: `~/Library/Application Support/forgectl/config.toml`
- Linux: `~/.config/forgectl/config.toml`

A `config.toml` that exists but does not parse is an error, not a fallback to defaults: every command exits `2` and names the file, line and column. So is one that exists but can't be read, such as a file you lack permission to read, a directory, or a FIFO: the error names the file and the reason. Only an absent file selects the defaults. `forgectl config` (alias `cfg`), `doctor`, `launch edit` and `launch doctor`, plus help, version and completion, still run so you can find and fix the file. So does `resume snapshot`, which runs from a Stop hook and always exits 0. `init` does not: it refuses to rewrite a file it cannot parse.

User workflow files share the same base: `<config dir>/workflows/<name>.workflow.toml`.

`forgectl surface ready` can take its readiness predicates from `<config dir>/surface-ready.toml`. Without that file it uses the table built into the binary (`internal/herdr/ready/predicates.toml`). A file that exists replaces the built-in table whole, so start from a copy of it. It must be a regular file (not a symlink), not writable by group or others, and at most 64 KiB; anything else, or a file that does not parse, is an error rather than a fallback. Predicates are never read from a repo or worktree.

```toml
no_icons  = false   # use ASCII markers instead of Nerd Font glyphs
log_level = "off"   # off | debug | info | warn | error
log_file  = ""      # "" = auto (daily-rotated file); "-" = stderr; or an explicit path
```

`forgectl config` (alias `cfg`) prints **every** section of the config — the host scalars, `[launch]` and its `[launch.defaults]`, and every domain section down to the nested `[review.gitea]` — one dotted key per line, each marked `(set)` when the config file supplied it or `(default)` when it came from a built-in fallback.

That marker is the point. Every section's zero value means "absent, built-in defaults apply", so a value alone cannot tell a key you never set from a key you misspelled. The command also lists **unrecognized keys** — anything in the file that bound to no field, which catches `probe_hostt` and a key filed under the wrong section — and surfaces the decode error from a malformed file instead of silently defaulting past it. Resolved values that differ from the stored ones (the `off` log level, the dated log path, the launch binary chosen by `FORGECTL_CLAUDE_BIN` > `binary_path` > `PATH`) print in their own labeled blocks.

`sessions.dsn` renders as `(redacted)`; its `(set)`/`(default)` marker is the useful signal and a connection string can carry a password. `launch.defaults.env` and `proxy.profiles` render their **key names only** for the same reason — they hold arbitrary environment values or credential-bearing proxy URLs. `forgectl launch which` applies the same policy to the launch map. `proxy.launch_profile` is a profile **name**, not a value, so it renders plain while the profile it names stays redacted.

Proxy values reach exactly two forgectl outputs, both purpose-built and neither a display surface: the `proxy use` shell protocol, and the environment of a harness started with `proxy.launch_profile` set — both described in the [proxy](commands/proxy.md) doc. Capture the shell protocol through the wrapper rather than displaying it. Note what the second one implies: a launched agent can read its own environment, so every proxy value is readable by the harness and anything it runs. A `forgectl pr` review window receives the same environment as tmux `-e KEY=VALUE` arguments, which any local user can read on the command line, so a launch profile whose proxy URL carries `user:pass@` is refused.

`--json` emits the same information machine-readably and is the stable surface — the human rendering may reflow.

## Logging

Logging is **off by default**. Set `log_level` to `debug` for the full narrative (every tmux/sesh subprocess, with timing) or `info` for just the success/failure story. Logs follow an action-oriented pattern — `Preparing to…` / `Successfully…` / `Failed to…` — so they read top-to-bottom when something goes sideways.

With `log_file = ""` (the default target once a level is set), forgectl writes to a daily file — `forgectl-YYYY-MM-DD.log` — in the config dir and prunes any such file older than 7 days on startup. Set `log_file = "-"` to log to stderr instead, or give an explicit path to opt out of rotation.

**Running `forgectl pr drain --watch` as a long-lived watcher is the case this matters most for.** Its own one-line-per-pass summary (`pass=… free=… queued=…`) prints regardless of `log_level` — it is a plain stdout write, not a log — but that line is deliberately terse: it names the pass counts, not *why* a given launch failed. Set `log_level = "info"` (or `debug` for the full subprocess narrative) with an explicit `log_file` when running `--watch` unattended, so a launch failure's `slog` detail lands somewhere a stdout-discarding process supervisor won't drop it.

## Per-command config sections

Several command groups own their own config section, documented alongside that command:

- [`env`](commands/env.md) — safe `.env` management
- [`resume`](commands/resume.md) — session resume across repos; `[[resume.on_update]]`, the hooks fired when claude updates
- [`launch`](commands/launch.md) — `[launch]`, per-project Claude Code / Codex / Pi profiles
- [`pr`](commands/pr.md) — `[pr]`, the clean-room reviewer's own posture
- [`proxy`](commands/proxy.md) — `[proxy.profiles]`, named profiles; `launch_profile` applies one to every launch
- [`projects` and `review`](commands/projects-and-review.md) — `[projects]`, `[[projects.wings]]`, `[review]`, `[github]`, whose repos get enumerated and where clones land
- [`bench`](commands/bench.md) — `[bench]`, interop with the local dev services
- [`k8s`](commands/k8s.md) — bounded, terminal-safe log streaming
- [`docs`](commands/docs.md) — `[docs]`, local markdown reader
- [`theme`](commands/theme.md) — `[theme]`, `[theme.colors]`, the palette every styled surface draws from
- [`herdr`](commands/herdr.md) — `[herdr.organize]`, the rules that group herdr tabs into workspaces
- [`desk`](commands/desk.md#forgectl-desk-add-file) — `[desk]`, `notify_herdr` and `notify_macos`: whether `desk add` signals the operator through herdr and macOS (both default on)
- [`surface`](herdr.md#drain) — `[surface.drain]`, how `surface drain` paces and caps workers: `interval`, `cap`, `per_repo`, `notify`, `idle_minutes`. An out-of-range value pauses the drain instead of falling back to a default
- `tasks` — `[tasks]`, `allowed_hosts`: the hosts, besides the built-in default, that a keychain credential may be sent to. The list applies to every keychain entry, the write entry included: a listed host can be sent whichever keychain token a command names. See [the `tasks done` contract](json-contract.md#tasks-done). An entry that is not a plain hostname makes the file invalid, and every command refuses it the way it refuses a file that does not parse

## Theme

`[theme]` selects the palette every styled surface draws from — the TUI, huh
prompts, doctor marks, and fang's `--help` and error frames. Defaults to the
Artificer terminal palette, resolved dark.

```toml
[theme]
preset = "artificer"  # "artificer" (default) | "legacy"
mode   = "dark"       # "auto" (default) | "dark" | "light"

[theme.colors]
accent = "#dbbb6f"                                # one hex: both modes
danger = { dark = "#e6a8a2", light = "#8a2418" }  # per mode
```

`mode = "auto"` asks the terminal for its background, but only where that is
safe: both stdin and stdout must be a real TTY, `NO_COLOR` must be unset, and
`TERM` must not be `screen*` or `tmux*`. A multiplexer does not forward the
query, so **everything renders dark inside tmux** regardless of the terminal
behind it. On a light terminal inside tmux, set `mode = "light"` explicitly —
that is the case auto-detection cannot see.

**Help and errors still query the terminal.** The policy above governs
forgectl's own detection. `--help`, `--version` and error output are rendered by
[fang](https://github.com/charmbracelet/fang) v1.0.0, which asks the terminal
for its background (an OSC 11 query plus a DA1 request) whenever stdout is a
TTY. It does this even where forgectl's policy refuses to probe: inside
tmux/screen, with `NO_COLOR` set, or with a forced `[theme] mode`. forgectl
ignores the answer in those cases (it resolves the palette from `mode` or the
dark default instead), so the colours are right; only the query itself is sent.
On a terminal that answers, the cost is under 50 ms. On one that never answers
(some multiplexer, ssh, and mosh setups, and agent harnesses that run a bare pty),
each help or error render waits about 4.5 s for the reply. A cancelled prompt
(Esc or Ctrl+C) skips the renderer, so it does not pay that wait (#1099). Piped
output never queries. There is no fang option to turn the query off, so this is accepted
until upstream adds one (tracked in #546).

A bad `[theme]` never stops the binary starting: it is reported by `doctor` and
`launch doctor`, and the default palette is used. Run `forgectl theme show` to
see what actually resolved, including which roles came from an override and
whether any override is inert. Role names and the contrast column are
documented in [commands/theme.md](commands/theme.md).
