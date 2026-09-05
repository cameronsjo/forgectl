package theme

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestShouldProbe(t *testing.T) {
	tests := []struct {
		name string
		env  Env
		want bool
	}{
		{"fully interactive", Env{StdinTTY: true, StdoutTTY: true, Term: "xterm-256color"}, true},
		{"no stdin tty", Env{StdinTTY: false, StdoutTTY: true, Term: "xterm"}, false},
		{"no stdout tty", Env{StdinTTY: true, StdoutTTY: false, Term: "xterm"}, false},
		{"no color", Env{StdinTTY: true, StdoutTTY: true, Term: "xterm", NoColor: true}, false},
		{"screen term", Env{StdinTTY: true, StdoutTTY: true, Term: "screen-256color"}, false},
		{"tmux term", Env{StdinTTY: true, StdoutTTY: true, Term: "tmux-256color"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldProbe(ModeAuto, tt.env); got != tt.want {
				t.Errorf("ShouldProbe(%+v) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	interactive := Env{StdinTTY: true, StdoutTTY: true, Term: "xterm-256color"}
	notInteractive := Env{StdinTTY: true, StdoutTTY: true, Term: "tmux-256color"}

	tests := []struct {
		name      string
		mode      Mode
		env       Env
		useProbe  bool
		probeRet  bool
		wantDark  bool
		wantCalls int
	}{
		{"dark fixed, no probe", ModeDark, interactive, true, false, true, 0},
		{"light fixed, no probe", ModeLight, interactive, true, true, false, 0},
		{"auto, unsafe env, no probe, defaults dark", ModeAuto, notInteractive, true, false, true, 0},
		{"auto, nil probe, defaults dark", ModeAuto, interactive, false, false, true, 0},
		{"auto, safe env, probes and trusts a false result", ModeAuto, interactive, true, false, false, 1},
		{"auto, safe env, probes and trusts a true result", ModeAuto, interactive, true, true, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			var probe func() bool
			if tt.useProbe {
				probe = func() bool {
					calls++
					return tt.probeRet
				}
			}
			got := Detect(tt.mode, tt.env, probe)
			if got != tt.wantDark {
				t.Errorf("Detect() = %v, want %v", got, tt.wantDark)
			}
			if calls != tt.wantCalls {
				t.Errorf("probe called %d times, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.ThemeConfig
		wantErr string
		check   func(t *testing.T, o Options)
	}{
		{
			name: "zero value",
			cfg:  config.ThemeConfig{},
			check: func(t *testing.T, o Options) {
				if o.Preset != "" || o.Mode != ModeAuto || len(o.Overrides) != 0 {
					t.Errorf("zero ThemeConfig -> %+v, want zero Options with ModeAuto", o)
				}
			},
		},
		{
			name: "artificer preset, dark mode",
			cfg:  config.ThemeConfig{Preset: "artificer", Mode: "dark"},
			check: func(t *testing.T, o Options) {
				if o.Preset != "artificer" || o.Mode != ModeDark {
					t.Errorf("got %+v", o)
				}
			},
		},
		{
			name: "legacy preset, light mode",
			cfg:  config.ThemeConfig{Preset: "legacy", Mode: "light"},
			check: func(t *testing.T, o Options) {
				if o.Preset != "legacy" || o.Mode != ModeLight {
					t.Errorf("got %+v", o)
				}
			},
		},
		{
			name:    "unknown preset",
			cfg:     config.ThemeConfig{Preset: "neon"},
			wantErr: `unknown preset "neon"`,
		},
		{
			name:    "unknown mode",
			cfg:     config.ThemeConfig{Mode: "midnight"},
			wantErr: `unknown mode "midnight"`,
		},
		{
			name: "color override, both modes",
			cfg: config.ThemeConfig{
				Colors: map[string]config.ColorOverride{"accent": {Dark: "#111111", Light: "#222222"}},
			},
			check: func(t *testing.T, o Options) {
				if got := o.Overrides[RoleAccent]; got != (Pair{Dark: "#111111", Light: "#222222"}) {
					t.Errorf("Overrides[RoleAccent] = %+v", got)
				}
			},
		},
		{
			name: "color override, dark only",
			cfg: config.ThemeConfig{
				Colors: map[string]config.ColorOverride{"danger": {Dark: "#111111"}},
			},
			check: func(t *testing.T, o Options) {
				if got := o.Overrides[RoleDanger]; got != (Pair{Dark: "#111111"}) {
					t.Errorf("Overrides[RoleDanger] = %+v", got)
				}
			},
		},
		{
			name: "color override, role matched case-insensitively",
			cfg: config.ThemeConfig{
				Colors: map[string]config.ColorOverride{"ACCENT": {Dark: "#111111"}},
			},
			check: func(t *testing.T, o Options) {
				if _, ok := o.Overrides[RoleAccent]; !ok {
					t.Error("Overrides missing RoleAccent for an uppercase key")
				}
			},
		},
		{
			name:    "unknown role",
			cfg:     config.ThemeConfig{Colors: map[string]config.ColorOverride{"nonexistent": {Dark: "#111111"}}},
			wantErr: `unknown role "nonexistent"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := FromConfig(tt.cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("FromConfig() = nil error, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("FromConfig() error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromConfig(): %v", err)
			}
			tt.check(t, o)
		})
	}
}

// TestTheme_ZeroValueMatchesDefault pins the load-bearing property that lets
// module.Deps.Theme be a zero value in tests that never set it: every method
// on Theme{} must behave exactly like Default().
func TestTheme_ZeroValueMatchesDefault(t *testing.T) {
	var zero Theme
	def := Default()

	if zero.IsDark() != def.IsDark() {
		t.Errorf("IsDark: zero=%v default=%v", zero.IsDark(), def.IsDark())
	}
	if zero.Mode() != def.Mode() {
		t.Errorf("Mode: zero=%v default=%v", zero.Mode(), def.Mode())
	}
	for _, r := range []Role{RoleAccent, RoleOK, RoleDanger, RoleWarn, RoleActive, RoleMuted, RoleMeta,
		RoleDim, RoleFg, RoleSteel, RoleBrand, RoleSurfaceRaised, RoleAccentFill, RoleOnAccent,
		RoleUrgentFill, RoleOnUrgent, RoleBg} {
		if zero.Hex(r) != def.Hex(r) {
			t.Errorf("Hex(%v): zero=%q default=%q", r, zero.Hex(r), def.Hex(r))
		}
		if zero.Contrast(r) != def.Contrast(r) {
			t.Errorf("Contrast(%v): zero=%v default=%v", r, zero.Contrast(r), def.Contrast(r))
		}
	}
	if !reflect.DeepEqual(zero.Styles(), def.Styles()) {
		t.Error("Styles() differ between zero value and Default()")
	}
	if zero.Marks() != def.Marks() {
		t.Error("Marks() differ between zero value and Default()")
	}
	if zero.Huh().Theme(true) == nil || zero.Huh().Theme(false) == nil {
		t.Error("Huh() returned a nil-rendering theme on the zero value")
	}
	if zero.Fang()(nil) != def.Fang()(nil) {
		t.Error("Fang() differs between zero value and Default()")
	}
	// list.Styles and fang.ColorScheme are not comparable with == reliably in
	// every field (lipgloss.Style embeds internal slices); reflect.DeepEqual
	// covers List(), and Fang() is compared field-by-field via its lipgloss
	// Style — a full lipgloss.Style is fine under DeepEqual since it holds no
	// funcs beyond what's already dereferenced by evaluation.
	if !reflect.DeepEqual(zero.List(), def.List()) {
		t.Error("List() differs between zero value and Default()")
	}
}

func TestTheme_WithDark(t *testing.T) {
	th := New(Options{}, false)
	if th.IsDark() {
		t.Fatal("expected light")
	}
	dark := th.WithDark(true)
	if !dark.IsDark() {
		t.Error("WithDark(true) did not flip IsDark")
	}
	if th.IsDark() {
		t.Error("WithDark mutated the receiver")
	}
}

func TestTheme_LegacyPreset(t *testing.T) {
	th := New(Options{Preset: "legacy"}, true)
	if th.Hex(RoleAccent) != legacyAccent {
		t.Errorf("legacy accent = %q, want %q", th.Hex(RoleAccent), legacyAccent)
	}
}

func TestTheme_Overrides(t *testing.T) {
	th := New(Options{Overrides: map[Role]Pair{RoleAccent: {Dark: "#123456"}}}, true)
	if got := th.Hex(RoleAccent); got != "#123456" {
		t.Errorf("Hex(RoleAccent) = %q, want overridden #123456", got)
	}
	light := th.WithDark(false)
	if got := light.Hex(RoleAccent); got == "#123456" {
		t.Error("a dark-only override leaked into light mode")
	}
}

// TestZeroValueIsDark pins the orientation of the zero value, which is the
// whole reason Theme stores isLight rather than isDark.
//
// forgectl is dark-first, and module.Deps.Theme is a zero value in every test
// and on any path that forgets to set it. A zero value resolving LIGHT would
// render light-on-dark exactly the way huh v2 does — huh's own dark flag
// zero-values to false, which is why internal/keymap has to pin it. Storing
// the inverse makes the safe state the state you get by doing nothing.
func TestZeroValueIsDark(t *testing.T) {
	if !(Theme{}).IsDark() {
		t.Error("zero-value Theme resolves LIGHT; it must resolve dark (see the isLight field comment)")
	}
	if !Default().IsDark() {
		t.Error("Default() resolves LIGHT; it must resolve dark")
	}
	// And the inversion must not have broken the explicit path.
	if New(Options{}, false).IsDark() {
		t.Error("New(o, false) reports dark; the isDark argument was inverted twice")
	}
	if !New(Options{}, true).IsDark() {
		t.Error("New(o, true) reports light")
	}
	if (Theme{}).WithDark(false).IsDark() {
		t.Error("WithDark(false) reports dark")
	}
	// The hex must actually follow the mode, not just the flag.
	if darkHex, lightHex := New(Options{}, true).Hex(RoleFg), New(Options{}, false).Hex(RoleFg); darkHex == lightHex {
		t.Errorf("Fg is %q in both modes; the mode is not reaching palette resolution", darkHex)
	}
}
