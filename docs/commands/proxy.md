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
`launch_profile` and every launch injects it:

```toml
[proxy]
launch_profile = "work"
```

This covers all three paths that start a harness — `launch`, `resume`, and
`surface launch` — and applies whether or not the calling shell ever ran
`proxy use`. The injected block sits *under* a launch profile's own `env`, so a
`[[launch.project]]` block still wins for a directory that needs different
values.

Every supported variable is injected, and a field the profile omits is injected
as the empty string rather than left alone. That is deliberate: the merge
overrides the calling shell's environment key by key, so an omitted key would
let a stale exported value survive into the harness — the one thing naming a
profile is meant to prevent. Empty reads as no-proxy in the clients that matter,
which is the same effect `use` gets by unsetting the pair.

A `launch_profile` naming no configured profile **refuses the launch**. Falling
back to the shell's variables would reach the network by a path nobody chose,
and would look like a successful launch.

`launch_profile` is the profile *name*, which is not sensitive and prints
normally in `forgectl config`. The values it selects stay redacted there, and no
launch surface renders them: the banner prints argv, and `launch which` prints
env *keys*.

## Read-only verbs

`list` and `status` are the read-only verbs, and neither opens a new sensitive
output surface: `list` prints configured profile *names* only, sorted, never a
value; `status` names which configured profile (if any) the current
environment matches, then reports each proxy variable as `set`/`unset` —
also never a value, from either the configuration or the live environment. A
half-applied environment (some but not all of a profile's variables set)
matches no profile, and that "no match" verdict, plus the per-variable
set/unset breakdown, is `status`'s whole point.
