package config

import (
	"slices"
	"testing"
)

func TestLaunchRolesDecode(t *testing.T) {
	raw := `
[launch.roles.chief-of-staff]
charter_file = "~/Projects/cadence-ecosystem/scripts/chief-of-staff/charter.md"
disable_plugins = ["cadence-voice", "artificer-voice"]
posture = "autonomous"
exclude_claude_md = true
claude_md_excludes = ["~/extra/CLAUDE.md"]
session_name = "chief-of-staff"
allow_unsafe_plugin_disable = false
`
	cfg, err := DecodeStrict([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if cfg.Launch.IsZero() {
		t.Fatal("LaunchConfig with only [launch.roles.*] must not be zero")
	}
	r, ok := cfg.Launch.Roles["chief-of-staff"]
	if !ok {
		t.Fatalf("expected chief-of-staff role, got %+v", cfg.Launch.Roles)
	}
	if r.CharterFile != "~/Projects/cadence-ecosystem/scripts/chief-of-staff/charter.md" {
		t.Errorf("CharterFile = %q", r.CharterFile)
	}
	if !slices.Equal(r.DisablePlugins, []string{"cadence-voice", "artificer-voice"}) {
		t.Errorf("DisablePlugins = %v", r.DisablePlugins)
	}
	if r.Posture != "autonomous" {
		t.Errorf("Posture = %q", r.Posture)
	}
	if !r.ExcludeClaudeMD {
		t.Error("ExcludeClaudeMD = false, want true")
	}
	if !slices.Equal(r.ClaudeMDExcludes, []string{"~/extra/CLAUDE.md"}) {
		t.Errorf("ClaudeMDExcludes = %v", r.ClaudeMDExcludes)
	}
	if r.SessionName != "chief-of-staff" {
		t.Errorf("SessionName = %q", r.SessionName)
	}
}

func TestLaunchRoles_LegacyBoundaryRefusesRoles(t *testing.T) {
	lc, unsupported, err := decodeLegacyLaunch([]byte("[roles.cos]\nposture = \"autonomous\"\n"))
	if err != nil {
		t.Fatalf("decodeLegacyLaunch: %v", err)
	}
	if !slices.Contains(unsupported, "roles") {
		t.Errorf("unsupported = %v, want 'roles'", unsupported)
	}
	if len(lc.Roles) != 0 {
		t.Errorf("legacy decode must strip Roles, got %+v", lc.Roles)
	}
}
