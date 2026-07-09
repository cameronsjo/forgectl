# forgectl bench interop — `bench status`, `launch` env-injection, `bench up|open`

## Context

Two work-critical local services now exist as working builds and each publishes a **frozen
interop contract** aimed explicitly at forgectl:

- **hearth** — a local OpenTelemetry stack on Colima (collector, Prometheus, Tempo, Loki,
  Pyroscope, Grafana).
- **chronicle** — a Postgres-backed session-transcript retention layer.

forgectl is the interop spine across the bench — one CLI that discovers, health-checks, and
wires these systems on any machine (home or work). This lane adds that spine: aggregate
health, telemetry env-injection into launched sessions, and thin lifecycle delegation —
**orchestrate, never reimplement.** If a built service later drifts from its contract, that
is a bug to file on the service, not a reason to adapt forgectl.

## Frozen contracts forgectl consumes

| System | Health probe | Lifecycle | Notes |
|---|---|---|---|
| **hearth** | `docker compose -p hearth ps --format json` (works independent of cwd — queries by project label) + HTTP probes of `hearth.localhost`, `grafana.localhost`, OTLP `:16317` | `<hearth_dir>/scripts/start.sh`; open `*.localhost` | compose project `hearth`; OTLP gRPC `:16317` / HTTP `:16318` frozen; Loki host port `:16100` |
| **chronicle** | LaunchAgent `local.chronicle-sync` loaded + `chronicle status --json` | `make -C <chronicle_dir> sync` / `launchagent` | `status --json` schema below; DSN `postgresql:///chronicle`; CLI run via `uv --directory <dir> run chronicle …` |
| **flux** | board reachable via the existing `flux` CLI + `FLUX_DIR`/`CADENCE_KANBAN` | — | probe is best-effort |

`chronicle status --json` frozen schema:
```json
{"events":0,"files":0,"sessions":0,"metrics_events":0,
 "sources":[{"source":"active","files":0,"events":0}],
 "last_sync":"ISO-8601|null","generated_at":"ISO-8601"}
```

**Telemetry block for launch injection** (Claude-Code-tailored, not hearth's verbatim
`.envrc.example`). Claude Code does **not** read `OTEL_SERVICE_NAME`; it emits metrics+logs
(traces need a separate beta flag, omitted). Minimal correct set:
```
CLAUDE_CODE_ENABLE_TELEMETRY=1
OTEL_EXPORTER_OTLP_ENDPOINT=<cfg.otlp_endpoint | http://localhost:16317>
OTEL_EXPORTER_OTLP_PROTOCOL=<cfg.otlp_protocol | grpc>
OTEL_METRICS_EXPORTER=otlp
OTEL_LOGS_EXPORTER=otlp
OTEL_METRICS_INCLUDE_SESSION_ID=true
```
The endpoint/protocol are hearth's frozen transport (baked defaults, overridable); the
enable + exporter knobs are Claude-Code-specific constants forgectl owns.

## Decisions

- **Bench config lives in a new `[bench]` section of the existing `config.toml`** — no new
  file. Frozen transport values baked as defaults + overridable; repo paths configured with
  env-var fallback (`$HEARTH_DIR` / `$CHRONICLE_DIR`); degrade gracefully when unset.
- **Telemetry injection is opt-in** via `[bench].telemetry = true` — a hearth-less machine
  (or work) omits it and no session points at a dead collector.
- **macOS-primary** (launchctl, `open`, Colima are the contract surfaces). Cross-platform is
  detect-and-degrade: skip the launchctl probe where absent; pick `open` vs `xdg-open` by
  `runtime.GOOS`.

## Config model — `internal/config/config.go`

