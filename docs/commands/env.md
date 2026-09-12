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
- **Accepted, not fixed:** a hardlink read (`ln /outside/secret ./x.env`) can read a file outside the intended tree — but creating the hardlink already implies filesystem access, so this adds nothing an attacker with that access didn't already have; the *write* path is neutralized (`writeAtomic` renames a fresh inode, so a pre-existing hardlink to the target never receives the new content).
- **The resolve-to-write TOCTOU was accepted in v1 and is now closed.** The original ruling — "openat-style hardening is overkill for a local, single-operator CLI" — rested on the wrong threat: the attacker is not another operator, it is a *cloned repository*, which can ship a symlink (git stores one as mode `120000`) and needs no local access at all.

  Two things changed. Resolution happens exactly once, and its result travels as a value rather than a boolean, so the path a human confirms is the path that gets written. And that value carries an **open descriptor on the containing directory**, pinned at resolution, with every read, write, and rename performed relative to it — so no later operation re-walks the path by name. That second half is what closes the interesting case: a fix that carried only the path still let an *intermediate directory* be swapped during the confirmation, which redirected the write exactly as the original bug did.

  **What remains:** the directory is pinned by path immediately after resolution, so its own components are walked once more at that instant — a window of microseconds rather than of operator think-time, and the same ordinary same-uid local race that predates this command. Closing even that would need a component-by-component walk from the repository root.
- **Agent-write threat model, one line:** running `env set`/`env get` under an agent grants that agent write authority over repo-contained **env files** for the duration of the session — containment (refuses outside the git repo), the env-file-name rule (below), 0600 permissions, and atomic writes bound the blast radius, but they don't remove the authority itself. The two subcommands grant distinct authorities: `env set` is **write** authority (the agent can create or overwrite a key in the file); `env get --clipboard` is **read/exfil** authority (the agent can copy an existing secret to the clipboard, where — see the residual-risk note above — any local process or clipboard manager can then read it too). Granting one does not imply granting the other.

**Safety notes:**

- Values never appear in argv, stdout, or log output — every value-bearing operation lives inside the domain package, not the CLI layer.
- Every write lands at `0600`; a looser pre-existing mode is tightened and reported (`tightened <file> to 0600`) rather than silently left alone.
- `--file` is refused unless it resolves inside the current git repository (walk-up `.git` detection, symlink-escape checked) — no editing a `.env` outside the repo you're working in.
- **`--file` must also name an env file** — `.env`, `.env.*` (`.env.local`, `.env.prod`, `.env.staging`, `.env.example`), or `*.env`. Repo-containment alone is not a bound worth having: `.git/config` is inside the repo, and `KEY=value` is valid git-config syntax, so an unconstrained `--file` turns `env set` into `core.sshCommand` — arbitrary code execution on the next `git fetch`. `.envrc` (direnv executes it) and `Makefile` (`KEY=value` is valid make) are the same shape. A blocklist would be whack-a-mole against every future execute-on-read format, so the allowlist is the bound. The point of this tool is to be the thing you hand an agent *instead of* raw shell; it must not be a shell in a trench coat.
- **`--any-file` overrides that rule behind an interactive confirmation**, and the confirmation names the *resolved* path, so it cannot be used to approve a file you were not shown. With no TTY — a piped invocation, a CI job, a harness tool call — it refuses outright.

  **What that gate does and does not bound.** It stops a caller with no pty, which covers the common agent case. It does **not** stop an agent running inside a terminal multiplexer pane: stdin there is a real pty and the prompt is answerable. An earlier version of this note claimed the TTY gate was *the* bound on an agent; that was wrong, and it is corrected here rather than quietly dropped. Read it as raising the cost and covering the ptyless case. The bound that does not depend on a pty is the env-file-name allowlist above — `--any-file` is the deliberate, human-facing way around it, and granting a session that flag is granting real authority.
- `--clipboard` is macOS-only (shells out to `pbcopy`/`pbpaste`); it errors clearly on other platforms rather than silently no-op'ing.
- Secret **lengths** stay out of the logs too: `env` builds its clipboard client with `clip.WithSensitive()`, which drops the byte-count the clipboard layer otherwise logs at `info`. A length is signal — it distinguishes key types and tracks rotations — which is the same reason `redact` masks to a fixed `****` rather than revealing length.
