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
harness = "codex"
model = "opus"
permission_mode = "plan"
allow_danger = true
approval_policy = "never"
sandbox = "read-only"
effort = "low"
codex_binary_path = "/opt/codex"

[[launch.project]]
match = "~/Projects/minute"
model = "sonnet"
effort = "xhigh"
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
	if got.Launch.Defaults.Harness != "codex" ||
		got.Launch.Defaults.ApprovalPolicy != "never" ||
		got.Launch.Defaults.Sandbox != "read-only" ||
		got.Launch.Defaults.CodexBinaryPath != "/opt/codex" {
		t.Errorf("Codex launch defaults did not round-trip: %+v", got.Launch.Defaults)
	}
	if len(got.Launch.Projects) != 2 {
		t.Fatalf("len(Projects) = %d, want 2", len(got.Launch.Projects))
	}
	if got.Launch.Defaults.Effort != "low" {
		t.Errorf("Defaults.Effort = %q, want %q", got.Launch.Defaults.Effort, "low")
	}
	if got.Launch.Projects[0].Effort != "xhigh" {
		t.Errorf("Projects[0].Effort = %q, want %q", got.Launch.Projects[0].Effort, "xhigh")
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

// TestLaunchConfig_EffortOnlyDefaultsIsNotZero guards a trap that is silent at
// three live call sites. LaunchConfig.IsZero drives whether config.toml's
// [launch] section is honored at all: report a populated section as zero and
// `forgectl launch` falls through to the legacy claunch.conf, the shadow
// warning never fires, and `launch doctor` reports "no launch profiles
// configured" — none of which surface as an error. A config setting ONLY
// `effort` is a legitimate section, so isZero has to know about the field.
func TestLaunchConfig_EffortOnlyDefaultsIsNotZero(t *testing.T) {
	lc := LaunchConfig{Defaults: LaunchDefaults{Effort: "high"}}
	if lc.IsZero() {
		t.Error("a [launch.defaults] setting only `effort` must not report as an absent section")
	}
	if !(LaunchConfig{}).IsZero() {
		t.Error("a genuinely empty LaunchConfig must still report as zero")
	}
}
