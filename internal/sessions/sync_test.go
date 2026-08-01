package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestRunbooksDirWithLegacyFallsBackOnlyWhenCurrentIsAbsent(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "state", "runbooks")
	legacy := filepath.Join(root, ".claude", "cadence", "runbooks")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := runbooksDirWithLegacy(SyncOptions{
		RunbooksDir:       current,
		LegacyRunbooksDir: legacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != legacy {
		t.Fatalf("absent current corpus should use legacy: got %q, want %q", got, legacy)
	}

	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = runbooksDirWithLegacy(SyncOptions{
		RunbooksDir:       current,
		LegacyRunbooksDir: legacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Fatalf("existing current corpus should win without merging: got %q, want %q", got, current)
	}
}

func TestRunbooksDirWithLegacyReturnsCurrentWhenBothAreAbsent(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "state", "runbooks")

	got, err := runbooksDirWithLegacy(SyncOptions{
		RunbooksDir:       current,
		LegacyRunbooksDir: filepath.Join(root, "legacy"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != current {
		t.Fatalf("missing corpora should retain current path: got %q, want %q", got, current)
	}
}

// TestExpandTilde covers the raw expansion logic in isolation — no home
// directory resolution, no filesystem, no Resolve wiring. Mirrors the three
// sibling copies (internal/config, internal/launch, internal/clean).
func TestExpandTilde(t *testing.T) {
	cases := []struct {
		name string
		path string
		home string
		want string
	}{
		{"bare tilde expands to home", "~", "/home/u", "/home/u"},
		{"tilde slash joins onto home", "~/foo/bar", "/home/u", "/home/u/foo/bar"},
		{"tilde-user is left untouched", "~otheruser/x", "/home/u", "~otheruser/x"},
		{"absolute path is unchanged", "/abs/path", "/home/u", "/abs/path"},
		{"relative path is unchanged, not absolutised", "./metrics", "/home/u", "./metrics"},
		{"empty home is a no-op", "~/foo", "", "~/foo"},
		{"empty path stays empty", "", "/home/u", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandTilde(tc.path, tc.home); got != tc.want {
				t.Fatalf("expandTilde(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
			}
		})
	}
}

// fixtureHome creates a temp HOME and pins CADENCE_STATE_HOME under it so
// Resolve's baked-default derivation is deterministic regardless of the
// real environment.
func fixtureHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CADENCE_STATE_HOME", filepath.Join(home, "state", "cadence"))
	return home
}

func TestResolve_MetricsDirTildeExpansion(t *testing.T) {
	home := fixtureHome(t)
	want := filepath.Join(home, ".claude", "metrics")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := SyncOptions{}.Resolve(config.SessionsConfig{MetricsDir: "~/.claude/metrics"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MetricsDir != want {
		t.Fatalf("MetricsDir = %q, want %q", got.MetricsDir, want)
	}
}

func TestResolve_RunbooksDirTildeExpansion(t *testing.T) {
	home := fixtureHome(t)
	want := filepath.Join(home, ".claude", "runbooks")

	got, err := SyncOptions{}.Resolve(config.SessionsConfig{RunbooksDir: "~/.claude/runbooks"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunbooksDir != want {
		t.Fatalf("RunbooksDir = %q, want %q", got.RunbooksDir, want)
	}
}

// TestResolve_MetricsDirAbsoluteUnchanged is a CONTROL: an already-absolute
// configured path must pass through byte-identical, not get re-joined onto
// home.
func TestResolve_MetricsDirAbsoluteUnchanged(t *testing.T) {
	fixtureHome(t)
	abs := t.TempDir() // exists, so the loudness check doesn't fire

	got, err := SyncOptions{}.Resolve(config.SessionsConfig{MetricsDir: abs})
	if err != nil {
		t.Fatal(err)
	}
	if got.MetricsDir != abs {
		t.Fatalf("MetricsDir = %q, want byte-identical %q", got.MetricsDir, abs)
	}
}

// TestResolve_MetricsDirEmptyConfigKeepsDefault is a CONTROL: an absent
// [sessions] metrics_dir key must still fall through to the built-in
// state-home default — expansion must not disturb the empty-key path.
func TestResolve_MetricsDirEmptyConfigKeepsDefault(t *testing.T) {
	home := fixtureHome(t)
	want := filepath.Join(home, "state", "cadence", "metrics") // CADENCE_STATE_HOME set by fixtureHome

	got, err := SyncOptions{}.Resolve(config.SessionsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got.MetricsDir != want {
		t.Fatalf("MetricsDir = %q, want %q", got.MetricsDir, want)
	}
}

func TestResolve_FlagSuppliedMetricsDirWinsAndExpands(t *testing.T) {
	home := fixtureHome(t)
	want := filepath.Join(home, "flagdir")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := SyncOptions{MetricsDir: "~/flagdir"}.Resolve(config.SessionsConfig{MetricsDir: "~/configdir"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MetricsDir != want {
		t.Fatalf("MetricsDir = %q, want flag value %q to win and expand", got.MetricsDir, want)
	}
}

func TestResolve_FlagSuppliedRunbooksDirWinsAndExpands(t *testing.T) {
	home := fixtureHome(t)
	want := filepath.Join(home, "flagbooks")

	got, err := SyncOptions{RunbooksDir: "~/flagbooks"}.Resolve(config.SessionsConfig{RunbooksDir: "~/configbooks"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunbooksDir != want {
		t.Fatalf("RunbooksDir = %q, want flag value %q to win and expand", got.RunbooksDir, want)
	}
}

// TestResolve_MetricsDirExplicitAbsent_Errors is the loudness arm: an
// explicitly configured metrics_dir that doesn't exist on disk (a typo, or a
// path that moved) must fail loudly instead of silently syncing zero
// sessions.
func TestResolve_MetricsDirExplicitAbsent_Errors(t *testing.T) {
	home := fixtureHome(t)
	missing := filepath.Join(home, "nonexistent-metrics")

	_, err := SyncOptions{}.Resolve(config.SessionsConfig{MetricsDir: missing})
	if err == nil {
		t.Fatal("expected an error for an explicitly configured, absent metrics_dir")
	}
	if !strings.Contains(err.Error(), "metrics_dir") {
		t.Fatalf("error %q does not name the metrics_dir key", err.Error())
	}
}

// TestResolve_MetricsDirDefaultAbsent_NoError is the loudness CONTROL: the
// baked-default metrics dir staying absent (the fresh-machine case, no
// config, no flag) must NOT error.
func TestResolve_MetricsDirDefaultAbsent_NoError(t *testing.T) {
	fixtureHome(t) // baked default under CADENCE_STATE_HOME is never created

	_, err := SyncOptions{}.Resolve(config.SessionsConfig{})
	if err != nil {
		t.Fatalf("baked-default absent metrics_dir must not error, got: %v", err)
	}
}
