# forgectl proxy — apply config-defined profiles to the current shell, or to every launched harness

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl proxy use NAME                  # emit a fixed export/unset batch (does not mutate the parent shell)
forgectl proxy off                       # emit unsets for every supported upper/lower-case variable
forgectl proxy list                      # list configured profile names only — never a value
forgectl proxy status                    # matched profile + per-variable set/unset — never a value
```

Proxy profiles live in config and use the environment variable spellings as
field names. Missing fields are deliberately unset when a profile is applied,
so switching profiles cannot retain stale values from the previous one:

```toml
[proxy.profiles.work]
http_proxy  = "http://proxy.example:8080"
https_proxy = "http://proxy.example:8080"
all_proxy   = "socks5://proxy.example:1080"
no_proxy    = "localhost,127.0.0.1"
```

A process cannot change its parent shell. Add this explicit zsh wrapper to
`.zshrc`; it captures the generated protocol before evaluating it, so sensitive
proxy URLs do not print in the terminal or transcript:

```zsh
function forgectl-proxy() {
  local proxy_env
  proxy_env="$(command forgectl proxy "$@")" || return
  builtin eval "$proxy_env"
}

forgectl-proxy use work
forgectl-proxy off
```

`use` emits only a fixed sequence of `export`/`unset` builtins with strictly
single-quoted values. Each configured value is applied to both its upper- and
lower-case spelling; `off` unsets all eight supported spellings. Running bare
`forgectl proxy use NAME` prints the protocol instead of changing the current
shell; use the wrapper for the actual switch.

## Applying a profile to every launch

A shell wrapper covers the shell. It does not cover the harness `forgectl launch`
starts, which is a child process with its own environment — and on a network
where the harness can only reach the internet through a proxy, a launch that
inherited nothing fails at the first request. Name a profile as
`launch_profile` and the launch verbs inject it:

```toml
[proxy]
launch_profile = "work"
```

Four commands start a harness and all four inject it: `launch`, `resume`,
`surface launch`, and `pr`. It applies whether or not the calling shell ever ran
`proxy use`. `forgectl launch which` names the injected variables, so you can
confirm a profile is in play without reading a value. The injected block sits *under* a launch profile's own `env`, so a
`[[launch.project]]` block still wins for a directory that needs different
values — but set **both** spellings there if you do, since an override of
`HTTPS_PROXY` alone leaves `https_proxy` to the injected block.

`forgectl pr` is covered too, with one difference worth knowing. Its clean-room
reviewer runs in a tmux window, and tmux can set a variable on a new window but
cannot unset one — so a variable the profile omits arrives **empty** there
rather than absent. For an HTTP client those are the same thing; for a program
that checks whether a variable exists at all, they are not.

The other difference is disclosure. tmux takes the environment on its command
line, and **process command lines are readable by other accounts on the
machine** — not just yours. That is fine for a proxy URL with no credentials in
it. If your proxy URL carries a username and password, do not name that profile
as `launch_profile`; export it in the shell that starts the tmux server instead,
where it stays in an environment rather than an argument list.

A profile field that is set is applied to both spellings; a field the profile
omits **removes** both spellings from the harness's environment, exactly as
`proxy use` unsets them. Removal rather than emptying is what makes a profile
switch deterministic — an untouched key would let a stale exported value survive
into the harness — and it keeps the two ways of applying a profile in agreement
for a client that checks whether a variable is *present* rather than what it
says.

### A launch profile must carry a `no_proxy`

**`launch_profile` refuses a profile that names a proxy and no `no_proxy`.** An
absent `no_proxy` and an empty one behave identically, and neither exempts
loopback: measured with curl 8.7.1, `http://localhost:9/` went to the proxy in
both cases and went direct only once `no_proxy` named `localhost`. Go is the
exception — it exempts loopback ahead of the bypass list — so forgectl's own
requests are unaffected while the harness's `curl`, `libcurl`, and Node
subprocesses are not. Since `forgectl launch` also points the harness at a
loopback telemetry collector, a profile with no bypass list would send local
traffic to the corporate proxy. Include `localhost` and `127.0.0.1` at minimum.

A `launch_profile` also **refuses the launch** when it names no configured
profile, or names one that sets no values. Falling back to the shell's variables
would reach the network by a path nobody chose, and would look like a successful
launch.

It also **refuses credentials in a proxy URL** (`http://user:pass@host`). The
`pr` window path hands the launch environment to tmux as `-e KEY=VAL`
arguments, so a password there would sit on the command line, readable by any
local user through `ps`. Authenticate to the proxy some other way.

All four refusals exit 2 before anything is written to disk.
`forgectl launch doctor` reports all four, so a bad key surfaces without
starting anything.

`launch_profile` is the profile *name*, which is not sensitive and prints
normally in `forgectl config`. The values it selects stay redacted there, and no
launch surface renders them: the banner prints argv, and `launch which` prints
env *keys*. The harness itself is a different matter — it can read its own
environment, so every proxy value is readable by the agent and by anything it
runs. That is one more reason a launch profile refuses credentials.

## Read-only verbs

`list` and `status` are the read-only verbs, and neither opens a new sensitive
output surface: `list` prints configured profile *names* only, sorted, never a
value; `status` names which configured profile (if any) the current
environment matches, then reports each proxy variable as `set`/`unset` —
also never a value, from either the configuration or the live environment. A
half-applied environment (some but not all of a profile's variables set)
matches no profile, and that "no match" verdict, plus the per-variable
set/unset breakdown, is `status`'s whole point.
