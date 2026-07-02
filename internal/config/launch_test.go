package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_LaunchSection(t *testing.T) {
	dir := redirectConfigDir(t)
	cfgDir := filepath.Join(dir, "forgectl")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[launch.project]]
match = "~/Projects/minute"
model = "sonnet"
env = { OTEL_EXPORTER = "otlp" }
add_dir = ["~/Projects/minute/shared"]

[[launch.project]]
match = "~/Projects/infrastructure"
add_dir = ["~/Projects/infrastructure/homelab"]
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := Load()

	if got.Launch.Defaults.Model != "opus" {
		t.Errorf("Defaults.Model = %q, want %q", got.Launch.Defaults.Model, "opus")
	}
	if got.Launch.Defaults.AllowDanger == nil || *got.Launch.Defaults.AllowDanger != true {
		t.Errorf("Defaults.AllowDanger = %v, want pointer to true", got.Launch.Defaults.AllowDanger)
	}
	if len(got.Launch.Projects) != 2 {
		t.Fatalf("len(Projects) = %d, want 2", len(got.Launch.Projects))
	}
	if got.Launch.Projects[0].Model != "sonnet" {
		t.Errorf("Projects[0].Model = %q, want %q", got.Launch.Projects[0].Model, "sonnet")
	}
	if got.Launch.Projects[0].Env["OTEL_EXPORTER"] != "otlp" {
		t.Errorf("Projects[0].Env[OTEL_EXPORTER] = %q, want %q", got.Launch.Projects[0].Env["OTEL_EXPORTER"], "otlp")
	}
	wantAddDir := []string{"~/Projects/infrastructure/homelab"}
	if len(got.Launch.Projects[1].AddDir) != 1 || got.Launch.Projects[1].AddDir[0] != wantAddDir[0] {
		t.Errorf("Projects[1].AddDir = %v, want %v", got.Launch.Projects[1].AddDir, wantAddDir)
	}
}
