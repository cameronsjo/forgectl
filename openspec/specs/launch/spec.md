# launch Specification

## Purpose
`forgectl launch` resolves a per-project posture from the `[launch]` section of config.toml and execs Claude Code, Codex CLI, or Pi in place with that posture applied.

## Requirements

### Requirement: Exec the harness in place
A bare `forgectl launch` or `forgectl launch <args>` MUST resolve the profile and replace the forgectl process with the configured harness, with no interactive prompt, so the TTY, signals, and the harness exit code pass through untouched.

#### Scenario: Bare launch drops into the resolved profile
- **WHEN** the operator runs `forgectl launch` in a directory
- **THEN** forgectl execs the configured harness with the resolved posture
- **AND** no picker or prompt is shown first

### Requirement: Longest-prefix profile resolution
The launch profile SHALL be `[launch.defaults]` merged with the `[[launch.project]]` whose `match` (with `~` expanded) is the longest path prefix of the physical working directory. Scalars set on the project win, `env` maps merge with the project winning on collisions, and `add_dir` lists concatenate without duplicates. With no match, defaults apply alone.

#### Scenario: Nested project blocks
- **WHEN** blocks match both `~/Projects` and `~/Projects/minute` and the cwd is inside `~/Projects/minute`
- **THEN** the `~/Projects/minute` block is merged over the defaults

#### Scenario: No block matches
- **WHEN** no `[[launch.project]]` match is a prefix of the cwd
- **THEN** the profile is `[launch.defaults]` alone

### Requirement: Effort resolves last against the final model
Effort MUST come from the matched project block if set, else `[launch.defaults]`, else derived from the final model (`sonnet` to `high`, `opus` and `fable` to `medium`, with a `[1m]` suffix ignored). Any other model emits no `--effort` flag. An effort outside `low`, `medium`, `high`, `xhigh`, `max` MUST be refused before anything launches.

#### Scenario: Project overrides only the model
- **WHEN** a project block sets `model = "sonnet"` and no layer sets `effort`
- **THEN** the claude argv carries `--effort high`

#### Scenario: Unmapped model
- **WHEN** the resolved model is `haiku` or `opusplan` and no effort is configured
- **THEN** no `--effort` flag is emitted

#### Scenario: Invalid effort
- **WHEN** a profile sets `effort = "huge"`
- **THEN** launch refuses and nothing is executed

### Requirement: Non-writing default posture
With no configuration, a Claude launch SHALL start with `--permission-mode plan` and a Codex launch SHALL start with `--sandbox read-only`. A `permission_mode` outside `plan`, `default`, `manual`, `acceptEdits`, `auto`, `dontAsk`, `bypassPermissions` MUST be refused.

#### Scenario: Default Claude posture
- **WHEN** a Claude launch runs with an empty `[launch]` section
- **THEN** the argv carries `--permission-mode plan`

#### Scenario: Unknown permission mode
- **WHEN** `permission_mode = "yolo"` is configured
- **THEN** the launch is refused before exec

### Requirement: Injected posture first, operator args last
forgectl MUST place its injected flags before the operator's arguments so an operator flag such as `--model` wins under last-flag-wins, and MUST emit every `--add-dir` ahead of a closing flag so a trailing prompt is never consumed as a directory.

#### Scenario: Operator overrides the model
- **WHEN** the operator runs `forgectl launch --model sonnet` against a profile with `model = "opus"`
- **THEN** the argv carries the profile's `--model opus` before the operator's `--model sonnet`

#### Scenario: Prompt after add-dir
- **WHEN** a profile sets `add_dir` and the operator passes a prompt
- **THEN** the prompt reaches claude as a prompt, not as another directory

### Requirement: Byte-clean passthrough for subcommands, help, and version
When the first argument (after an optional leading `--`) is a Claude subcommand (such as `mcp`, `doctor`, `update`), `-v`, or `--version`, or is `-h` or `--help` after a leading `--`, and print mode is not selected, forgectl MUST run claude with no injected flags and no banner, while still applying the profile environment. `forgectl launch --help` SHALL print forgectl's own help.

