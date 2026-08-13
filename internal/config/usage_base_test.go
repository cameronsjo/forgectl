package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchUsageBase_XDGAbsoluteAndHomeFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	xdg := t.TempDir()
	t.Setenv(xdgStateHomeEnv, xdg)
	got, err := LaunchUsageBase()
	if err != nil {
		t.Fatalf("LaunchUsageBase with absolute XDG_STATE_HOME: %v", err)
	}
	if got != filepath.Clean(xdg) {
		t.Fatalf("XDG base = %q, want %q", got, filepath.Clean(xdg))
	}

	t.Setenv(xdgStateHomeEnv, "")
	got, err = LaunchUsageBase()
	if err != nil {
		t.Fatalf("LaunchUsageBase with empty XDG_STATE_HOME: %v", err)
	}
	want := filepath.Join(home, ".local", "state")
	if got != want {
		t.Fatalf("home fallback base = %q, want %q", got, want)
	}
}

func TestLaunchUsageBase_RejectsRelativeAndSymlinkBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv(xdgStateHomeEnv, "relative/state")
	if _, err := LaunchUsageBase(); !errors.Is(err, ErrUsageBaseUnsafe) {
		t.Fatalf("relative XDG_STATE_HOME error = %v, want ErrUsageBaseUnsafe", err)
	}

	t.Setenv(xdgStateHomeEnv, "~/state")
	if _, err := LaunchUsageBase(); !errors.Is(err, ErrUsageBaseUnsafe) {
		t.Fatalf("tilde XDG_STATE_HOME error = %v, want ErrUsageBaseUnsafe", err)
	}

	real := filepath.Join(home, "real-state")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "linked-state")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv(xdgStateHomeEnv, link)
	err := func() error { _, err := LaunchUsageBase(); return err }()
	if !errors.Is(err, ErrUsageBaseUnsafe) {
		t.Fatalf("symlinked XDG_STATE_HOME error = %v, want ErrUsageBaseUnsafe", err)
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("error text carries an escape byte: %q", err.Error())
	}
}

func TestLaunchUsageBase_RejectsUnsetHomeWithoutXDG(t *testing.T) {
	t.Setenv(xdgStateHomeEnv, "")
	t.Setenv("HOME", "")
	if _, err := LaunchUsageBase(); !errors.Is(err, ErrUsageBaseUnsafe) {
		t.Fatalf("unset HOME error = %v, want ErrUsageBaseUnsafe", err)
	}
}

func TestLaunchConfig_UsageStatsAbsentAndFalseStayDisabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want bool
	}{
		{"absent table", ``, false},
		{"empty table", "[launch]\n", false},
		{"explicit false", "[launch]\nusage_stats = false\n", false},
		{"explicit true", "[launch]\nusage_stats = true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := decodeConfigForTest(t, tc.toml)
			if cfg.Launch.UsageStats != tc.want {
				t.Fatalf("UsageStats = %v, want %v", cfg.Launch.UsageStats, tc.want)
			}
		})
	}
}

func TestLaunchConfig_ExplicitFalseRetainsLaunchTablePresence(t *testing.T) {
	cfg := decodeConfigForTest(t, "[launch]\nusage_stats = false\n")
	if !cfg.HasLaunchSection() {
		t.Fatal("explicit [launch] usage_stats = false must keep HasLaunchSection true")
	}
	if !cfg.Launch.IsZero() {
		t.Fatal("usage_stats = false must not make the launch section non-zero")
	}

	absent := decodeConfigForTest(t, "[bench]\ntelemetry = false\n")
	if absent.HasLaunchSection() {
		t.Fatal("absent [launch] must report HasLaunchSection false")
	}
}

func TestLaunchConfig_UsageStatsTrueMakesSectionNonZero(t *testing.T) {
	cfg := decodeConfigForTest(t, "[launch]\nusage_stats = true\n")
	if cfg.Launch.IsZero() {
		t.Fatal("usage_stats = true must make the launch section non-zero")
	}
}

// decodeConfigForTest decodes body through the real strict decoder, so table
// presence metadata (launchSet) is exercised rather than a hand-built struct.
func decodeConfigForTest(t *testing.T, body string) Config {
	t.Helper()
	cfg, err := DecodeStrict([]byte(body))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	return cfg
}
