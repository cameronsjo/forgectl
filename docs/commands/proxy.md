# forgectl proxy — apply config-defined profiles to the current shell through an explicit wrapper

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

`list` and `status` are the read-only verbs, and neither opens a new sensitive
output surface: `list` prints configured profile *names* only, sorted, never a
value; `status` names which configured profile (if any) the current
environment matches, then reports each proxy variable as `set`/`unset` —
also never a value, from either the configuration or the live environment. A
half-applied environment (some but not all of a profile's variables set)
matches no profile, and that "no match" verdict, plus the per-variable
set/unset breakdown, is `status`'s whole point.