#### Scenario: Claude subcommand
- **WHEN** the operator runs `forgectl launch mcp list`
- **THEN** claude receives exactly `mcp list` with no permission mode, model, or banner

#### Scenario: Claude help through the separator
- **WHEN** the operator runs `forgectl launch -- --help`
- **THEN** claude's help is shown, not forgectl's

### Requirement: Print mode keeps only the permission mode
When `-p`/`--print` appears in flag position before claude's own `--`, or `--output-format` appears with stdout not a terminal, forgectl SHALL inject only the profile's `--permission-mode`: no model, effort, `--add-dir`, `--allow-dangerously-skip-permissions`, or banner.

#### Scenario: Print-mode launch
- **WHEN** the operator runs `forgectl launch -p "summarize"`
- **THEN** the argv carries `--permission-mode` and the prompt, and no `--model` or `--add-dir`

### Requirement: Unattended runs never reach bypass
When stdout is not a terminal, every session-starting posture (bare, builder, `agents` with injection, and `resume`) MUST withhold `--allow-dangerously-skip-permissions` even when `allow_danger` is set, while keeping the rest of the posture.

#### Scenario: Piped launch
- **WHEN** `allow_danger = true` and the operator runs `forgectl launch -- "task" | tee log`
- **THEN** the argv omits `--allow-dangerously-skip-permissions`
- **AND** keeps `--permission-mode`, `--model`, and `--add-dir`

### Requirement: Launch-owned leading tokens
A single leading `--` MUST be consumed by forgectl for every harness, so launch verbs such as `which` or `doctor` after it reach the harness as arguments and the harness never sees the separator. A leading `--here` MUST be consumed as forgectl's own flag and never reach the harness.

#### Scenario: Prompt named like a verb
- **WHEN** the operator runs `forgectl launch -- doctor`
- **THEN** claude's `doctor` subcommand runs, not `forgectl launch doctor`

### Requirement: Start Claude at the settings root
For postures that start a Claude session, when the cwd has no `.claude/settings.json` or `.claude/settings.local.json` and the nearest ancestor holding a `.git` entry has one, forgectl SHALL start claude at that root, print one stderr line naming both directories, and set `PWD`. It MUST NOT move for `--here`, passthroughs, Codex, Pi, continue or resume flags, or a root whose `.claude` is the user config directory.

#### Scenario: Launch from a subfolder
- **WHEN** the operator launches from `repo/pkg` and only `repo/.claude/settings.json` exists
- **THEN** claude starts in `repo` and stderr names the move

#### Scenario: Continuing a session stays put
- **WHEN** the arguments carry `--continue`
- **THEN** claude starts in the current directory

### Requirement: Binary selection order
forgectl SHALL choose the harness binary from the environment override (`FORGECTL_CLAUDE_BIN`, `FORGECTL_CODEX_BIN`, `FORGECTL_PI_BIN`), then the config path (`binary_path`, `codex_binary_path`, `pi_binary_path`), then the name on `PATH`.

#### Scenario: Environment override wins
- **WHEN** both `FORGECTL_CLAUDE_BIN` and `binary_path` are set
- **THEN** forgectl execs the `FORGECTL_CLAUDE_BIN` binary

### Requirement: launch which shows names, never values
`forgectl launch which` (alias `config`) MUST report the resolved profile, the environment variable names it sets, and an injected-keys row naming variables forgectl adds or removes (removals prefixed `-`), without printing any environment value. `--json` SHALL carry the same as `injected_env_keys`, plus `run_directory` for Claude.

#### Scenario: Proxy profile injected
- **WHEN** `[proxy] launch_profile` is set and the operator runs `forgectl launch which --json`
- **THEN** `injected_env_keys` lists proxy variable names
- **AND** no proxy value appears in the output

