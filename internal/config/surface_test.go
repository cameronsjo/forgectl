package config

import (
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
