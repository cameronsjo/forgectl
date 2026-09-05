package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Test plan for the [theme] section (ThemeConfig, ColorOverride)
//
// ColorOverride.UnmarshalTOML (Classification: decode)
//   [x] Happy: scalar hex string sets both Dark and Light
//   [x] Happy: table sets only "dark" — Light stays empty
//   [x] Happy: table sets only "light" — Dark stays empty
//   [x] Happy: table sets both
//
// ThemeConfig.Validate (Classification: pure decision function)
//   [x] Happy: zero value is valid
//   [x] Happy: preset "artificer" and "legacy" are valid
//   [x] Sad:   preset anything else is rejected
//   [x] Happy: mode "auto", "dark", "light" are valid
//   [x] Sad:   mode anything else is rejected
//   [x] Sad:   an unknown role is rejected, and the error lists every valid role
//   [x] Sad:   bad hex in .dark is rejected, naming the role/mode/value
//   [x] Sad:   bad hex in .light is rejected, naming the role/mode/value
//   [x] Sad:   an override with neither Dark nor Light set is rejected
//   [x] Happy: role names are matched case-insensitively
//   [x] Happy: multiple colors are walked in sorted key order (deterministic
//              first error)
//
// ThemeConfig.IsZero (Classification: pure decision function)
//   [x] Happy: true on the zero value
//   [x] Happy: false when Preset, Mode, or Colors alone is set

func TestColorOverride_UnmarshalTOML_ScalarSetsBothModes(t *testing.T) {
	cfg, err := DecodeStrict([]byte(`[theme.colors]
accent = "#dbbb6f"
`))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	want := ColorOverride{Dark: "#dbbb6f", Light: "#dbbb6f"}
	if got := cfg.Theme.Colors["accent"]; got != want {
		t.Errorf("Colors[accent] = %+v, want %+v", got, want)
	}
}

