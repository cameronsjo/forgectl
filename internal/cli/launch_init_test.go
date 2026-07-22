package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newLegacyHarnessWithBody is newLegacyHarness but lets the caller supply the
// legacy claunch.conf body directly, so tests can exercise a malformed or
// empty legacy file (newLegacyHarness itself always writes a valid, non-zero
// config). config.toml has no [launch] section, matching newLegacyHarness's
// posture, so `launch init --from-claunch` reaches runClaunchImport's legacy
// decode branch rather than short-circuiting on the "already has [launch]"
// refusal.
func newLegacyHarnessWithBody(t *testing.T, legacyBody string) *harness {
	t.Helper()

	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve symlinks on temp cwd: %v", err)
	}
	binDir := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "claude.out")
	base := t.TempDir()

	writeStubClaude(t, binDir)

	cfgPath := childConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte("log_level = \"off\"\n"), 0o644); err != nil {
		t.Fatalf("write config.toml (no [launch] section): %v", err)
	}

	legacyPath := legacyConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir legacy config dir: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacyBody), 0o644); err != nil {
		t.Fatalf("write legacy claunch.conf: %v", err)
	}

	return &harness{
		bin:     builtBinPath,
		cwd:     cwd,
		binDir:  binDir,
		outFile: outFile,
		base:    base,
		env: []string{
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + base,
			"XDG_CONFIG_HOME=" + base,
			"FORGECTL_TEST_OUT=" + outFile,
		},
	}
}

// TestIntegration_LaunchInit_FromClaunch_Malformed covers runClaunchImport's
// malformed-file branch: LoadLegacyLaunch returns a distinguishing error --
// ErrNoLegacyLaunch when the file is absent, a wrapped decode error otherwise --
// and runClaunchImport uses errors.Is to tell them apart, so a syntactically
// broken claunch.conf must surface as "malformed", not be misreported as "no
// legacy claunch.conf found".
func TestIntegration_LaunchInit_FromClaunch_Malformed(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "this is not [valid toml\n= = =\n")

	stderr, err := h.runExpectErr(t, nil, "init", "--from-claunch")
	if err == nil {
		t.Fatal("`launch init --from-claunch` succeeded against a malformed legacy claunch.conf, want an error")
	}
	// fang (the styled-error renderer) capitalizes the message's first letter,
	// so assert past the sentence-case-sensitive first word (mirrors the
	// NoLegacy test's convention in launch_test.go).
	if !strings.Contains(stderr, "claunch.conf is malformed, not importing") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "claunch.conf is malformed, not importing")
	}
	if strings.Contains(stderr, "no legacy claunch.conf found") {
		t.Errorf("stderr = %q, a malformed file must not be misreported as absent", stderr)
	}
}

// TestIntegration_LaunchInit_FromClaunch_EmptyLegacy covers the IsZero
// refusal: a legacy claunch.conf that decodes cleanly (valid TOML, or no
// content at all) but defines neither [defaults] nor any [[project]] has
// nothing to import -- LoadLegacyLaunch returns (zero, path, nil) for this case
// (a clean decode of an empty/section-less file), a different branch than both
// the RoundTrip (non-zero) and Malformed (decode error) cases.
func TestIntegration_LaunchInit_FromClaunch_EmptyLegacy(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "")

	stderr, err := h.runExpectErr(t, nil, "init", "--from-claunch")
	if err == nil {
		t.Fatal("`launch init --from-claunch` succeeded against an empty legacy claunch.conf, want an error")
	}
	if !strings.Contains(stderr, "no [defaults] or [[project]] to import") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "no [defaults] or [[project]] to import")
	}
}

// TestIntegration_LaunchInit_FromClaunch_PreservesOtherSections covers the new
// appendToConfig helper's append-not-overwrite contract: importing into a
// config.toml that already has an unrelated [bench] section must leave that
// section intact rather than truncating or clobbering the file.
func TestIntegration_LaunchInit_FromClaunch_PreservesOtherSections(t *testing.T) {
	h := newLegacyHarness(t)

	cfgPath := childConfigPath(h.base)
	existing, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml before import: %v", err)
	}
	withBench := string(existing) + "\n[bench]\ntelemetry = true\n"
	if err := os.WriteFile(cfgPath, []byte(withBench), 0o644); err != nil {
		t.Fatalf("write config.toml with [bench] section: %v", err)
	}

	h.run(t, "init", "--from-claunch")

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after import: %v", err)
	}
	body := string(data)
	for _, want := range []string{"[bench]", "telemetry = true", "[launch.defaults]", "[[launch.project]]"} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml missing %q after import (append clobbered existing content); got:\n%s", want, body)
		}
	}
}

// TestIntegration_LaunchInit_FromClaunch_ImportedProfileDrivesLaunch covers
// end-to-end fidelity of the import: the legacy profile's fields, including
// the AllowDanger *bool pointer field, must round-trip through the
// toml.Encoder used by runClaunchImport (encode from the decoded LaunchConfig)
// and then back through config.toml's own decode path, so a subsequent real
// `launch` invocation resolves the exact same posture the legacy file
// specified -- not just a written file that happens to contain the right
// substrings.
func TestIntegration_LaunchInit_FromClaunch_ImportedProfileDrivesLaunch(t *testing.T) {
	h := newLegacyHarness(t)
	h.run(t, "init", "--from-claunch")

	h.run(t, "-p", "hi")

	got := h.recordedArgs(t)
	want := []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--model", "sonnet",
		"-p", "hi",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args after import = %v, want %v (imported profile should drive launch identically to the legacy file it replaced)", got, want)
	}
}