### Requirement: Worker posture caps
A coordinator worker MUST take its posture from `[launch.worker]` or built-in worker values (`acceptEdits`, `workspace-write`, `on-request`), taking the stricter of that and a matched project block. Setting `dontAsk`, `bypassPermissions`, `danger-full-access`, `never`, or an unranked value in `[launch.worker]` MUST refuse the launch. `auto` SHALL be allowed.

#### Scenario: Worker opts into auto
- **WHEN** `[launch.worker] permission_mode = "auto"` and no project block sets a mode
- **THEN** the worker starts in `auto`

#### Scenario: Worker bypass refused
- **WHEN** `[launch.worker] permission_mode = "bypassPermissions"`
- **THEN** the worker launch is refused

### Requirement: Opt-in local launch statistics
forgectl MUST record launches only when `[launch] usage_stats = true`. When on, it SHALL append one JSONL row per exec attempt to `${XDG_STATE_HOME:-~/.local/state}/forgectl/launch-usage.jsonl` with exactly `schema_version`, `ts`, `event`, `harness`, `model`, `session_mode`, and `posture`, and MUST NOT send it anywhere. `forgectl launch stats [days] [--json]` reads it back as an aggregate.

#### Scenario: Collection off by default
- **WHEN** `usage_stats` is unset
- **THEN** no row is written on launch

#### Scenario: Recorded row carries no context
- **WHEN** collection is on and a launch execs claude
- **THEN** the appended row holds only the seven named fields, with no directory, prompt, or argument

### Requirement: Legacy claunch.conf migration is non-destructive
On Unix, an automatic migration MUST refuse a `claunch.conf` that carries keys the launch schema does not model, leaving the source in place. Backups MUST never overwrite an existing `claunch.conf.bak`, and new config and backup files MUST be no broader than `0600`. `FORGECTL_SKIP_LEGACY_MIGRATE=1` SHALL select read-only fallback with no writes.

#### Scenario: Unknown legacy key
- **WHEN** `claunch.conf` holds a key with no `[launch]` field and the operator runs `forgectl launch migrate`
- **THEN** the command names the key and exits non-zero
- **AND** `claunch.conf` is left unchanged

### Requirement: Worker settings file merges into a claude worker's settings
When `[launch.worker] settings` names a file (absolute, or `~/`-relative), a claude worker launch MUST read it as JSON and merge it into the worker's `--settings` document. A key forgectl already sets SHALL keep forgectl's value, and deny rules SHALL be the union of both lists. Codex and pi workers MUST NOT read the file.

#### Scenario: Plugin turned off for workers
- **WHEN** the file is `{"enabledPlugins": {"artificer-voice@workbench": false}}`
- **THEN** a claude worker's `--settings` carries that entry beside forgectl's own keys

#### Scenario: No file
- **WHEN** `[launch.worker] settings` is unset
- **THEN** the worker's `--settings` is unchanged

### Requirement: Worker settings file can only remove
The file MUST only carry `enabledPlugins` with `false` values, `skillOverrides` with `off` only, `outputStyle`, and `permissions.deny` rules shaped `Tool`, `Tool(specifier)` or `mcp__…`. Any other key or value, a missing or unreadable file, or a file over 64 KiB MUST refuse the launch.

#### Scenario: Widening file refused
- **WHEN** the file carries `permissions.allow` or `enabledPlugins` with `true`
- **THEN** the worker launch is refused

### Requirement: launch which shows the worker settings file
`forgectl launch which` and `forgectl config` SHALL show the configured file under the worker row, and `--json` SHALL carry it as `worker.settings`. A file the launch would refuse MUST be reported in `worker.refused`.

#### Scenario: Refused file reported
- **WHEN** the file carries `hooks`
- **THEN** `forgectl launch which --json` reports `worker.refused` naming `[launch.worker] settings`
