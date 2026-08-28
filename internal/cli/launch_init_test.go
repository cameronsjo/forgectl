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
// posture, so `launch init --from-claunch` reaches runLaunchMigrate's legacy
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
			"XDG_CONFIG_HOME=" + testXDGConfigHome(base),
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
		"--effort", "high", // derived; the legacy file predates the effort key entirely
		"-p", "hi",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args after import = %v, want %v (imported profile should drive launch identically to the legacy file it replaced)", got, want)
	}
}

// TestIntegration_Launch_UnrepresentableLegacy_WarnsAndRetainsSource is
// forgectl#417 end to end on the surface that actually destroys. An ordinary
// `forgectl launch which` runs the automatic migration, which backs up and
// unlinks claunch.conf — so a file forgectl only partly decoded must leave
// that path untaken. The launch itself must still succeed: a refusal that
// blocks launch is a worse bug than the silent loss it prevents.
func TestIntegration_Launch_UnrepresentableLegacy_WarnsAndRetainsSource(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "[defaults]\nmodel = \"opus\"\nunknown_field = \"x\"\n")
	legacyPath := legacyConfigPath(h.base)

	stdout, stderr := h.run(t, "which")

	if !strings.Contains(stderr, "unknown_field") {
		t.Errorf("stderr = %q, want it to name the field forgectl cannot represent", stderr)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy claunch.conf was retired despite the refusal: %v", err)
	}
	// config.toml existed before the run (no [launch] section); the refusal
	// must not have added one.
	cfg, err := os.ReadFile(childConfigPath(h.base))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(cfg), "[launch") {
		t.Errorf("config.toml gained a [launch] section despite the refusal:\n%s", cfg)
	}
	// The read stays lenient: the modelled fields still drive the launch.
	if !strings.Contains(stdout, "opus") {
		t.Errorf("which stdout = %q, want the legacy model still resolved (a refusal must not blank the profile)", stdout)
	}
}

// TestIntegration_LaunchMigrate_UnrepresentableLegacy_Refuses covers the
// on-demand surface: `launch migrate` must exit non-zero naming the field, and
// write nothing.
func TestIntegration_LaunchMigrate_UnrepresentableLegacy_Refuses(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "[defaults]\nmodel = \"opus\"\nunknown_field = \"x\"\n")

	stderr, err := h.runExpectErr(t, nil, "migrate")
	if err == nil {
		t.Fatal("`launch migrate` succeeded against an unrepresentable legacy config, want an error")
	}
	if !strings.Contains(stderr, "unknown_field") {
		t.Errorf("stderr = %q, want it to name the field forgectl cannot represent", stderr)
	}
	if !strings.Contains(stderr, "cannot represent") {
		t.Errorf("stderr = %q, want it to say forgectl cannot represent the settings", stderr)
	}
	if _, err := os.Stat(legacyConfigPath(h.base)); err != nil {
		t.Fatalf("legacy claunch.conf was retired despite the refusal: %v", err)
	}
	cfg, err := os.ReadFile(childConfigPath(h.base))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(cfg), "[launch") {
		t.Errorf("config.toml gained a [launch] section despite the refusal:\n%s", cfg)
	}
}

// TestIntegration_LaunchMigrate_DefaultsOnlyImport covers the second half of
// #417's message fixes. A legacy file with [defaults] and no [[project]] is a
// legitimate import, and `launch migrate` used to report it as
// "Imported 0 launch profile(s)" — a success that reads as a no-op. Without
// this test the whole zero-profile branch could be deleted and the suite would
// stay green (#418 review).
func TestIntegration_LaunchMigrate_DefaultsOnlyImport(t *testing.T) {
	h := newLegacyHarnessWithBody(t, "[defaults]\nmodel = \"opus\"\n")

	stdout, _ := h.run(t, "migrate")

	if strings.Contains(stdout, "Imported 0 launch profile(s)") {
		t.Errorf("stdout = %q, a defaults-only import must not report zero profiles", stdout)
	}
	if !strings.Contains(stdout, "Imported launch defaults (no project profiles)") {
		t.Errorf("stdout = %q, want the defaults-only import line", stdout)
	}
	// The import still has to have happened.
	cfg, err := os.ReadFile(childConfigPath(h.base))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !strings.Contains(string(cfg), "opus") {
		t.Errorf("config.toml = %q, want the imported defaults", cfg)
	}
}
