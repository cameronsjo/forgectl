package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSurfaceDrainConfig_Resolve(t *testing.T) {
	n := func(v int) *int { return &v }
	off := false
	cases := map[string]struct {
		in      SurfaceDrainConfig
		want    DrainSettings
		wantErr []string
	}{
		"absent section takes every default": {in: SurfaceDrainConfig{}, want: DefaultDrainSettings()},
		"every field set": {
			in:   SurfaceDrainConfig{Interval: "30s", Cap: n(10), PerRepo: n(2), Notify: &off, IdleMinutes: n(45)},
			want: DrainSettings{Interval: 30 * time.Second, Cap: 10, PerRepo: 2, Notify: false, Idle: 45 * time.Minute},
		},
		"interval at the floor":     {in: SurfaceDrainConfig{Interval: "5s"}, want: withInterval(5 * time.Second)},
		"interval below the floor":  {in: SurfaceDrainConfig{Interval: "4s"}, wantErr: []string{"interval", "5s", "4s"}},
		"interval not a duration":   {in: SurfaceDrainConfig{Interval: "fast"}, wantErr: []string{"interval", "fast"}},
		"cap zero":                  {in: SurfaceDrainConfig{Cap: n(0)}, wantErr: []string{"cap", "1 to 10", "got 0"}},
		"cap above the max":         {in: SurfaceDrainConfig{Cap: n(11)}, wantErr: []string{"cap", "got 11"}},
		"per_repo zero":             {in: SurfaceDrainConfig{PerRepo: n(0)}, wantErr: []string{"per_repo", "got 0"}},
		"idle_minutes zero":         {in: SurfaceDrainConfig{IdleMinutes: n(0)}, wantErr: []string{"idle_minutes", "got 0"}},
		"idle_minutes past one day": {in: SurfaceDrainConfig{IdleMinutes: n(1441)}, wantErr: []string{"idle_minutes", "got 1441"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := c.in.Resolve()
			if len(c.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Resolve() = %+v, want an error", got)
				}
				for _, w := range c.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not name %q", err, w)
					}
				}
				if got != (DrainSettings{}) {
					t.Errorf("an invalid value returned settings %+v; it must never fall back to a default", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("Resolve() = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
}

func withInterval(d time.Duration) DrainSettings {
	s := DefaultDrainSettings()
	s.Interval = d
	return s
}

// TestValidatePath_SurfaceDrain pins that an out-of-range [surface.drain]
// value makes the file invalid for `launch doctor`, and a valid one does not.
func TestValidatePath_SurfaceDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[surface.drain]\ncap = 3\nper_repo = 1\ninterval = \"15s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err != nil {
		t.Fatalf("valid [surface.drain]: %v", err)
	}
	if err := os.WriteFile(path, []byte("[surface.drain]\ncap = 11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("cap = 11: err %v, want one naming cap", err)
	}
	cfg := LoadPath(path)
	if cfg.Surface.Drain.Cap == nil || *cfg.Surface.Drain.Cap != 11 {
		t.Fatalf("LoadPath dropped [surface.drain] cap: %+v", cfg.Surface.Drain)
	}
}

func TestSurfaceProfiles_Validate(t *testing.T) {
	cases := map[string]struct {
		profiles map[string]SurfaceProfile
		wantErr  []string
	}{
		"none":              {},
		"absolute":          {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "/Users/x/.claude-work"}}},
		"tilde":             {profiles: map[string]SurfaceProfile{"alt_2": {ConfigDir: "~/.claude-alt"}}},
		"relative path":     {profiles: map[string]SurfaceProfile{"work": {ConfigDir: ".claude-work"}}, wantErr: []string{"[surface.profiles.work] config_dir", "absolute", ".claude-work"}},
		"other user tilde":  {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "~bob/.claude"}}, wantErr: []string{"config_dir", "~bob"}},
		"empty config_dir":  {profiles: map[string]SurfaceProfile{"work": {}}, wantErr: []string{"config_dir"}},
		"main is reserved":  {profiles: map[string]SurfaceProfile{"main": {ConfigDir: "/x"}}, wantErr: []string{"main", "reserved"}},
		"home itself":       {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "~"}}, wantErr: []string{"config_dir", "home directory"}},
		"home with slash":   {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "~/"}}, wantErr: []string{"config_dir", "home directory"}},
		"home dot":          {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "~/."}}, wantErr: []string{"config_dir", "home directory"}},
		"root":              {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "/"}}, wantErr: []string{"config_dir", "root"}},
		"root, unclean":     {profiles: map[string]SurfaceProfile{"work": {ConfigDir: "//."}}, wantErr: []string{"config_dir", "root"}},
		"upper-case name":   {profiles: map[string]SurfaceProfile{"Work": {ConfigDir: "/x"}}, wantErr: []string{"profile name"}},
		"leading dash name": {profiles: map[string]SurfaceProfile{"-w": {ConfigDir: "/x"}}, wantErr: []string{"profile name"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := SurfaceConfig{Profiles: c.profiles}.ValidateProfiles()
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ValidateProfiles: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateProfiles accepted it")
			}
			for _, w := range c.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestSurfaceProfiles_ConfigDir(t *testing.T) {
	sc := SurfaceConfig{Profiles: map[string]SurfaceProfile{
		"work": {ConfigDir: "~/.claude-work"},
		"abs":  {ConfigDir: "/opt/claude/../claude-abs"},
	}}
	home := func() (string, error) { return "/home/op", nil }
	cases := map[string]struct {
		name, want string
		unknown    bool
	}{
		"no profile":        {name: "", want: ""},
		"main":              {name: "main", want: ""},
		"tilde expanded":    {name: "work", want: "/home/op/.claude-work"},
		"absolute, cleaned": {name: "abs", want: "/opt/claude-abs"},
		"unknown name":      {name: "nope", unknown: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := sc.ProfileConfigDir(c.name, home)
			if c.unknown {
				if !errors.Is(err, ErrUnknownProfile) || !strings.Contains(err.Error(), c.name) {
					t.Fatalf("ProfileConfigDir(%q) = %q, %v; want ErrUnknownProfile naming it", c.name, got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("ProfileConfigDir(%q) = %q, %v; want %q", c.name, got, err, c.want)
			}
		})
	}
	// ProfileConfigDir checks the expanded path again, for a table that
	// skipped ValidateProfiles.
	for _, dir := range []string{"~/sub/..", "/opt/.."} {
		raw := SurfaceConfig{Profiles: map[string]SurfaceProfile{"w": {ConfigDir: dir}}}
		if got, err := raw.ProfileConfigDir("w", home); err == nil {
			t.Errorf("config_dir %q resolved to %q; want the home directory and root refused", dir, got)
		}
	}
	if _, err := sc.ProfileConfigDir("work", func() (string, error) { return "relative-home", nil }); err == nil {
		t.Fatal("a config_dir that is not absolute after expanding ~ was accepted")
	}
}

// TestSurfaceProfiles_RefusedAtLoad pins that a bad [surface.profiles] entry
// is refused when the file loads, and the whole table is dropped, so no
// worker runs under a directory the loader refused.
func TestSurfaceProfiles_RefusedAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[surface.profiles.work]\nconfig_dir = \"~/.claude-work\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	if cfg := LoadPath(path); cfg.DecodeError() != nil || cfg.Surface.Profiles["work"].ConfigDir != "~/.claude-work" {
		t.Fatalf("LoadPath: %+v, %v", cfg.Surface, cfg.DecodeError())
	}
	if err := os.WriteFile(path, []byte("[surface.profiles.work]\nconfig_dir = \"rel/dir\"\n[surface.profiles.ok]\nconfig_dir = \"/abs\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "config_dir") {
		t.Fatalf("relative config_dir: ValidatePath %v, want an error naming config_dir", err)
	}
	cfg := LoadPath(path)
	if err := cfg.DecodeError(); err == nil || !strings.Contains(err.Error(), "is not valid") {
		t.Fatalf("LoadPath decode error %v, want the file reported invalid", err)
	}
	if cfg.Surface.Profiles != nil {
		t.Fatalf("an invalid [surface.profiles] kept entries: %+v", cfg.Surface.Profiles)
	}
}

func TestCheckModelName(t *testing.T) {
	for _, ok := range []string{"opus", "sonnet", "claude-opus-5-5", "opus[1m]", "gpt-5.1", "a_b"} {
		if err := CheckModelName(ok); err != nil {
			t.Errorf("CheckModelName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-p", "--dangerously-skip-permissions", "opus sonnet", "opus;rm", "a/b", "é", strings.Repeat("a", 65)} {
		if err := CheckModelName(bad); !errors.Is(err, ErrInvalidModel) {
			t.Errorf("CheckModelName(%q) = %v, want ErrInvalidModel", bad, err)
		}
	}
}

func TestSurfaceIntakeConfig_Resolve(t *testing.T) {
	s, err := SurfaceIntakeConfig{}.Resolve()
	if err != nil || len(s.Authors) != 0 || len(s.Labels) != 1 || s.Labels[0] != "queue:drain" || s.MaxPerRun != DefaultIntakeMaxPerRun {
		t.Fatalf("defaults: %+v, %v", s, err)
	}
	seven := 7
	s, err = SurfaceIntakeConfig{Authors: []string{"alice", "Bob-2"}, Labels: []string{"ready"}, MaxPerRun: &seven}.Resolve()
	if err != nil || len(s.Authors) != 2 || s.Labels[0] != "ready" || s.MaxPerRun != 7 {
		t.Fatalf("set: %+v, %v", s, err)
	}
	zero, big := 0, MaxIntakeMaxPerRun+1
	for name, c := range map[string]SurfaceIntakeConfig{
		"bot author":          {Authors: []string{"dependabot[bot]"}},
		"author with dash":    {Authors: []string{"-alice"}},
		"author too long":     {Authors: []string{strings.Repeat("a", 40)}},
		"empty author":        {Authors: []string{""}},
		"empty label list":    {Labels: []string{}},
		"padded label":        {Labels: []string{" ready"}},
		"control in label":    {Labels: []string{"re\x1bady"}},
		"label too long":      {Labels: []string{strings.Repeat("l", 51)}},
		"max_per_run zero":    {MaxPerRun: &zero},
		"max_per_run too big": {MaxPerRun: &big},
	} {
		if _, err := c.Resolve(); err == nil || !strings.Contains(err.Error(), "[surface.intake]") {
			t.Errorf("%s: %v, want a [surface.intake] error", name, err)
		}
	}
}

// TestSurfaceIntake_RefusedAtLoad pins that a bad [surface.intake] value
// makes the file invalid at load, and that the section is kept rather than
// dropped to defaults, which would widen who can write a brief.
func TestSurfaceIntake_RefusedAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[surface.intake]\nauthors = [\"alice\"]\nmax_per_run = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err != nil {
		t.Fatalf("valid intake: %v", err)
	}
	if cfg := LoadPath(path); cfg.DecodeError() != nil || cfg.Surface.Intake.Authors[0] != "alice" {
		t.Fatalf("LoadPath: %+v, %v", cfg.Surface.Intake, cfg.DecodeError())
	}
	if err := os.WriteFile(path, []byte("[surface.intake]\nauthors = [\"alice\", \"bot[bot]\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "[surface.intake] authors") {
		t.Fatalf("bot author: ValidatePath %v", err)
	}
	cfg := LoadPath(path)
	if err := cfg.DecodeError(); err == nil || !strings.Contains(err.Error(), "is not valid") {
		t.Fatalf("LoadPath decode error %v", err)
	}
	if _, err := cfg.Surface.Intake.Resolve(); err == nil {
		t.Fatal("the loaded section resolves; a caller that goes on would use it")
	}
}

// TestSurfaceIntake_UnknownKeyRefused pins security review N2: a misspelled
// key under [surface.intake] makes the file invalid instead of leaving its
// field absent, which would select the default labels.
func TestSurfaceIntake_UnknownKeyRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for name, body := range map[string]string{
		"misspelled key": "[surface.intake]\nlables = [\"ready\"]\n",
		"nested table":   "[surface.intake.labels_extra]\nx = 1\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePath(path); err == nil || !strings.Contains(err.Error(), "[surface.intake]: unknown key") {
			t.Errorf("%s: ValidatePath %v, want an unknown-key error", name, err)
		}
		cfg := LoadPath(path)
		if err := cfg.DecodeError(); err == nil || !strings.Contains(err.Error(), "is not valid") {
			t.Errorf("%s: LoadPath decode error %v, want the file reported invalid", name, err)
		}
		if _, err := cfg.Surface.Intake.Resolve(); err == nil {
			t.Errorf("%s: the loaded section resolves to defaults", name)
		}
	}
	// The correctly spelled key still loads.
	if err := os.WriteFile(path, []byte("[surface.intake]\nlabels = [\"ready\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(path); err != nil {
		t.Fatalf("valid intake: %v", err)
	}
}
