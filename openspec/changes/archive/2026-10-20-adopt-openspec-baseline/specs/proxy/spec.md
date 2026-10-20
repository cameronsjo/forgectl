## Purpose
`forgectl proxy` applies config-defined proxy profiles to the caller's shell through an explicit eval wrapper, and to every harness forgectl launches, without ever displaying a proxy value.

## ADDED Requirements

### Requirement: use emits a fixed shell batch
`forgectl proxy use NAME` SHALL print one line of `export` and `unset` builtins covering `http_proxy`, `https_proxy`, `all_proxy`, and `no_proxy`. Each set field MUST be exported to both its upper- and lower-case spelling with a strictly single-quoted value, and each omitted field MUST unset both spellings. The batch SHALL be built in full before any byte is written.

#### Scenario: Partial profile
- **WHEN** profile `work` sets only `https_proxy` and `no_proxy`
- **THEN** stdout exports `HTTPS_PROXY`, `https_proxy`, `NO_PROXY`, and `no_proxy`
- **AND** unsets `HTTP_PROXY`, `http_proxy`, `ALL_PROXY`, and `all_proxy`

#### Scenario: Quote in a value
- **WHEN** a value contains a single quote
- **THEN** it is emitted as a closed quote, escaped quote, reopened quote so the shell reads it literally

### Requirement: use refuses unrepresentable or empty profiles
`proxy use` MUST refuse an unconfigured profile name, a profile with no values, and a value containing a NUL byte, writing nothing to stdout. The NUL error MUST name neither the field nor the value.

#### Scenario: Empty profile
- **WHEN** profile `blank` sets no fields
- **THEN** `proxy use blank` fails and stdout is empty

#### Scenario: Unknown profile
- **WHEN** the operator runs `proxy use nope` and no `nope` profile exists
- **THEN** the command fails naming the profile as not configured

### Requirement: off unsets every spelling
`forgectl proxy off` SHALL print a single `unset` of all eight supported spellings, regardless of configuration.

#### Scenario: Off
- **WHEN** the operator runs `forgectl proxy off`
- **THEN** stdout is `unset HTTP_PROXY http_proxy HTTPS_PROXY https_proxy ALL_PROXY all_proxy NO_PROXY no_proxy`

### Requirement: list prints names only
`forgectl proxy list` MUST print configured profile names sorted, one per line, and never a profile value. With no profiles it SHALL leave stdout empty and note it on stderr. `--json` SHALL emit `[{"name":...}]`, and `[]` when none are configured.

#### Scenario: Empty configuration as JSON
- **WHEN** no profiles are configured and the operator runs `proxy list --json`
- **THEN** stdout is `[]`

### Requirement: status reports presence, never values
`forgectl proxy status` SHALL name the configured profile whose values the environment carries in both spellings, then report each variable as `set` or `unset`. A half-applied environment MUST match no profile, and then only the no-match verdict is printed. No value from config or environment MUST appear. `--json` SHALL emit `{"matched","profile","variables":[{"name","set"}]}`.

#### Scenario: Half-applied profile
- **WHEN** only `HTTPS_PROXY` of profile `work` is exported
- **THEN** status prints that no configured profile matches
- **AND** under `--json`, `matched` is false and `variables` is `[]`

#### Scenario: Matched profile
- **WHEN** both spellings of every `work` field are exported with the configured values
- **THEN** status prints `profile: work` followed by each variable's `set` or `unset`

### Requirement: Launch profile injection
When `[proxy] launch_profile` names a profile, `launch`, `resume`, `surface launch`, and the `pr` reviewer MUST inject that profile into the harness environment: set fields in both spellings, omitted fields removed. A launch profile's own `env` SHALL override the injected values.

#### Scenario: Stale shell value
- **WHEN** the shell exports `ALL_PROXY` and the launch profile omits `all_proxy`
- **THEN** the harness starts without `ALL_PROXY` or `all_proxy`

### Requirement: Launch profile refusals exit 2
A launch MUST refuse with exit 2, before anything is written to disk, when `launch_profile` names no configured profile, names one that sets no values, sets a proxy with no `no_proxy`, or carries `user:pass@` credentials in a proxy URL. The error MUST NOT print the value.

#### Scenario: Missing no_proxy
- **WHEN** the launch profile sets `https_proxy` but no `no_proxy`
- **THEN** `forgectl launch` exits 2 and tells the operator to add `no_proxy` with `localhost` and `127.0.0.1`

#### Scenario: Credentials in URL
- **WHEN** the launch profile sets `http_proxy = "http://u:p@proxy:8080"`
- **THEN** the launch exits 2 without showing the URL

### Requirement: Proxy values stay off display surfaces
Proxy values MUST NOT be logged or rendered by any forgectl display surface: `forgectl config` redacts them, `launch which` shows only variable names, and the `use` script is the sole output that carries values.

#### Scenario: Inspect a launch
- **WHEN** a launch profile is in effect and the operator runs `forgectl launch which`
- **THEN** the injected proxy variable names appear and none of their values do
