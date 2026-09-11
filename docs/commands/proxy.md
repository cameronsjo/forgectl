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

Three commands start a harness and all three inject it: `launch`, `resume`, and
`surface launch`. It applies whether or not the calling shell ever ran
`proxy use`. The injected block sits *under* a launch profile's own `env`, so a
`[[launch.project]]` block still wins for a directory that needs different
values — but set **both** spellings there if you do, since an override of
`HTTPS_PROXY` alone leaves `https_proxy` to the injected block.

`forgectl pr` is **not** covered. Its clean-room reviewer runs in a tmux window,
which inherits the tmux *server's* environment, and the window-creation call
takes no environment argument. On a proxy-only network the reviewer fails at its
first network call — so export the profile in the shell that started the tmux
server, or run `pr` from a shell that has run `proxy use`.

A profile field that is set is applied to both spellings; a field the profile
omits **removes** both spellings from the harness's environment, exactly as
`proxy use` unsets them. Removal rather than emptying is what makes a profile
switch deterministic: an untouched key would let a stale exported value survive
into the harness, and an emptied key is worse than either for `NO_PROXY`, whose
empty value means "no bypass exceptions" and would send loopback traffic to the
proxy.

A `launch_profile` naming no configured profile — or naming one that sets no
values — **refuses the launch**, with exit code 2, before anything is written to
disk. Falling back to the shell's variables would reach the network by a path
nobody chose, and would look like a successful launch. `forgectl launch doctor`
reports the same refusal, so a typo surfaces without starting anything.

`launch_profile` is the profile *name*, which is not sensitive and prints
normally in `forgectl config`. The values it selects stay redacted there, and no
launch surface renders them: the banner prints argv, and `launch which` prints
env *keys*. The harness itself is a different matter — it can read its own
environment, so a proxy URL carrying credentials is readable by the agent and by
anything it runs.

## Read-only verbs

`list` and `status` are the read-only verbs, and neither opens a new sensitive
output surface: `list` prints configured profile *names* only, sorted, never a
value; `status` names which configured profile (if any) the current
environment matches, then reports each proxy variable as `set`/`unset` —
also never a value, from either the configuration or the live environment. A
half-applied environment (some but not all of a profile's variables set)
matches no profile, and that "no match" verdict, plus the per-variable
set/unset breakdown, is `status`'s whole point.
