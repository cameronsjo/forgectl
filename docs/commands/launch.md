# forgectl launch — per-project Claude Code, Codex, and Pi profiles

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl launch                    # drop straight into the resolved profile (no prompt)
forgectl launch <harness args…>    # apply the project profile, then exec the configured harness
forgectl launch agents --json      # pure passthrough (byte-clean); posture injected only when interactive
forgectl launch mcp list           # Claude subcommands, -- --help, --version: byte-clean, no posture
forgectl launch -p "<prompt>"      # print mode: only the profile's --permission-mode is injected
forgectl launch -- <harness args…> # `--` ends launch verbs: `-- doctor` is claude's, not forgectl's
forgectl launch --here [args…]     # from a repo subfolder: start claude here, not at the repo root
forgectl launch which              # show the profile resolved for the current directory (alias: config)
forgectl launch init               # scaffold the [launch] section into config.toml
forgectl launch migrate            # explicitly import an existing claunch.conf without retiring it
forgectl launch init --from-claunch # deprecated alias for `launch migrate`
forgectl launch edit               # open config.toml in $EDITOR
forgectl launch doctor             # check harness availability + launch config validity
```

`forgectl launch` resolves a posture from the `[launch]` section of the same
`config.toml`, then **execs** Claude Code, Codex CLI, or Pi in place (via
`syscall.Exec`, so Ctrl-C, the TTY, and the exit code pass through untouched).
There is no prompt — a bare `forgectl launch` drops straight into the resolved
profile. To resume or fork an earlier session, use `forgectl resume`, which
discovers sessions across every repo, flags the live ones, and restores their
tasks. Claude remains the compatibility default.

```toml
[launch.defaults]
harness         = "claude"   # or "codex" / "pi"
model           = "opus"     # remove or replace for Codex/Pi
# effort        = "medium"   # low|medium|high|xhigh|max; unset = derived from model
permission_mode = "plan"     # Claude starts in plan; plan|default|manual|acceptEdits|auto|dontAsk|bypassPermissions, anything else is refused
allow_danger    = true       # adds --allow-dangerously-skip-permissions (reachable, not on)
# binary_path   = ""         # explicit claude path; $FORGECTL_CLAUDE_BIN overrides this
# Codex settings when harness = "codex":
# approval_policy   = "on-request"   # untrusted|on-request|never; older Codex accepted "untrusted", 0.160 refuses it
# sandbox           = "read-only"   # launch always starts non-writing; opt up to "workspace-write"
# codex_binary_path = ""      # $FORGECTL_CODEX_BIN overrides this
# Pi settings when harness = "pi":
# provider       = "lm-studio" # optional; unset = Pi's configured/default provider
# pi_binary_path = ""          # $FORGECTL_PI_BIN overrides this

