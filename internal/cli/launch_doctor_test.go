package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unmigratableSiblingHarness builds a claunch directory holding a config.toml
// and no claunch.conf — the shape forgectl#417 reports. forgectl migrates the
// historical claunch.conf format only, so this file is neither a migration
// source nor absent, and reporting it as absent sends the operator looking for
// a file that is right there.
func unmigratableSiblingHarness(t *testing.T) *harness {
	t.Helper()
	h := newLegacyHarnessWithBody(t, "")
	legacyPath := legacyConfigPath(h.base)
	if err := os.Remove(legacyPath); err != nil {
		t.Fatalf("remove claunch.conf: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(legacyPath), "config.toml")
	if err := os.WriteFile(sibling, []byte("[defaults]\nmodel = \"opus\"\n"), 0o600); err != nil {
		t.Fatalf("write sibling config.toml: %v", err)
	}
	return h
}

func TestIntegration_LaunchMigrate_NamesAnUnmigratableSibling(t *testing.T) {
	h := unmigratableSiblingHarness(t)

	stderr, err := h.runExpectErr(t, nil, "migrate")
	if err == nil {
		t.Fatal("`launch migrate` succeeded with no claunch.conf present, want an error")
	}
	if !strings.Contains(stderr, "config.toml") {
		t.Errorf("stderr = %q, want it to name the config.toml sitting in the claunch directory", stderr)
	}
	if strings.Contains(stderr, "no legacy claunch.conf found") {
		t.Errorf("stderr = %q, a present-but-unmigratable file must not be reported as absent", stderr)
	}
	// Probed, never read: the file stays exactly as written.
	sibling := filepath.Join(filepath.Dir(legacyConfigPath(h.base)), "config.toml")
	got, err := os.ReadFile(sibling) //nolint:gosec // a path this test just constructed under t.TempDir
	if err != nil || !strings.Contains(string(got), "opus") {
		t.Fatalf("sibling config.toml = %q, error %v; want it untouched", got, err)
	}
}

func TestIntegration_LaunchDoctor_NamesAnUnmigratableSibling(t *testing.T) {
	h := unmigratableSiblingHarness(t)

	stdout, _ := h.run(t, "doctor")

	if !strings.Contains(stdout, "config.toml") {
		t.Errorf("doctor stdout = %q, want it to name the config.toml sitting in the claunch directory", stdout)
	}
	if !strings.Contains(stdout, "claunch") {
		t.Errorf("doctor stdout = %q, want it to say which directory the file is in", stdout)
	}
}

// TestIntegration_LaunchDoctor_SilentWithoutASibling is the control: without
// it the assertions above could pass against an implementation that prints the
// line unconditionally.
func TestIntegration_LaunchDoctor_SilentWithoutASibling(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "")
	if err := os.Remove(legacyConfigPath(h.base)); err != nil {
		t.Fatalf("remove claunch.conf: %v", err)
	}

	stdout, _ := h.run(t, "doctor")

	if strings.Contains(stdout, "cannot migrate") {
		t.Errorf("doctor stdout = %q, want no unmigratable-sibling line when there is no sibling", stdout)
	}
}

// TestIntegration_LaunchDoctor_UnrepresentableLegacy_Warns closes the one
// surface the #417 refusal was otherwise unpinned on. `which` and `migrate`
// both have their own tests; doctor reaches the same refusal through a
// different print path (its `notice` line), so a change that dropped the
// notice there would go unnoticed everywhere else.
func TestIntegration_LaunchDoctor_UnrepresentableLegacy_Warns(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "[defaults]\nmodel = \"opus\"\nunknown_field = \"x\"\n")

	stdout, _ := h.run(t, "doctor")

	if !strings.Contains(stdout, "unknown_field") {
		t.Errorf("doctor stdout = %q, want it to name the field forgectl cannot represent", stdout)
	}
	if _, err := os.Stat(legacyConfigPath(h.base)); err != nil {
		t.Fatalf("legacy claunch.conf was retired by `doctor` despite the refusal: %v", err)
	}
}
