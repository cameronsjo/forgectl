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
