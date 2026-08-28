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

	stdout, stderr, err := h.exec("migrate")
	if err == nil {
		t.Fatal("`launch migrate` succeeded with no claunch.conf present, want a non-zero exit")
	}
	sibling := filepath.Join(filepath.Dir(legacyConfigPath(h.base)), "config.toml")
	// The full path is asserted on stdout, where the styled error renderer
	// cannot title-case it into something uncopyable (#418 review).
	if !strings.Contains(stdout, sibling) {
		t.Errorf("stdout = %q, want it to name the full path %q", stdout, sibling)
	}
	if !strings.Contains(stdout, "cannot migrate it") {
		t.Errorf("stdout = %q, want it to say forgectl cannot migrate the file", stdout)
	}
	// The absent-file wording must be gone from the message itself, not merely
	// reshaped by the renderer — asserting the lowercase form alone passed
	// against a message that literally opened with it (#418 review).
	both := strings.ToLower(stdout + stderr)
	if strings.Contains(both, "no legacy claunch.conf found") {
		t.Errorf("output = %q, a present-but-unmigratable file must not be reported as absent", both)
	}
	// Probed, never read: the file stays exactly as written.
	got, err := os.ReadFile(sibling) //nolint:gosec // a path this test just constructed under t.TempDir
	if err != nil || !strings.Contains(string(got), "opus") {
		t.Fatalf("sibling config.toml = %q, error %v; want it untouched", got, err)
	}
}

func TestIntegration_LaunchDoctor_NamesAnUnmigratableSibling(t *testing.T) {
	h := unmigratableSiblingHarness(t)

	stdout, _ := h.run(t, "doctor")

	sibling := filepath.Join(filepath.Dir(legacyConfigPath(h.base)), "config.toml")
	if !strings.Contains(stdout, sibling) {
		t.Errorf("doctor stdout = %q, want it to name the full path %q", stdout, sibling)
	}
	if !strings.Contains(stdout, "cannot migrate it") {
		t.Errorf("doctor stdout = %q, want it to say forgectl cannot migrate the file", stdout)
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

// TestIntegration_Launch_HostileUndecodedKeyIsNeutralized closes the #417
// refusal's render loop end to end: an undecoded key name is attacker-
// influenceable content read out of a config file, and it now reaches the
// terminal on three surfaces. A bidi override decodes cleanly through TOML (it
// is not a TOML control character), so termsafe is the only thing standing
// between the file and the operator's terminal.
func TestIntegration_Launch_HostileUndecodedKeyIsNeutralized(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "[defaults]\nmodel = \"opus\"\n\"own\u202eresrever\" = 1\n")

	stdout, stderr := h.run(t, "which")
	both := stdout + stderr

	if strings.ContainsRune(both, '\u202e') {
		t.Errorf("a raw bidi override reached the terminal unescaped; output = %q", both)
	}
	// Neutralized, not dropped — the operator still has to be able to find the
	// key in their file.
	if !strings.Contains(both, "resrever") {
		t.Errorf("output = %q, want the key still identifiable after escaping", both)
	}
	if _, err := os.Stat(legacyConfigPath(h.base)); err != nil {
		t.Fatalf("legacy claunch.conf was retired despite the refusal: %v", err)
	}
}

// TestIntegration_LaunchDoctor_WhollyUnrepresentableLegacy_Warns covers the
// input class the first doctor test missed. That fixture carried a modelled
// `model` key, so `lc` was non-zero and doctor took its `default` arm — the
// one arm that prints the notice. A legacy file forgectl models *nothing* of
// leaves `lc` zero, takes the "no launch profiles configured" arm, and
// reported exactly that while a refused claunch.conf sat right there: the same
// defect the sibling probe exists to fix, one filename over.
func TestIntegration_LaunchDoctor_WhollyUnrepresentableLegacy_Warns(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "[gateway]\ntoken = \"y\"\n")

	stdout, _ := h.run(t, "doctor")

	if !strings.Contains(stdout, "gateway") {
		t.Errorf("doctor stdout = %q, want it to name the fields forgectl cannot represent", stdout)
	}
	if _, err := os.Stat(legacyConfigPath(h.base)); err != nil {
		t.Fatalf("legacy claunch.conf was retired despite the refusal: %v", err)
	}
}
