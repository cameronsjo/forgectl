# forgectl bench — discover, health-check, and wire the local dev bench (hearth, chronicle)

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl bench status                     # aggregate health card across all components
forgectl bench status --json              # machine-readable JSON (safe to pipe)
forgectl bench up                         # bring up the configured services via their own entrypoints
forgectl bench open [target]              # open a bench UI (hearth | grafana; default hearth)
```

`forgectl bench` is the interop spine across the local bench: the **hearth** telemetry stack and the **chronicle** transcript-retention layer. It orchestrates each system through its own frozen contract — it never reimplements one. Configure it in the `[bench]` section of the same `config.toml`:

```toml
[bench]
hearth_dir    = "~/Projects/hearth"      # else $HEARTH_DIR; unset ⇒ hearth reports not-configured
chronicle_dir = "~/Projects/chronicle"   # else $CHRONICLE_DIR
otlp_endpoint = "http://localhost:16317" # hearth's frozen OTLP transport (baked default)
otlp_protocol = "grpc"                    # baked default
telemetry     = false                     # opt-in: inject OTLP env into `forgectl launch` sessions
```

- **`bench status`** probes each component — `docker compose -p hearth ps` plus HTTP/OTLP reachability, and `chronicle status --json` corroborated against `docker compose -p sessions ps` (chronicle's DB containers) plus the `local.chronicle-sync` LaunchAgent. Each resolves to `ok | degraded | unavailable | not-configured` with a human reason; a missing `docker`, an unloaded daemon, or an unconfigured dir is a graceful state, never an error, so `bench status` always exits 0. `--json` emits the report to stdout for scripting.
- **`telemetry = true`** injects the Claude-Code-tailored OpenTelemetry env block into launched sessions so their metrics and logs flow to the local collector. Opt-in: with it off, no session points at a collector. A profile `env` value wins over the injected default. `forgectl launch doctor` shows the current telemetry state.
- **`bench up`** brings the configured services up via their own entrypoints (hearth's `scripts/start.sh`, chronicle's `make sync`); an unconfigured service is skipped with a note. **`bench open`** opens a service UI in the browser (`open` on macOS, `xdg-open` elsewhere).
