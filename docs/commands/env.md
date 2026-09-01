# forgectl env — safe `.env` management: key names visible, values never

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

`forgectl env` touches `.env` files without ever putting a secret value in argv, terminal output, or a session transcript: key names are always visible, values never print. It's built for agent-driven workflows — an agent can be trusted with the tool even though it can't be trusted to keep a value out of its own transcript, because the tool structurally never hands one back.

```sh
forgectl env keys [--file .env]                             # list KEY names only — never values
forgectl env set KEY [--file .env] [--clipboard]             # value from piped stdin, no-echo prompt, or clipboard — never argv
forgectl env get KEY --clipboard [--file .env]               # value to clipboard only; no print path exists
forgectl env check [--file .env] [--example .env.example]    # missing/extra keys, names only
forgectl env redact [--file .env]                            # print file with values masked ****
#   --file must name an env file (.env, .env.*, *.env); --any-file overrides, TTY-confirmed only
```

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
