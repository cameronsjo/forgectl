package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// The scaffold is where an operator meets this feature, so the disclosure it
// carries is part of the feature. Every encoded field must be named, along
// with the sensitivity, the retention, and how to delete the store.
func TestLaunchInit_ScaffoldDisclosesEveryRecordedField(t *testing.T) {
	for _, field := range []string{"schema_version", "ts", "event", "harness", "model", "session_mode", "posture"} {
		if !strings.Contains(launchScaffold, field) {
			t.Errorf("scaffold never names the recorded field %q", field)
		}
	}
	for _, promise := range []string{
		"usage_stats = false",
		"no network call",
		"launch-usage.jsonl",
		"launch-usage.jsonl.lock",
		"forgectl launch stats",
		"Deletion is permanent",
	} {
		if !strings.Contains(launchScaffold, promise) {
			t.Errorf("scaffold omits the disclosure %q", promise)
		}
	}
	// A scaffold that does not decode is a scaffold nobody can edit.
	cfg, err := config.DecodeStrict([]byte(launchScaffold))
	if err != nil {
		t.Fatalf("scaffold does not decode as TOML: %v", err)
	}
	if cfg.Launch.UsageStats {
		t.Fatal("the scaffold ships with collection enabled")
	}
	if !cfg.HasLaunchSection() {
		t.Fatal("the scaffold does not declare a [launch] section")
	}
}

func TestLaunchDoctor_DisabledIsHealthyAndCreatesNothing(t *testing.T) {
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)

	out := &strings.Builder{}
	if healthy := reportUsageStats(out, false); !healthy {
		t.Fatal("disabled collection reported as unhealthy; off is a legitimate choice")
	}
	if !strings.Contains(out.String(), "off") {
		t.Fatalf("doctor line = %q, want it to say collection is off", out.String())
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Fatalf("a disabled doctor created state: %v", err)
	}
}

func TestLaunchDoctor_EnabledInspectsWithoutCreating(t *testing.T) {
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)

	out := &strings.Builder{}
	if healthy := reportUsageStats(out, true); !healthy {
		t.Fatalf("an absent store reported as unhealthy: %q", out.String())
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Fatalf("doctor created the state base while merely inspecting: %v", err)
	}
	if !strings.Contains(out.String(), "nothing recorded yet") {
		t.Fatalf("doctor line = %q, want it to distinguish an empty store", out.String())
	}
}

// Reaching the store narrows a mode broader than forgectl's own, and an
// operator runs doctor precisely to learn that something widened it. The
// correction is fine; correcting it silently is not, so every tightened path
// must appear by name in the output.
func TestLaunchDoctor_ReportsEveryPermissionItTightened(t *testing.T) {
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)
	leaf := filepath.Join(base, "forgectl")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(leaf, "launch-usage.jsonl")
	if err := os.WriteFile(data, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod after creation: umask would otherwise trim the planted modes.
	if err := os.Chmod(data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(leaf, 0o777); err != nil {
		t.Fatal(err)
	}

	out := &strings.Builder{}
	if healthy := reportUsageStats(out, true); !healthy {
		t.Fatalf("a store doctor successfully tightened reported as unhealthy: %q", out.String())
	}
	for _, path := range []string{leaf, data} {
		if !strings.Contains(out.String(), path) {
			t.Errorf("doctor tightened %s without naming it: %q", path, out.String())
		}
	}
	if strings.Count(out.String(), "tightened") != 2 {
		t.Errorf("doctor output = %q, want one tightening line per widened path", out.String())
	}

	leafInfo, err := os.Lstat(leaf)
	if err != nil || leafInfo.Mode().Perm() != 0o700 {
		t.Fatalf("leaf mode = %v (err %v), want 0700", leafInfo, err)
	}
	dataInfo, err := os.Lstat(data)
	if err != nil || dataInfo.Mode().Perm() != 0o600 {
		t.Fatalf("data mode = %v (err %v), want 0600", dataInfo, err)
	}
}

// The inverse: an already-correct store must produce no tightening line at
// all, or the signal means nothing.
func TestLaunchDoctor_SaysNothingWhenNothingWasTightened(t *testing.T) {
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)
	leaf := filepath.Join(base, "forgectl")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaf, "launch-usage.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &strings.Builder{}
	if healthy := reportUsageStats(out, true); !healthy {
		t.Fatalf("a well-formed store reported as unhealthy: %q", out.String())
	}
	if strings.Contains(out.String(), "tightened") {
		t.Fatalf("doctor claimed a tightening on an already-correct store: %q", out.String())
	}
}

func TestLaunchDoctor_RefusedStoreIsUnhealthyAndNamed(t *testing.T) {
	base := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", base)
	leaf := filepath.Join(base, "forgectl")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(elsewhere, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(leaf, "launch-usage.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out := &strings.Builder{}
	if healthy := reportUsageStats(out, true); healthy {
		t.Fatalf("a symlinked data file reported as healthy: %q", out.String())
	}
	if !strings.Contains(out.String(), "refused") {
		t.Fatalf("doctor line = %q, want it to name the refusal", out.String())
	}
	info, err := os.Lstat(elsewhere)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("doctor touched the attacker's target: %v %v", info, err)
	}
}