[[launch.project]]
match           = "~/Projects/minute"
model           = "sonnet"
effort          = "xhigh"    # omit to take sonnet's derived "high"
env             = { OTEL_EXPORTER = "otlp" }
add_dir         = ["~/Projects/minute/shared"]
```

Resolution expands `~`, picks the `[[launch.project]]` whose `match` is the **longest path-prefix** of the real working directory, and merges it over `[launch.defaults]` — scalars: project wins when set; `env`: merged, project wins on collisions; `add_dir`: concatenated and de-duplicated. No match falls back to defaults alone. Inspect the result with `forgectl launch which`.

**Effort is the exception to "scalars merge like the others."** It resolves *last*, against the **final** model:

| Layer | Wins when |
|---|---|
| `effort` on the matching `[[launch.project]]` | set — beats everything below |
| `effort` in `[launch.defaults]` | set, and the project didn't set one |
| derived from the resolved model | neither layer set one |
| nothing — no `--effort` flag at all | the model is unmapped |

The derivation is `sonnet` → `high`, `opus`/`fable` → `medium`, with a `[1m]` context-window suffix (`opus[1m]`) treated as its base model. Anything else — `haiku`, `opusplan`, a full model id like `claude-opus-5` — emits **no flag**, leaving your `settings.json` `effortLevel` in charge. `opusplan` is excluded deliberately: it runs opus for planning and sonnet for execution, and those map to different levels.

Because derivation runs last, a project block overriding only `model` **re-derives** its level rather than inheriting one picked for a different model — unless `[launch.defaults] effort` is set, which is a deliberate global floor and survives the override.

A command-line `--model` does *not* re-derive: `forgectl launch --model sonnet` keeps the profile's effort, and last-flag-wins gives you sonnet at that level. Switch models mid-session with `/model` if you want the matching effort.

An `effort` outside the five accepted levels is rejected before anything is launched — by `forgectl launch`, `forgectl launch doctor`, and the `forgectl pr` review dispatch. That last one is why the check exists: a review runs in a *detached* tmux window, and `tmux new-window` returns 0 the instant the window exists — it never observes the child. When the agent then rejects a value and exits, tmux **destroys the window** (`remain-on-exit` is off by default), taking the error message with it. Nothing is left to read. `forgectl pr list` reports each session's window liveness for exactly this reason — see the [pr](pr.md) section.

**Design invariants** (verified against `claude` v2.1.285):

- **Injected posture first, user args last** — a user-supplied flag (e.g. `--model`) overrides the profile because Claude Code is last-flag-wins.
- **`--add-dir` never sits directly before user args** — it is variadic, so
  the builder emits every `--add-dir` ahead of `--model`, which closes the
  list. Otherwise `forgectl launch "<prompt>"` would hand the prompt to claude
  as one more directory.
- **Claude subcommands, help, and version pass through byte-clean** — when the
  first argument is a Claude subcommand (`mcp`, `doctor`, `update`, …; also
  right after a leading `--`, since claude dispatches it there too) or is
  `-h`/`--help`/`-v`/`--version`, claude runs with no injected flags and no
  banner, as plain `claude` would, unless print mode is also selected
  (`launch -v -p hi` gets the print posture). The profile environment still
  applies. `forgectl launch --help` and `-h` are forgectl's own help; reach
  claude's with `forgectl launch -- --help`. Only
  the first argument counts: later, a help flag can be another flag's value
  (`-p --append-system-prompt --help "<task>"` runs the task), so it keeps the
  print or builder posture. The subcommand list is pinned against the
  installed `claude --help` by a test.
- **Print mode keeps only the permission mode** — when an argument before
  Claude's own `--` is `-p`/`--print` in flag position, or `--output-format`
  with stdout not a terminal (piped or redirected), forgectl injects the
  profile's `--permission-mode` first and nothing else: no model, effort,
  `--add-dir`, or `--allow-dangerously-skip-permissions`, and no banner. A
  later `--permission-mode` of your own still wins. On a terminal,
  `--output-format` without `-p` opens Claude's interactive session, so it
  keeps the builder posture, which also carries the permission mode. A print flag that is
  another option's value is not print mode: `--append-system-prompt -p
  "<task>"` keeps the builder posture. A flag forgectl does not know is
  assumed to take a value, which leaves the builder posture and its
  permission mode in place.
- **A piped run withholds `--allow-dangerously-skip-permissions`** — with
  stdout not a terminal, claude runs a prompt non-interactively even without
  `-p`, so `forgectl launch -- "<task>" | tee log` keeps the builder posture
  (permission mode, `--add-dir`, `--model`, `--effort`) but drops the one flag
  print mode drops for safety. The same holds for a bare `forgectl launch`
  (which keeps the session posture), `launch agents` with injection, and
  `forgectl resume`. `allow_danger` never makes bypass reachable in an
  unattended run. A flag you type into the arguments yourself still reaches
  claude.
- **One leading `--` belongs to forgectl, for every harness** — `forgectl
  launch -- <args>` skips launch's own verbs (`which`, `doctor`, `edit`, …) and
  drops the separator, so Claude, Codex, and Pi never see it. A shell wrapper
  that should behave like `claude` is `claude() { forgectl launch -- "$@"; }`.
- **`agents` is Claude-only** — Codex and Pi profiles reject the passthrough and
  point out that no adapter ships. Claude retains its agents-valid injection
  and byte-clean `--json`/`--help` passthrough. Those count only in flag
  position, not as a preceding option's value (`agents --settings --help`
  keeps its posture).
- **Claude and Codex start in postures that cannot write** — `permission_mode =
  "plan"` for Claude (print mode included; only subcommands, help, and version,
  which start no session, carry no permission mode), `sandbox = "read-only"`
  for Codex. Both are opt-ups:
  `allow_danger` makes bypass reachable, `sandbox = "workspace-write"` makes
  the checkout writable. Neither is on by default.
- **Pi uses Pi's native tool posture** — forgectl injects only configured
  `provider`/`model` selectors and the resolved profile environment. Configure
  Cadence bridge variables such as `CADENCE_BRIEFS_DIR`,
  `CADENCE_METRICS_DIR`, and `GIT_GUARDRAILS_ALLOWED_OWNERS` in `env`; forgectl
  passes their values to Pi but shows only their names in `launch which`.

**A claude session starts at the repository root when the settings live
there.** Claude Code reads `.claude/settings.json` and
`.claude/settings.local.json` only from the directory it starts in (measured on
2.1.292), so a session started in a subfolder of a repository silently runs
without the root's env, hooks, and permissions. `forgectl launch` and
`forgectl surface launch` therefore start claude at the repository root when
all of these hold:

- the directory has neither settings file of its own;
- walking up from it, the nearest directory with a `.git` entry (a directory,
  or the file a linked worktree has) holds one of them.

Otherwise claude starts where you are. The profile is still resolved for the
directory you ran from, so a `[[launch.project]]` block that matches the
subfolder still applies. The move prints one stderr line naming both
directories, and sets `PWD` to the root. It applies to the postures that start
a session (bare, builder, print, and `agents` with posture); the byte-clean
passthroughs (`mcp …`, `--help`, `agents --json`) never move, so `mcp add
--scope project` still writes where you ran it. Codex and Pi never move, since
neither reads `.claude`, and neither does a coordinator worker, which already
starts at its worktree's root.

To stay put, pass `--here` as the **first** argument: `forgectl launch --here`,
`forgectl launch --here -- "<task>"`. Like the leading `--`, it is forgectl's
and never reaches the harness, and only the first position counts: `forgectl
launch -- --here` hands claude a `--here` of its own. `surface launch` takes it
as an ordinary flag, `--here`.

**Choosing the binary** uses env → config → PATH:
`FORGECTL_CLAUDE_BIN` / `binary_path` / `claude`, or
`FORGECTL_CODEX_BIN` / `codex_binary_path` / `codex`, or
`FORGECTL_PI_BIN` / `pi_binary_path` / `pi`.

**On a network that requires a proxy**, set `[proxy] launch_profile` and
`launch`, `resume`, `surface launch`, and the `forgectl pr` reviewer all inject
that profile's variables into the harness environment — see [proxy](proxy.md),
which covers the two ways the `pr` path differs. A profile's `env` still wins
over the injected values.

**`launch which` names what gets injected.** An `injected` row (and
`injected_env_keys` under `--json`) lists the variable names forgectl adds on
top of the profile, with a `-` prefix on any it removes. Names only, never
values — the same rule the `env` row follows.

Codex modes translate to `codex`, `codex resume --last`, `codex fork --last`,
and `codex exec`. Clean-room reviews accept `--agent codex` only for
`forgectl pr local --operator-authored` — code you state that you wrote.

Pi launches as `pi [--provider <name>] [--model <pattern>]`, with operator
arguments appended last so an explicit command-line selector wins. Surface
placement stays separate: use `forgectl surface launch <target> --surface
tmux|cmux|herdr`; forgectl does not add Pi-specific pane management or start,
probe, or supervise a local model server.

> **`--agent codex` requires you to assert authorship, by design.** The Claude
> clean room confines the reviewer to a deny-by-default allowlist with no
> command-execution primitive. Codex's `--sandbox read-only` scopes writes and
> network egress but not *which commands run*, so a prompt injection in a diff
> you did not write could reach a shell with read access to your whole home
> directory — and anything read reaches the model provider as tool output.
> `codex exec` exposes no way to confine that: it accepts no
> `--permission-profile`, and `shell_environment_policy.inherit=none` is
> honored by `codex sandbox` but ignored by `codex exec` (measured on
> codex-cli 0.146.0).
>
> So the boundary is drawn by **authorship**, not by sandbox and not by
> locality. A local path proves only that the *directory* is yours — `gh pr
> checkout` puts someone else's commit in your own repository, which is the
> ordinary way to review a PR. So `forgectl pr local --agent codex` refuses
> unless you pass `--operator-authored`, and `forgectl pr <ref>` refuses
> outright with no flag to override, since a fetched PR head is someone else's
> content by construction. Use `--agent claude` (the default) for anything you
> did not write.
>
> Refusal is enforced in the dispatch path before anything is fetched or
> created, and again at launch — the second check is what covers a session
> restored from a breadcrumb. Provenance is never inferred from a signal:
> not from HEAD being attached or detached, not from path or repository
> ownership, not from the Git author or a commit signature, and not from your
> profile, UID, or terminal.

Provenance is recorded in the session breadcrumb, which has one **downgrade
limitation** worth knowing: the breadcrumb decoder rejects unknown fields, so an
*older* forgectl refuses a breadcrumb written by a newer one. Upgrading is
seamless in the forward direction — legacy breadcrumbs read as unknown
provenance, so Claude, `pr list`, `pr attach`, and `pr teardown` keep working
and only Codex needs a fresh `--operator-authored` run. If you must roll back,
use the newer binary or recreate the session state; the field is never stripped
automatically, because doing so would silently change what the record asserts.

The decoder also requires a breadcrumb file to hold exactly one JSON record:
anything but whitespace after it is rejected, since forgectl writes one record
per file and trailing content means something else has written there. This
changes nothing for a file forgectl wrote — its own terminating newline is
whitespace — so there is no migration.

**Legacy `claunch.conf` migration** — on Darwin and Linux, a launch command captures the legacy file once, decodes that exact byte slice, and uses those same bytes for a no-clobber backup before retiring the named source. An existing `claunch.conf.bak` is never overwritten; forgectl allocates an exclusive `claunch.conf.bak.<random>` name instead. A hardlinked source is allowed, but retirement removes only the `claunch.conf` directory entry and leaves sibling links unchanged. `launch migrate` is the explicit import-only form: it refuses an existing `[launch]` table and does not back up or retire the source.

Migration is refused outright when the captured file carries keys forgectl's launch schema has no field for. Such a file was only partly decoded, so rendering `[launch]` from that decode would drop the remainder and retirement would delete the one copy that still held it. Automatic migration reports the offending keys and leaves the source in place; `launch migrate` names them and exits non-zero. The read-only fallback stays lenient, so the fields forgectl does model still resolve a profile for that invocation. A `config.toml` sitting in the legacy directory is named as unmigratable rather than reported absent—it is probed for existence only, never opened, since forgectl migrates the historical `claunch.conf` format alone.

The path policy is lexical and fail-closed. An explicit `XDG_CONFIG_HOME` must be absolute, and both `<xdg>/forgectl/config.toml` and `<xdg>/claunch/claunch.conf` must be the exact cleaned children of that same root—prefix lookalikes and cross-root Darwin pairs are refused. With no explicit XDG value, Darwin keeps the historical native-config/`~/.config/claunch` pair. Symlinks, directories, FIFOs, sockets, devices, malformed TOML, unrepresentable keys, unstable reads, and identity drift are never migration sources. A legacy leaf symlink may still be followed only by the nonblocking read-only compatibility fallback when it resolves to a regular file.

On an automatic refusal, a missing native `[launch]` uses a safely decoded read-only legacy profile for that invocation; a present `[launch]`—even an empty table—remains authoritative. `FORGECTL_SKIP_LEGACY_MIGRATE=1` always selects that non-mutating behavior. No pre-capture refusal creates the config directory, writer lock, config temp, config file, or backup.

Config replacement, backup creation, and source retirement are separate reported phases. The config temp remains pinned by an attempt-owned descriptor: its name is identity-checked immediately before rename, and the renamed identity, exact bytes, and private mode are checked again before directory durability can be claimed. Cleanup immediately identity-validates the name and live descriptor before removal, preserving any replacement already visible at that check. The backup allocation likewise retains an attempt-owned identity descriptor through cleanup, so immediate inode reuse cannot make a replacement appear owned. The config file and its directory entry must be durable in the current attempt before backup begins; an added-zero retry reopens the current bytes, syncs the file, and syncs its parent before retirement can resume. If a config rename is visible but its identity/content/mode proof or directory sync fails, the source is retained. Any later backup or identity failure likewise retains the source; a failure syncing the legacy directory after unlink is reported as durability-unknown and keeps the verified backup.

A captured legacy name disappearing while a process waits for the writer lock is a successful peer no-op only when the locked `[launch]` config already supersedes every captured legacy addition. Otherwise the disappearance is reported as drift/refusal, the source is not claimed as cooperatively retired, and fallback uses the captured profile when no native `[launch]` is authoritative.

New config and backup files are owner-only and no broader than `0600`; a restrictive umask may narrow them further, and an existing config mode such as `0400`, `0200`, or `0000` is not broadened. Every cooperating launch/top-level init writer shares the same Unix sibling lock and atomic replacement path. Secure legacy mutation and directory-durability claims apply to Unix builds (the shipped Darwin/Linux targets); non-Unix builds refuse automatic and explicit legacy mutation before writer activity. Their developer-only normal init may make a replacement visible, but reports that directory durability and cross-process serialization are unavailable.

### Worker posture: `[launch.worker]`

A coordinator worker (`forgectl surface launch --worktree`) does not take its posture from `[launch.defaults]`. Those are your interactive defaults, and their built-in `plan` would leave every worker unable to write. Each worker posture field comes from `[launch.worker]`, or the built-in worker value when it is unset. When the matched `[[launch.project]]` block sets the same field itself, the worker takes the stricter of the two:

```toml
[launch.worker]
permission_mode = "acceptEdits"   # claude workers; built-in value acceptEdits
sandbox         = "workspace-write" # codex workers; built-in value workspace-write
approval_policy = "on-request"    # codex workers; built-in value on-request
```

To keep workers read-only, set `permission_mode = "plan"` in `[launch.worker]`. When an explicit `[launch.defaults]` value is stricter than what a worker gets, the worker launch prints a note naming it.

A repo block can only make a worker stricter. Its `plan` wins over `acceptEdits`, and its `bypassPermissions` loses to it. A worker floor then caps every field at `acceptEdits`, `workspace-write` and `on-request`. Setting `auto`, `dontAsk`, `bypassPermissions`, `danger-full-access` or `never` in `[launch.worker]` refuses the launch. The same values in a project block are not refused, but they lose to the stricter worker value. A value the tables do not rank is refused wherever a worker reads it: in `[launch.worker]`, and in a project block field for the worker's harness.

> Absorbed from the standalone `claunch` tool. A `claunch='forgectl launch'` shell alias preserves the old muscle memory.

## Local launch statistics

`forgectl` can count the harness launches it attempts, locally and only when you
ask it to. Collection is off unless `config.toml` says otherwise:

```toml
[launch]
usage_stats = true
```

Nothing but that key turns it on — no upgrade, migration, or environment
variable does, and a `usage_stats` in a legacy `claunch.conf` is ignored.

When on, one line is appended to
`${XDG_STATE_HOME:-~/.local/state}/forgectl/launch-usage.jsonl` immediately
before each harness exec forgectl attempts:

```json
{"schema_version":1,"ts":"2026-08-13T19:42:17Z","event":"exec_attempt","harness":"claude","model":"opus","session_mode":"new","posture":"default"}
```

Those seven fields are the whole record. There is no directory, project,
repository, branch, session id, harness argument, prompt, environment variable,
task, host, user, or process id in it — and no hash of any of them. They are
still sensitive: exact timestamps describe when you work, a model label can name
an internal deployment, and the session and posture counts describe how you
work. Aggregating locally does not change that.

Nothing is uploaded. There is no network call, no device identifier, and no
import of the retired claunch wrapper's own log.

Read it back with `forgectl launch stats [days] [--json]`; `--json` emits an
aggregate only, while the JSONL file itself is your raw export. `forgectl launch
doctor` reports whether collection is on, where the file lives, and any
permission it had to tighten.

Rows are kept until you delete them. Setting `usage_stats = false` stops new
rows but neither hides nor removes old ones. Delete them yourself, while no
`forgectl launch`, `resume`, or `stats` is running:

```sh
rm -- "${XDG_STATE_HOME:-$HOME/.local/state}/forgectl/launch-usage.jsonl"
rm -- "${XDG_STATE_HOME:-$HOME/.local/state}/forgectl/launch-usage.jsonl.lock"
```

Deletion is permanent.

An `exec_attempt` means forgectl finished validating a profile and called
`exec`. It does not mean the harness started or that the session was useful:
nothing after `syscall.Exec` is observable from a process that replaced itself.
