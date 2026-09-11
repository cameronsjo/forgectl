package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// runTheme executes `theme <args...>` over deps and returns stdout.
func runTheme(t *testing.T, deps module.Deps, args ...string) string {
	t.Helper()
	cmd := newThemeCmd(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("theme %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestThemeShowJSON_CoversEveryRole pins that the machine-readable report is
// complete and self-consistent. An agent reading this is the second audience
// for the whole package, and a role missing from it is a colour nobody can
// look up.
func TestThemeShowJSON_CoversEveryRole(t *testing.T) {
	out := runTheme(t, module.Deps{}, "show", "--json")

	var rep themeShowJSON
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
	}

	names := theme.RoleNames()
	if len(rep.Roles) != len(names) {
		t.Fatalf("report has %d roles, want %d", len(rep.Roles), len(names))
	}
	for i, want := range names {
		got := rep.Roles[i]
		if got.Role != want {
			t.Errorf("role %d is %q, want %q — the report must follow RoleNames order", i, got.Role, want)
		}
		if !strings.HasPrefix(got.Hex, "#") || len(got.Hex) != 7 {
			t.Errorf("role %q has hex %q, want #rrggbb", got.Role, got.Hex)
		}
		if got.Contrast <= 0 {
			t.Errorf("role %q has contrast %v, want a positive ratio", got.Role, got.Contrast)
		}
	}
	// Warnings must be an array, never null — a consumer should be able to
	// range over it without a nil check.
	if !strings.Contains(out, `"warnings": []`) {
		t.Errorf("warnings should serialise as [] when empty, got:\n%s", out)
	}
}

// TestThemeShowJSON_ZeroDepsResolvesDark is the end-to-end half of the
// zero-value orientation: a Deps nobody populated must still report the dark
// Artificer palette, not a light one and not empty strings.
func TestThemeShowJSON_ZeroDepsResolvesDark(t *testing.T) {
	var rep themeShowJSON
	if err := json.Unmarshal([]byte(runTheme(t, module.Deps{}, "show", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Dark {
		t.Error("a zero-value Deps.Theme reported LIGHT; forgectl is dark-first and the zero value must be dark")
	}
	if rep.Preset != "artificer" {
		t.Errorf("preset = %q, want artificer", rep.Preset)
	}
}

// TestThemeShow_ReportsOverrideProvenance pins that show distinguishes a
// configured colour from a preset one. Without it the command answers "what
// colour" but not "why", which is the half an operator debugging their own
// config actually needs.
func TestThemeShow_ReportsOverrideProvenance(t *testing.T) {
	th := themeFromTOML(t, config.ThemeConfig{
		Mode:   "dark",
		Colors: map[string]config.ColorOverride{"accent": {Dark: "#abcdef", Light: "#abcdef"}},
	})

	var rep themeShowJSON
	if err := json.Unmarshal([]byte(runTheme(t, module.Deps{Theme: th}, "show", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	for _, row := range rep.Roles {
		switch row.Role {
		case "accent":
			if row.Hex != "#abcdef" {
				t.Errorf("accent = %q, want the override #abcdef", row.Hex)
			}
			if row.Provenance != "override" {
				t.Errorf("accent provenance = %q, want override", row.Provenance)
			}
		case "fg":
			if row.Provenance != "artificer" {
				t.Errorf("fg provenance = %q, want artificer — only the overridden role is an override", row.Provenance)
			}
		}
	}
}

// TestThemeShow_WarnsOnAnInertOverride pins the one case that is otherwise
// invisible: an override naming the mode the theme did NOT resolve to is
// silently ignored, looks correct in config.toml, and leaves the operator
// comparing their hex against a different colour on screen.
func TestThemeShow_WarnsOnAnInertOverride(t *testing.T) {
	th := themeFromTOML(t, config.ThemeConfig{
		Mode:   "dark",
		Colors: map[string]config.ColorOverride{"accent": {Light: "#abcdef"}},
	})

	var rep themeShowJSON
	if err := json.Unmarshal([]byte(runTheme(t, module.Deps{Theme: th}, "show", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("a light-only override on a dark-resolved theme produced no warning; it is inert and nothing else would say so")
	}
	if !strings.Contains(rep.Warnings[0], "accent") {
		t.Errorf("warning should name the role: %q", rep.Warnings[0])
	}
}

// TestThemePreview_RendersEveryRole keeps preview honest: it is the command
// whose entire job is to show every colour, so a role missing from it is the
// one defect it cannot have.
func TestThemePreview_RendersEveryRole(t *testing.T) {
	out := runTheme(t, module.Deps{}, "preview")
	for _, name := range theme.RoleNames() {
		if !strings.Contains(out, name) {
			t.Errorf("preview omits role %q", name)
		}
	}
}

// TestThemeOutput_IsTerminalInert runs both verbs through the shared
// inertness contract. These commands write more escape sequences than anything
// else in the binary by design, which is exactly why the boundary is worth
// asserting here rather than assumed.
func TestThemeOutput_IsTerminalInert(t *testing.T) {
	termsafetest.AssertInert(t, "theme show", runTheme(t, module.Deps{}, "show"))
	termsafetest.AssertInert(t, "theme preview", runTheme(t, module.Deps{}, "preview"))
}

// themeFromTOML builds a Theme the way production does — through FromConfig
// and Detect — so a test cannot accidentally assert against a path the binary
// never takes.
func themeFromTOML(t *testing.T, c config.ThemeConfig) theme.Theme {
	t.Helper()
	opts, err := theme.FromConfig(c)
	if err != nil {
		t.Fatalf("FromConfig(%+v): %v", c, err)
	}
	return theme.New(opts, theme.Detect(opts.Mode, theme.Env{}, nil))
}

// TestResolveTheme_FallsBackOnBadConfig pins the startup path's refusal to
// fail the run over a colour.
//
// A bad [theme] is reported by doctor and launch doctor through ValidatePath;
// refusing to start here would trade a cosmetic problem for an unusable
// binary. The review that prompted this test noted resolveTheme had no
// coverage at all, so nothing would have caught a regression that panicked,
// silently used zero Options, or dropped the warning.
func TestResolveTheme_FallsBackOnBadConfig(t *testing.T) {
	bad := config.Config{Theme: config.ThemeConfig{Preset: "chartreuse"}}

	th := resolveTheme(bad)

	if !th.IsDark() {
		t.Error("the fallback theme is not dark; Default() must be used on the error path")
	}
	if th.PresetName() != "artificer" {
		t.Errorf("fallback preset = %q, want artificer", th.PresetName())
	}
	// The fallback must be a WORKING theme, not a zero Options that happens to
	// answer IsDark — a role has to resolve to a real colour.
	if hex := th.Hex(theme.Role(0)); len(hex) != 7 || hex[0] != '#' {
		t.Errorf("fallback theme resolved %q for the first role; want a #rrggbb colour", hex)
	}
}

// TestResolveTheme_HonoursAGoodConfig is the other half: the fallback above
// must not be what every config gets.
func TestResolveTheme_HonoursAGoodConfig(t *testing.T) {
	good := config.Config{Theme: config.ThemeConfig{
		Mode:   "light",
		Colors: map[string]config.ColorOverride{"accent": {Dark: "#abcdef", Light: "#abcdef"}},
	}}

	th := resolveTheme(good)

	if th.IsDark() {
		t.Error(`mode = "light" did not reach the resolved theme`)
	}
	if got := th.Hex(theme.Role(0)); got != "#abcdef" {
		t.Errorf("accent = %q, want the configured #abcdef", got)
	}
}
