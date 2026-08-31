# forgectl configuration

> Part of [forgectl](../README.md) — see the [command roster](../README.md#command-groups).

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

`sessions.dsn` renders as `(redacted)`; its `(set)`/`(default)` marker is the useful signal and a connection string can carry a password. `launch.defaults.env` and `proxy.profiles` render their **key names only** for the same reason — they hold arbitrary environment values or credential-bearing proxy URLs. `forgectl launch which` applies the same policy to the launch map. The only forgectl output containing proxy values is the purpose-built `proxy use` shell protocol described in the [proxy](commands/proxy.md) doc; capture it through the wrapper rather than displaying it.

`--json` emits the same information machine-readably and is the stable surface — the human rendering may reflow.

## Logging

Logging is **off by default**. Set `log_level` to `debug` for the full narrative (every tmux/sesh subprocess, with timing) or `info` for just the success/failure story. Logs follow an action-oriented pattern — `Preparing to…` / `Successfully…` / `Failed to…` — so they read top-to-bottom when something goes sideways.

With `log_file = ""` (the default target once a level is set), forgectl writes to a daily file — `forgectl-YYYY-MM-DD.log` — in the config dir and prunes any such file older than 7 days on startup. Set `log_file = "-"` to log to stderr instead, or give an explicit path to opt out of rotation.

## Per-command config sections

Several command groups own their own config section, documented alongside that command:

- [`env`](commands/env.md) — safe `.env` management
- [`resume`](commands/resume.md) — session resume across repos
- [`launch`](commands/launch.md) — `[launch]`, per-project Claude Code / Codex / Pi profiles
- [`pr`](commands/pr.md) — `[pr]`, the clean-room reviewer's own posture
- [`proxy`](commands/proxy.md) — `[proxy.profiles]`, current-shell profiles
- [`projects` and `review`](commands/projects-and-review.md) — `[projects]`, `[review]`, `[github]`, whose repos get enumerated
- [`bench`](commands/bench.md) — `[bench]`, interop with the local dev services
- [`k8s`](commands/k8s.md) — bounded, terminal-safe log streaming
- [`docs`](commands/docs.md) — `[docs]`, local markdown reader
