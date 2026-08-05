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

// TestMergeLegacyIntoLaunch covers the additive-only merge behind the
// shadow-scenario auto-migration (#114): a config.toml field that's already
// set must never be touched, a zero field is backfilled from legacy, and a
// legacy [[project]] is appended only when its match path isn't already
// present.
func TestMergeLegacyIntoLaunch(t *testing.T) {
	t.Run("backfills only zero-valued defaults fields", func(t *testing.T) {
		trueVal := true
		cfg := Config{Launch: LaunchConfig{Defaults: LaunchDefaults{
			Model:       "opus", // already set — must not be overwritten
			AllowDanger: &trueVal,
		}}}
		legacy := LaunchConfig{Defaults: LaunchDefaults{
			Model:          "sonnet", // conflicts with cfg's — must be ignored
			PermissionMode: "plan",   // absent in cfg — must be pulled in
			Effort:         "high",   // absent in cfg — must be pulled in
		}}

		merged, added := MergeLegacyIntoLaunch(cfg, legacy)

		if merged.Defaults.Model != "opus" {
			t.Errorf("Model = %q, want the config.toml value preserved (%q)", merged.Defaults.Model, "opus")
		}
		if merged.Defaults.AllowDanger == nil || *merged.Defaults.AllowDanger != true {
			t.Errorf("AllowDanger = %v, want the config.toml pointer preserved", merged.Defaults.AllowDanger)
		}
		if merged.Defaults.PermissionMode != "plan" {
			t.Errorf("PermissionMode = %q, want backfilled %q", merged.Defaults.PermissionMode, "plan")
		}
		if merged.Defaults.Effort != "high" {
			t.Errorf("Effort = %q, want backfilled %q", merged.Defaults.Effort, "high")
		}
		if added != 2 {
			t.Errorf("added = %d, want 2 (PermissionMode + Effort)", added)
		}
	})

	t.Run("appends only unseen project match paths", func(t *testing.T) {
		cfg := Config{Launch: LaunchConfig{Projects: []LaunchProject{
			{Match: "~/Projects/minute", Model: "opus"}, // must survive untouched
		}}}
		legacy := LaunchConfig{Projects: []LaunchProject{
			{Match: "~/Projects/minute", Model: "sonnet"}, // same match — must be skipped
			{Match: "~/Projects/hearth", Model: "haiku"},  // new match — must be appended
		}}

		merged, added := MergeLegacyIntoLaunch(cfg, legacy)

		if len(merged.Projects) != 2 {
			t.Fatalf("len(Projects) = %d, want 2 (no duplicate for the shared match)", len(merged.Projects))
		}
		if merged.Projects[0].Model != "opus" {
			t.Errorf("Projects[0].Model = %q, want the config.toml value preserved (%q)", merged.Projects[0].Model, "opus")
		}
		if merged.Projects[1].Match != "~/Projects/hearth" || merged.Projects[1].Model != "haiku" {
			t.Errorf("Projects[1] = %+v, want the new legacy entry appended", merged.Projects[1])
		}
		if added != 1 {
			t.Errorf("added = %d, want 1 (only the unseen match)", added)
		}
	})

	t.Run("fully superseded legacy contributes nothing", func(t *testing.T) {
		trueVal := true
		cfg := Config{Launch: LaunchConfig{
			Defaults: LaunchDefaults{Model: "opus", PermissionMode: "plan", AllowDanger: &trueVal},
			Projects: []LaunchProject{{Match: "~/Projects/minute", Model: "opus"}},
		}}
		legacy := LaunchConfig{
			Defaults: LaunchDefaults{Model: "sonnet", PermissionMode: "plan"},
			Projects: []LaunchProject{{Match: "~/Projects/minute", Model: "sonnet"}},
		}

		merged, added := MergeLegacyIntoLaunch(cfg, legacy)

		if added != 0 {
			t.Errorf("added = %d, want 0 (every legacy field/project already present)", added)
		}
		if merged.Defaults.Model != "opus" || len(merged.Projects) != 1 || merged.Projects[0].Model != "opus" {
			t.Errorf("merged = %+v, want config.toml's values entirely unchanged", merged)
		}
	})
}
