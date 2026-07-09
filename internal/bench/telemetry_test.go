package bench

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestTelemetryEnv_DisabledReturnsNil(t *testing.T) {
	if env := TelemetryEnv(config.Config{}); env != nil {
		t.Errorf("TelemetryEnv with telemetry off = %v, want nil", env)
	}
}

func TestTelemetryEnv_EnabledDefaults(t *testing.T) {
	cfg := config.Config{Bench: config.BenchConfig{Telemetry: true}}
	env := TelemetryEnv(cfg)

	want := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":    "1",
		"OTEL_EXPORTER_OTLP_ENDPOINT":     "http://localhost:16317",
		"OTEL_EXPORTER_OTLP_PROTOCOL":     "grpc",
		"OTEL_METRICS_EXPORTER":           "otlp",
		"OTEL_LOGS_EXPORTER":              "otlp",
		"OTEL_METRICS_INCLUDE_SESSION_ID": "true",
	}
	if len(env) != len(want) {
		t.Fatalf("env has %d keys, want %d: %v", len(env), len(want), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
	// Claude Code does not read OTEL_SERVICE_NAME — it must not be injected.
	if _, ok := env["OTEL_SERVICE_NAME"]; ok {
		t.Errorf("OTEL_SERVICE_NAME must not be injected: %v", env)
	}
}

func TestTelemetryEnv_EndpointAndProtocolOverride(t *testing.T) {
	cfg := config.Config{Bench: config.BenchConfig{
		Telemetry:    true,
		OTLPEndpoint: "http://collector.local:4317",
		OTLPProtocol: "http/protobuf",
	}}
	env := TelemetryEnv(cfg)

	if got := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://collector.local:4317" {
		t.Errorf("endpoint = %q, want the override", got)
	}
	if got := env["OTEL_EXPORTER_OTLP_PROTOCOL"]; got != "http/protobuf" {
		t.Errorf("protocol = %q, want the override", got)
	}
}
