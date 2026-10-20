## Purpose
Configuration covers forgectl's optional `config.toml`: where it lives, how a broken file is refused rather than silently defaulted, how `forgectl config` shows the effective configuration without leaking secrets, and how `forgectl init` scaffolds it safely.

## ADDED Requirements

### Requirement: Config file location and optional defaults
forgectl SHALL read `config.toml` from `forgectl/` under the OS user config directory (`~/.config/forgectl/config.toml` on Linux, `~/Library/Application Support/forgectl/config.toml` on macOS). An absent file MUST select built-in defaults and MUST NOT be an error.

#### Scenario: No config file
- **WHEN** no `config.toml` exists and a verb runs
- **THEN** the verb runs on built-in defaults without error

### Requirement: Broken config is refused, not defaulted
A `config.toml` that exists but does not parse, cannot be read (permission denied, a directory, a FIFO), or holds a value the loader refuses (such as a `[tasks] allowed_hosts` entry that is not a plain hostname, or an unknown `log_level`) MUST stop every verb with exit 2 and an error naming the file and, for a syntax error, the line and column.

#### Scenario: Syntax error
- **WHEN** `config.toml` has a syntax error and `forgectl status` is run
- **THEN** the command exits 2 and the error names the file, line and column
- **AND** it points at `forgectl config` and `forgectl doctor`

#### Scenario: Unreadable file
- **WHEN** `config.toml` is a directory
- **THEN** every gated verb exits 2 with an error naming the file and the reason "not a regular file"

### Requirement: Recovery verbs run on a broken config
`config` (alias `cfg`), `doctor`, `launch edit`, `launch doctor`, `version`, `help`, `completion`, and any invocation with `--help`, `-h` or `--version` SHALL still run when the config is broken, so the operator can find and fix it. The hook verbs `resume snapshot` and `surface event` MUST also run. `init` MUST NOT run on an unparseable file.

#### Scenario: Inspect a broken config
- **WHEN** `config.toml` does not parse and `forgectl config` is run
- **THEN** it runs and reports the decode error instead of exiting 2

### Requirement: config shows every key with provenance
`forgectl config` SHALL print every config key as a dotted key, each marked `(set)` when the file supplied it or `(default)` when a built-in fallback applies, and SHALL list unrecognized keys that bound to no field. It MUST NOT accept positional arguments and MUST exit 0 even when it reports a decode error.

#### Scenario: Misspelled key
- **WHEN** the file sets `[net] probe_hostt = "x"`
- **THEN** `forgectl config` lists `net.probe_hostt` as unrecognized
- **AND** `net.probe_host` is marked `(default)`

### Requirement: config --json shape
`forgectl config --json` SHALL write one JSON document to stdout with keys `path`, `found`, `unrecognized` (an array, never null), `entries` (each `{key, group, value, set}`), `host_resolved`, `launch_resolved`, and `env_resolved` (each `{key, value, source}`), plus `path_error`, `decode_error` or `validation_error` when set. The shape MUST only change additively.

#### Scenario: Parse failure under --json
- **WHEN** `config.toml` does not parse and `forgectl config --json` is run
- **THEN** stdout carries the document with a `decode_error` key and the exit status is 0

### Requirement: Secrets never displayed
`forgectl config` MUST NOT print the value of `sessions.dsn` (it shows `(redacted)` with its set/default marker) and MUST render every map-valued key, including `launch.defaults.env` and `proxy.profiles`, as its key names only. Only `theme.colors` and `surface.profiles` show their values.

#### Scenario: DSN with a password
- **WHEN** `sessions.dsn` holds a connection string with a password
- **THEN** both text and `--json` output show `(redacted)` and never the string

#### Scenario: Launch environment
- **WHEN** `[launch.defaults] env` sets `GH_TOKEN`
- **THEN** `forgectl config` shows the key `GH_TOKEN` and never its value

### Requirement: Environment-resolved settings with their source
`forgectl config` SHALL show each setting an environment variable can supply (`sessions.dsn`, `bench.hearth_dir`, `bench.chronicle_dir`, `desk.dir`) as the verbs resolve it, with its source: the variable's name, `config`, `default`, `unset`, or `error` with the reason as the value.

#### Scenario: Desk dir from the environment
- **WHEN** `DESK_DIR=/abs/desk` is set and `forgectl config --json` is run
- **THEN** `env_resolved` holds `{key: "desk.dir", value: "/abs/desk", source: "DESK_DIR"}`

### Requirement: init scaffolds missing sections only
`forgectl init` SHALL create `config.toml` with every section as a commented template, and on an existing file SHALL add only the missing sections, never editing, reordering or removing a section already present. Rerunning it MUST be a no-op once every section exists. `--dry-run` MUST list what would be added and write nothing.

#### Scenario: Rerun after an edit
- **WHEN** `config.toml` already has a hand-edited `[net]` section and `forgectl init` is run
- **THEN** `[net]` is reported as already present and its bytes are unchanged

#### Scenario: Dry run
- **WHEN** `forgectl init --dry-run` is run
- **THEN** it prints the sections it would add and the file on disk is unchanged

### Requirement: Config writes are locked, atomic and private
`forgectl init` SHALL write `config.toml` under a lock, strictly decoding the current file first and refusing one that does not parse, and SHALL replace it atomically. The written file MUST carry no group or other permission bits and MUST NOT widen the mode of an existing file.

#### Scenario: Write over a world-readable file
- **WHEN** `config.toml` has mode 0644 and `forgectl init` adds a section
- **THEN** the replaced file has no group or other permission bits

### Requirement: Logging is off by default
Logging SHALL be off unless `log_level` names a level (`debug`, `info`, `warn`, `error`). With a level and an empty `log_file`, logs MUST go to a daily `forgectl-YYYY-MM-DD.log` in the config directory, created mode 0600, and such files older than 7 days are pruned. `log_file = "-"` SHALL log to stderr.

#### Scenario: Default posture
- **WHEN** no `log_level` is set
- **THEN** no log file is written