A `BenchConfig` mirroring `WorkflowConfig` (the minimal config-driven precedent):
```go
type BenchConfig struct {
    HearthDir    string `toml:"hearth_dir"`
    ChronicleDir string `toml:"chronicle_dir"`
    OTLPEndpoint string `toml:"otlp_endpoint"`   // default http://localhost:16317
    OTLPProtocol string `toml:"otlp_protocol"`   // default grpc
    Telemetry    bool   `toml:"telemetry"`
}
```
Dir resolution: `cfg.Bench.HearthDir` → else `$HEARTH_DIR` → else empty (degrade); same for
chronicle with `$CHRONICLE_DIR`. A leading `~/` is expanded. Endpoint/protocol getters return
the baked default when the field is empty. Introduced in **PR1** (shared by PR2/PR3).

## Sequencing — one epic issue, then three phased PRs

### PR1 — `forgectl bench status` (+ `[bench]` config)

New domain package `internal/bench/` + CLI group `internal/cli/bench.go`.

- `internal/config/config.go` — `BenchConfig` (above) + resolution getters.
- `internal/bench/status.go` — `Status(ctx, cfg, runner, probe) Report`. `Report` and its
  per-component structs carry `json:"…"` tags. Each component resolves to one of
  `ok | degraded | unavailable | not-configured` with a human `reason`; a missing docker
  binary, absent launchctl, or unconfigured dir is a graceful state, never a returned error.
- `internal/bench/probe.go` — `Prober` interface with an `httpProber` (short timeout) for
  prod and a fake for tests. The only `net/http` caller; kept behind the interface so `bench`
  logic stays pure and table-testable (mirrors the `exec.Runner`/`FakeRunner` seam).
- `internal/cli/bench.go` / `bench_status.go` — parent + `status` verb; `--json` follows
  `projects list` (payload to stdout, diagnostics to stderr); human output uses the
  `✓`/`!`/`✗` glyph vocabulary.
- Tests: `internal/bench/*_test.go` (table-driven, `FakeRunner` + fake `Prober`) plus a CLI
  integration test asserting `--json` shape and the stdout/stderr split.

### PR2 — `forgectl launch` telemetry env-injection

- `internal/bench/telemetry.go` — `TelemetryEnv(cfg) map[string]string` returns the block
  above when `cfg.Bench.Telemetry`, else `nil`.
- `internal/cli/launch.go` (`launchExec`) — combine telemetry with the profile env so
  **profile env wins**, then merge over the process env.
- Surface telemetry on/off + the resolved endpoint in `forgectl launch doctor`.
- Tests: `TelemetryEnv` on/off + endpoint override; a launch integration test asserting the
  injected vars reach the child env.

### PR3 — `forgectl bench up | open`

- `internal/bench/lifecycle.go` — `Up(ctx, cfg, runner)` and `Open(ctx, cfg, runner, target)`.
  - `up`: hearth → `<hearth_dir>/scripts/start.sh`; chronicle → `make -C <chronicle_dir>
    sync`. Clear error when a required dir is unconfigured. Delegate only to contract-exposed
    entrypoints.
  - `open`: map a target name → URL (`hearth`→`hearth.localhost`, `grafana`→`grafana.localhost`,
    default `hearth.localhost`); shell `open` (darwin) / `xdg-open` (else).
- `internal/cli/bench_up.go` / `bench_open.go` — thin verbs.
- Tests: `FakeRunner` asserting the delegated argv per target + the GOOS open-command split.

## Verification (per phase)

1. `go build ./...` **and** `go vet ./...` **and** `go test ./...`.
2. **PR1**: `bench status` → human card; `bench status --json | jq` → valid JSON, one entry
   per component, stdout uncluttered. Docker/dirs absent → graceful `unavailable`/`not-configured`,
   sane exit.
3. **PR2**: with telemetry on, `launch doctor` shows telemetry + endpoint; a launched session
   emits metrics/logs to the collector. Telemetry off → no OTEL vars injected.
4. **PR3**: `bench open` opens `hearth.localhost`; `bench open grafana` opens Grafana;
   `bench up` triggers `scripts/start.sh` and `make sync`; clear errors when dirs are unconfigured.