func TestColorOverride_UnmarshalTOML_TableSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want ColorOverride
	}{
		{
			name: "both modes",
			body: `[theme.colors]
danger = { dark = "#e6a8a2", light = "#8a2418" }
`,
			want: ColorOverride{Dark: "#e6a8a2", Light: "#8a2418"},
		},
		{
			name: "dark only",
			body: `[theme.colors]
danger = { dark = "#e6a8a2" }
`,
			want: ColorOverride{Dark: "#e6a8a2"},
		},
		{
			name: "light only",
			body: `[theme.colors]
danger = { light = "#8a2418" }
`,
			want: ColorOverride{Light: "#8a2418"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeStrict([]byte(tc.body))
			if err != nil {
				t.Fatalf("DecodeStrict: %v", err)
			}
			if got := cfg.Theme.Colors["danger"]; got != tc.want {
				t.Errorf("Colors[danger] = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestColorOverride_UnmarshalTOML_WrongShapeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "array", body: "[theme.colors]\naccent = [1, 2, 3]\n"},
		{name: "integer", body: "[theme.colors]\naccent = 5\n"},
		{name: "non-string dark", body: "[theme.colors]\naccent = { dark = 5 }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeStrict([]byte(tc.body)); err == nil {
				t.Error("DecodeStrict: got nil error, want a decode error for a malformed [theme.colors] entry")
			}
		})
	}
}

func TestThemeConfig_Validate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		theme   ThemeConfig
		wantErr string // substring; "" means Validate must return nil
	}{
		{name: "zero value"},
		{name: "preset artificer", theme: ThemeConfig{Preset: "artificer"}},
		{name: "preset legacy", theme: ThemeConfig{Preset: "legacy"}},
		{
			name:    "bad preset",
			theme:   ThemeConfig{Preset: "neon"},
			wantErr: `[theme].preset = "neon": must be "artificer" or "legacy"`,
		},
		{name: "mode auto", theme: ThemeConfig{Mode: "auto"}},
		{name: "mode dark", theme: ThemeConfig{Mode: "dark"}},
		{name: "mode light", theme: ThemeConfig{Mode: "light"}},
		{
			name:    "bad mode",
			theme:   ThemeConfig{Mode: "midnight"},
			wantErr: `[theme].mode = "midnight": must be "auto", "dark", or "light"`,
		},
		{
			name:    "unknown role",
			theme:   ThemeConfig{Colors: map[string]ColorOverride{"nonexistent": {Dark: "#123456"}}},
			wantErr: `[theme].colors["nonexistent"]: unknown role; roles are `,
		},
		{
			name:    "role matched case-insensitively",
			theme:   ThemeConfig{Colors: map[string]ColorOverride{"ACCENT": {Dark: "#123456"}}},
			wantErr: "",
		},
		{
			name:    "bad hex in dark",
			theme:   ThemeConfig{Colors: map[string]ColorOverride{"accent": {Dark: "not-a-color"}}},
			wantErr: `[theme].colors["accent"].dark = "not-a-color": must be a #rrggbb hex colour`,
		},
		{
			name:    "bad hex in light",
			theme:   ThemeConfig{Colors: map[string]ColorOverride{"accent": {Dark: "#123456", Light: "nope"}}},
			wantErr: `[theme].colors["accent"].light = "nope": must be a #rrggbb hex colour`,
		},
		{
			name:    "shorthand hex rejected",
			theme:   ThemeConfig{Colors: map[string]ColorOverride{"accent": {Dark: "#fff"}}},
			wantErr: `[theme].colors["accent"].dark = "#fff": must be a #rrggbb hex colour`,
		},
		{
			name:    "empty override",
			theme:   ThemeConfig{Colors: map[string]ColorOverride{"accent": {}}},
			wantErr: `[theme].colors["accent"]: no colour given`,
		},
		{
			name: "valid scalar-style override",
			theme: ThemeConfig{Colors: map[string]ColorOverride{
				"accent": {Dark: "#dbbb6f", Light: "#dbbb6f"},
			}},
		},
		{
			name: "valid per-mode override",
			theme: ThemeConfig{Colors: map[string]ColorOverride{
				"danger": {Dark: "#e6a8a2", Light: "#8a2418"},
			}},
		},
		{
			name: "first error is deterministic across multiple entries",
			theme: ThemeConfig{Colors: map[string]ColorOverride{
				"warn": {Dark: "#123456"},
				"bg":   {Dark: "bad-value"},
			}},
			wantErr: `[theme].colors["bg"].dark = "bad-value": must be a #rrggbb hex colour`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.theme.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestThemeConfig_Validate_UnknownRoleListsEveryRole is the completeness half
// of the unknown-role error: it must not just say "unknown", it must enumerate
// every valid spelling so an operator can fix the typo without reading source.
func TestThemeConfig_Validate_UnknownRoleListsEveryRole(t *testing.T) {
	theme := ThemeConfig{Colors: map[string]ColorOverride{"nope": {Dark: "#123456"}}}
	err := theme.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an unknown-role error")
	}
	for _, role := range ThemeRoleNames {
		if !strings.Contains(err.Error(), role) {
			t.Errorf("unknown-role error %q does not list role %q", err.Error(), role)
		}
	}
}

func TestThemeConfig_IsZero(t *testing.T) {
	if !(ThemeConfig{}).IsZero() {
		t.Error("an empty ThemeConfig must report as zero")
	}
	for name, tc := range map[string]ThemeConfig{
		"preset": {Preset: "artificer"},
		"mode":   {Mode: "dark"},
		"colors": {Colors: map[string]ColorOverride{"accent": {Dark: "#123456"}}},
	} {
		if tc.IsZero() {
			t.Errorf("a [theme] section setting only %s must not report as absent", name)
		}
	}
}

// TestValidatePath_SurfacesThemeError mirrors
// TestValidatePath_SurfacesRootKindsError: `launch doctor`'s strict decode
// must surface a bad [theme] value, not just DecodeStrict/Validate called
// directly.
func TestValidatePath_SurfacesThemeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[theme]\npreset = \"neon\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "theme") {
		t.Errorf("ValidatePath = %v, want a [theme] error", err)
	}
}

// TestLoad_TolerantOfBadTheme pins the "Load stays tolerant" requirement: a
// bad [theme] section must not make Load fail — only ValidatePath (used by
// doctor and launch doctor) reports it.
func TestLoad_TolerantOfBadTheme(t *testing.T) {
	body := "[theme]\npreset = \"neon\"\nmode = \"midnight\"\n"
	cfg, err := DecodeStrict([]byte(body))
	if err != nil {
		t.Fatalf("DecodeStrict (Load's decode path) returned an error for a semantically-invalid but syntactically-valid [theme]: %v", err)
	}
	if cfg.Theme.Preset != "neon" || cfg.Theme.Mode != "midnight" {
		t.Errorf("Theme = %+v, want the invalid values decoded as-is (Load tolerates semantics, only ValidatePath checks them)", cfg.Theme)
	}
}

// TestThemeRoleNames_ExactOrder pins the documented role list — operators
// write these as literal TOML keys, and the unknown-role error joins this
// slice verbatim, so both the order and the exact spellings are part of the
// contract.
func TestThemeRoleNames_ExactOrder(t *testing.T) {
	want := []string{
		"accent", "ok", "danger", "warn", "active", "muted", "meta", "dim", "fg", "steel",
		"brand", "surfaceraised", "accentfill", "onaccent", "urgentfill", "onurgent", "bg",
	}
	if !reflect.DeepEqual(ThemeRoleNames, want) {
		t.Errorf("ThemeRoleNames = %v, want %v", ThemeRoleNames, want)
	}
}
