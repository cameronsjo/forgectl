# bench Specification

## Purpose
`forgectl bench` discovers, health-checks, starts and opens the local dev bench (the hearth telemetry stack and the chronicle transcript layer), delegating to each system's own entrypoints, and optionally wires launched sessions to the local OTLP collector.

## Requirements

### Requirement: Component locations from config or environment
`bench` SHALL locate hearth from `[bench] hearth_dir`, else `$HEARTH_DIR`, and chronicle from `[bench] chronicle_dir`, else `$CHRONICLE_DIR`. A component with neither set MUST be treated as not configured rather than as an error.

#### Scenario: hearth not configured
- **WHEN** neither `[bench] hearth_dir` nor `$HEARTH_DIR` is set
- **THEN** `bench status` reports hearth as `not-configured`

### Requirement: Health card always exits 0
`forgectl bench status` SHALL probe hearth (its `hearth` compose project plus HTTP and OTLP reachability) and chronicle (`chronicle status`, its `sessions` compose project and the `local.chronicle-sync` agent), and resolve each to `ok`, `degraded`, `unavailable` or `not-configured` with a reason. A missing `docker`, a stopped daemon or an unconfigured directory MUST be reported as a state, and the command MUST exit 0 whatever the states.

#### Scenario: Docker missing
- **WHEN** `bench status` runs on a machine with no `docker` binary
- **THEN** each configured component reports a non-`ok` state with a reason
- **AND** the exit code is 0

### Requirement: Machine-readable status
`bench status --json` SHALL print `{"hearth":{...},"chronicle":{...}}` to stdout, each component holding `name`, `state`, `reason` and `details`. Without `--json` the human card MUST also go to stdout, one line per component followed by its indented details.

#### Scenario: JSON status
- **WHEN** `forgectl bench status --json` runs
- **THEN** stdout is one JSON object with `hearth` and `chronicle` keys
- **AND** each has a `state` from `ok`, `degraded`, `unavailable` or `not-configured`

### Requirement: Bring-up through each system's own entrypoint
`forgectl bench up` SHALL start hearth by running its `scripts/start.sh` and chronicle by running `make -C <dir> sync`, never reimplementing either. An unconfigured service MUST be skipped with a note on stderr. The command MUST fail when no service is configured or a configured service's entrypoint fails, and MUST resolve both directories before running either entrypoint.

#### Scenario: Only hearth configured
- **WHEN** `bench up` runs with hearth configured and chronicle not
- **THEN** hearth's `scripts/start.sh` runs
- **AND** stderr notes that chronicle was skipped

#### Scenario: Nothing configured
- **WHEN** `bench up` runs with neither service configured
- **THEN** it exits non-zero and names the settings to set

### Requirement: Open a bench UI
`forgectl bench open [target]` SHALL open `http://hearth.localhost` for `hearth` or no target, and `http://grafana.localhost` for `grafana`, using `open` on macOS and `xdg-open` elsewhere. Any other target MUST be refused with an error naming the valid targets.

#### Scenario: Unknown target
- **WHEN** `forgectl bench open kibana` runs
- **THEN** it fails with an error suggesting `hearth` and `grafana`
- **AND** nothing is opened

### Requirement: Opt-in session telemetry
When `[bench] telemetry = true`, sessions started by `launch`, `resume`, `surface launch` and the `pr` tmux reviewer SHALL receive `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_PROTOCOL` (defaults `http://localhost:16317` and `grpc`, overridable in `[bench]`), `OTEL_METRICS_EXPORTER=otlp`, `OTEL_LOGS_EXPORTER=otlp` and `OTEL_METRICS_INCLUDE_SESSION_ID=true`. With telemetry off (the default) none of them MUST be injected.

#### Scenario: Telemetry off
- **WHEN** `[bench] telemetry` is unset and a session is launched
- **THEN** no OTLP variable is injected into its environment

#### Scenario: Telemetry on
- **WHEN** `[bench] telemetry = true` and `forgectl launch` starts a session
- **THEN** the session's environment holds the six telemetry variables

### Requirement: Launch profile overrides injected telemetry
A launch profile's own `env` value SHALL take precedence over an injected telemetry default for the same variable, and `forgectl launch which` MUST name the injected variables without showing their values.

#### Scenario: Profile sets the endpoint
- **WHEN** telemetry is on and the launch profile sets `OTEL_EXPORTER_OTLP_ENDPOINT`
- **THEN** the launched session uses the profile's value
