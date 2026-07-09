package bench

import "github.com/cameronsjo/forgectl/internal/config"

// TelemetryEnv returns the Claude-Code-tailored OpenTelemetry environment block
// to inject into a launched session when [bench].telemetry is enabled, else nil.
//
// The endpoint and protocol are hearth's frozen transport (baked defaults,
// overridable in [bench]); the enable + exporter knobs are Claude-Code-specific
// constants forgectl owns. Claude Code does not read OTEL_SERVICE_NAME, and
// emits only metrics + logs over OTLP (traces require a separate beta flag,
// deliberately omitted). Injection is opt-in, so a nil result — the disabled
// case — means no session ever points at a collector the user didn't choose.
func TelemetryEnv(cfg config.Config) map[string]string {
	if !cfg.Bench.Telemetry {
		return nil
	}
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":    "1",
		"OTEL_EXPORTER_OTLP_ENDPOINT":     cfg.Bench.ResolvedOTLPEndpoint(),
		"OTEL_EXPORTER_OTLP_PROTOCOL":     cfg.Bench.ResolvedOTLPProtocol(),
		"OTEL_METRICS_EXPORTER":           "otlp",
		"OTEL_LOGS_EXPORTER":              "otlp",
		"OTEL_METRICS_INCLUDE_SESSION_ID": "true",
	}
}
