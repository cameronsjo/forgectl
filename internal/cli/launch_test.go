package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// builtBinPath is the forgectl binary built once by TestMain and reused by
// every harness in this package's integration tests.
var builtBinPath string

// TestMain builds the forgectl binary once for the package's integration
// tests, rather than paying a `go build` per test case.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forgectl-integration-*")
	if err != nil {
		panic("create temp bin dir: " + err.Error())
	}
	builtBinPath = filepath.Join(dir, "forgectl")

	// Build by import path (no hardcoded Dir) so this is portable: `go test`
	// runs with the package dir as cwd, inside the module, so go resolves the
	// main package wherever the checkout lives (CI, any machine, post-merge).
	build := exec.Command("go", "build", "-o", builtBinPath, "github.com/cameronsjo/forgectl")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		_ = os.RemoveAll(dir)
		panic("build forgectl: " + err.Error())
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// --- harness -----------------------------------------------------------

// testXDGConfigHome returns the lexical XDG base accepted by the migration
// boundary on each OS. Darwin's os.UserConfigDir ignores XDG, so the explicit
// value must equal its native Application Support base; other Unix targets
// use the supplied base directly.
func testXDGConfigHome(base string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(base, "Library", "Application Support")
	}
	return base
}

func childConfigPath(base string) string {
	return filepath.Join(testXDGConfigHome(base), "forgectl", "config.toml")
}

// legacyConfigPath returns the legacy claunch.conf path under a fake base.
func legacyConfigPath(base string) string {
	return filepath.Join(testXDGConfigHome(base), "claunch", "claunch.conf")
}

const stubClaude = `#!/usr/bin/env bash
{
  echo ARGS_START
  for a in "$@"; do echo "$a"; done
  echo ARGS_END
  echo "OTEL_EXPORTER=${OTEL_EXPORTER:-}"
  echo "CLAUDE_CODE_ENABLE_TELEMETRY=${CLAUDE_CODE_ENABLE_TELEMETRY:-}"
  echo "OTEL_EXPORTER_OTLP_ENDPOINT=${OTEL_EXPORTER_OTLP_ENDPOINT:-}"
  echo "OTEL_EXPORTER_OTLP_PROTOCOL=${OTEL_EXPORTER_OTLP_PROTOCOL:-}"
} > "$FORGECTL_TEST_OUT"
`

// telemetryConfigTemplate is nativeConfigTemplate plus an opt-in [bench]
// telemetry block and a profile env override on the OTLP endpoint — so the
// harness can assert both that telemetry is injected and that a profile value
// wins over the injected default.
const telemetryConfigTemplate = `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[launch.project]]
match = "%s"
model = "sonnet"
env = { OTEL_EXPORTER_OTLP_ENDPOINT = "http://profile-wins:9999" }

[bench]
telemetry = true
`

const nativeConfigTemplate = `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[launch.project]]
match = "%s"
model = "sonnet"
env = { OTEL_EXPORTER = "otlp" }
add_dir = ["%s/shared"]
`

// harness wires one isolated forgectl invocation: a real cwd, a stub claude on
// PATH, an isolated HOME/XDG_CONFIG_HOME, and a file the stub writes its
// recorded argv/env to.
type harness struct {
	bin     string
	cwd     string
	binDir  string
	outFile string
	env     []string
	// base is the fake HOME/XDG_CONFIG_HOME root. EVERY constructor sets it:
	// childConfigPath("") returns a RELATIVE path, so a test reconstructing a
	// config path from a zero-value base writes into the checkout while the
	// child process reads a different XDG root — a mismatch that reads as a
	// config-loading bug. Leaving the zero value unreachable is cheaper than
	// documenting which harnesses may be asked for it.
	base string
}

// newHarness builds a harness with a native config.toml (a [launch.project]
// entry matching h.cwd) and a stub claude on PATH.
func newHarness(t *testing.T) *harness {
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
	body := fmt.Sprintf(nativeConfigTemplate, cwd, cwd)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
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

// newTelemetryHarness builds a harness whose config.toml enables [bench]
// telemetry (and sets a profile env override on the OTLP endpoint), so a launch
// exercises the injection + profile-wins precedence end to end.
func newTelemetryHarness(t *testing.T) *harness {
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
	body := fmt.Sprintf(telemetryConfigTemplate, cwd)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
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

// newLegacyHarness builds a harness whose config.toml has no [launch] section
// and whose profile instead comes from a legacy claunch.conf — the
// zero-migration fallback path.
func newLegacyHarness(t *testing.T) *harness {
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
	legacyBody := fmt.Sprintf(`[defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[project]]
match = "%s"
model = "sonnet"
`, cwd)
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

// shadowConfigTemplate is a minimal [launch] section — defaults only, no
// [[project]] entries — used alongside a legacy claunch.conf to reproduce
// #114's shadow scenario: config.toml's [launch] is non-zero, so
// resolveLaunchConfig returns it wholesale and never looks at the legacy
// file, silently orphaning any [[project]] profiles recorded there.
const shadowConfigTemplate = `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true
`

// newShadowHarness is newLegacyHarness plus a config.toml with a live
// [launch] section — the fallback-cliff scenario (#114). The legacy
// claunch.conf is present and well-formed, but config.toml's [launch] takes
// precedence and shadows it entirely.
func newShadowHarness(t *testing.T) *harness {
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
	if err := os.WriteFile(cfgPath, []byte(shadowConfigTemplate), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	legacyPath := legacyConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir legacy config dir: %v", err)
	}
	legacyBody := fmt.Sprintf(`[defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[project]]
match = "%s"
model = "sonnet"
`, cwd)
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

// shadowConfigWithProjectTemplate is shadowConfigTemplate plus a
// [[launch.project]] entry matching the same path the legacy claunch.conf's
// own [[project]] uses — the "both define the same project match" case: the
// additive merge must skip the legacy entry rather than duplicate or
// overwrite the one already in [launch].
const shadowConfigWithProjectTemplate = `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[launch.project]]
match = "%s"
model = "opus"
`

// newShadowHarnessWithProject is newShadowHarness, but config.toml already
// carries a [[launch.project]] entry for the same match path the legacy
// claunch.conf's own [[project]] declares (with a different model, so a bad
// merge would be observable). Every [defaults] field the legacy file sets is
// also already set in config.toml, so a correct merge contributes nothing
// at all — the "fully superseded" case.
func newShadowHarnessWithProject(t *testing.T) *harness {
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
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(shadowConfigWithProjectTemplate, cwd)), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	legacyPath := legacyConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir legacy config dir: %v", err)
	}
	legacyBody := fmt.Sprintf(`[defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[project]]
match = "%s"
model = "sonnet"
`, cwd)
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

func writeStubClaude(t *testing.T, binDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(stubClaude), 0o755); err != nil {
		t.Fatalf("write stub claude: %v", err)
	}
}

// run execs `forgectl launch <args…>`, failing the test on a non-zero exit.
// Returns (stdout, stderr).
func (h *harness) run(t *testing.T, args ...string) (string, string) {
	t.Helper()
	stdout, stderr, err := h.exec(args...)
	if err != nil {
		t.Fatalf("forgectl launch %v: %v\nstderr:\n%s", args, err, stderr)
	}
	return stdout, stderr
}

// runExpectErr execs `forgectl launch <args…>` with extraEnv appended, without
// failing the test on a non-zero exit — for asserting on failure paths.
// Returns (stderr, err) — error last, per Go convention.
func (h *harness) runExpectErr(t *testing.T, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(h.bin, append([]string{"launch"}, args...)...)
	cmd.Dir = h.cwd
	cmd.Env = append(append([]string{}, h.env...), extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

// runWithEnv is run but with extraEnv appended to the child's environment —
// for exercising an env-var toggle (e.g. FORGECTL_SKIP_LEGACY_MIGRATE) on the
// success path, where runExpectErr's error-path contract doesn't fit.
func (h *harness) runWithEnv(t *testing.T, extraEnv []string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(h.bin, append([]string{"launch"}, args...)...)
	cmd.Dir = h.cwd
	cmd.Env = append(append([]string{}, h.env...), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("forgectl launch %v (env=%v): %v\nstderr:\n%s", args, extraEnv, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func (h *harness) exec(args ...string) (string, string, error) {
	cmd := exec.Command(h.bin, append([]string{"launch"}, args...)...)
	cmd.Dir = h.cwd
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// recordedArgs parses the ARGS_START..ARGS_END block the stub claude wrote to
// h.outFile into the argv it received.
func (h *harness) recordedArgs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.outFile)
	if err != nil {
		t.Fatalf("read claude out file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var args []string
	inBlock := false
	for _, l := range lines {
		switch {
		case l == "ARGS_START":
			inBlock = true
		case l == "ARGS_END":
			inBlock = false
		case inBlock:
			args = append(args, l)
		}
	}
	return args
}

// recordedOTEL returns the OTEL_EXPORTER value the stub claude observed.
func (h *harness) recordedOTEL(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.outFile)
	if err != nil {
		t.Fatalf("read claude out file: %v", err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(l, "OTEL_EXPORTER="); ok {
			return v
		}
	}
	return ""
}

// stripFromPath returns env with any PATH entry rewritten to exclude dir, so a
// bare `claude` LookPath fails while an explicit-binary override is exercised.
func stripFromPath(env []string, dir string) []string {
	out := make([]string, len(env))
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			parts := strings.Split(strings.TrimPrefix(e, "PATH="), string(os.PathListSeparator))
			kept := parts[:0]
			for _, p := range parts {
				if p != dir {
					kept = append(kept, p)
				}
			}
			out[i] = "PATH=" + strings.Join(kept, string(os.PathListSeparator))
			continue
		}
		out[i] = e
	}
	return out
}

// recordedEnv returns the value of key the stub claude observed in its
// environment (empty string when unset).
func (h *harness) recordedEnv(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile(h.outFile)
	if err != nil {
		t.Fatalf("read claude out file: %v", err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}

// --- tests ---------------------------------------------------------------

func TestIntegration_Launch_InjectsTelemetryEnv(t *testing.T) {
	h := newTelemetryHarness(t)
	h.run(t, "-p", "hi")

	if got := h.recordedEnv(t, "CLAUDE_CODE_ENABLE_TELEMETRY"); got != "1" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q, want 1", got)
	}
	// Injected default, no profile override.
	if got := h.recordedEnv(t, "OTEL_EXPORTER_OTLP_PROTOCOL"); got != "grpc" {
		t.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL = %q, want grpc", got)
	}
	// Profile env must win over the injected default endpoint.
	if got := h.recordedEnv(t, "OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://profile-wins:9999" {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want the profile override to win", got)
	}
}

func TestIntegration_Launch_NoTelemetryWhenDisabled(t *testing.T) {
	h := newHarness(t) // native config has no [bench] section
	h.run(t, "-p", "hi")

	if got := h.recordedEnv(t, "CLAUDE_CODE_ENABLE_TELEMETRY"); got != "" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q, want empty when telemetry is off", got)
	}
}

func TestIntegration_Builder_AppliesProfileAndPassesThrough(t *testing.T) {
	h := newHarness(t)
	h.run(t, "-p", "hi")

	got := h.recordedArgs(t)
	want := []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--model", "sonnet",
		"--effort", "high", // derived from sonnet; the fixture sets no effort
		"--add-dir", h.cwd + "/shared",
		"-p", "hi",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args = %v, want %v", got, want)
	}
	if otel := h.recordedOTEL(t); otel != "otlp" {
		t.Errorf("OTEL_EXPORTER = %q, want %q", otel, "otlp")
	}
}

func TestIntegration_CodexBuilderAndAgentsGuidance(t *testing.T) {
	h := newBareHarness(t, `[launch.defaults]
harness = "codex"
model = "gpt-5"
approval_policy = "never"
sandbox = "read-only"
`)
	if err := os.WriteFile(filepath.Join(h.binDir, "codex"), []byte(stubClaude), 0o755); err != nil {
		t.Fatalf("write stub codex: %v", err)
	}
	h.run(t, "review this")
	want := []string{
		"exec", "--config", `approval_policy="never"`,
		"--sandbox", "read-only", "--model", "gpt-5", "review this",
	}
	if got := h.recordedArgs(t); !equalArgs(got, want) {
		t.Errorf("Codex args = %v, want %v", got, want)
	}

	stderr, err := h.runExpectErr(t, nil, "agents", "--json")
	if err == nil {
		t.Fatal("Codex launch agents passthrough should be rejected")
	}
	for _, wantText := range []string{"Claude-only", "no Codex adapter"} {
		if !strings.Contains(stderr, wantText) {
			t.Errorf("stderr missing %q: %s", wantText, stderr)
		}
	}
}

func TestIntegration_AgentsJSON_PurePassthrough(t *testing.T) {
	h := newHarness(t)
	stdout, _ := h.run(t, "agents", "--json")

	got := h.recordedArgs(t)
	want := []string{"agents", "--json"}
	if !equalArgs(got, want) {
		t.Errorf("recorded args = %v, want %v", got, want)
	}
	for _, forbidden := range []string{"--permission-mode", "--model"} {
		if containsArg(got, forbidden) {
			t.Errorf("recorded args %v unexpectedly contain %q", got, forbidden)
		}
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want byte-clean empty output", stdout)
	}
}

func TestIntegration_AgentsInteractive_InjectsSubsetAndBannerToStderr(t *testing.T) {
	h := newHarness(t)
	stdout, stderr := h.run(t, "agents", "--cwd", "/x")

	got := h.recordedArgs(t)
	want := []string{
		"agents",
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--model", "sonnet",
		"--effort", "high",
		"--cwd", "/x",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args = %v, want %v", got, want)
	}
	if !strings.Contains(stderr, "claude agents") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "claude agents")
	}
	if strings.Contains(stdout, "claude agents") {
		t.Errorf("banner leaked into stdout: %q", stdout)
	}
}

func TestIntegration_Which_PrintsResolvedProfile(t *testing.T) {
	h := newHarness(t)
	stdout, _ := h.run(t, "which")

	// "high" is sonnet's derived effort — the fixture sets no `effort` key, so
	// its presence here is the derivation reaching the display surface.
	for _, want := range []string{"sonnet", "effort", "high", "plan", h.cwd} {
		if !strings.Contains(stdout, want) {
			t.Errorf("which output missing %q; got:\n%s", want, stdout)
		}
	}
}

// TestIntegration_ClaudeBinOverride exercises the FORGECTL_CLAUDE_BIN override
// (the acceptance criteria from cameronsjo/claude-configurations#289).
func TestIntegration_ClaudeBinOverride(t *testing.T) {
	t.Run("valid override execs the named binary even off PATH", func(t *testing.T) {
		h := newHarness(t)
		env := stripFromPath(h.env, h.binDir)
		env = append(env, "FORGECTL_CLAUDE_BIN="+filepath.Join(h.binDir, "claude"))

		cmd := exec.Command(h.bin, "launch", "-p", "x")
		cmd.Dir = h.cwd
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			t.Fatalf("forgectl launch -p x with FORGECTL_CLAUDE_BIN set: %v", err)
		}

		got := h.recordedArgs(t)
		if len(got) < 2 || got[0] != "--permission-mode" || got[1] != "plan" {
			t.Errorf("recorded args = %v, want it to start with the builder posture", got)
		}
	})

	t.Run("invalid override exits non-zero with a clear error", func(t *testing.T) {
		// #289 acceptance: an invalid FORGECTL_CLAUDE_BIN is a clear error, not a
		// silent PATH fallback. The launch intercept (execute.go) prints the
		// ClaudePath() error to stderr before exiting — assert both the non-zero
		// exit and that the message names the failing source.
		h := newHarness(t)
		env := stripFromPath(h.env, h.binDir)
		env = append(env, "FORGECTL_CLAUDE_BIN=/no/such/claude")

		stderr, err := h.runExpectErr(t, env, "-p", "x")
		var exitErr *exec.ExitError
		if err == nil || !errors.As(err, &exitErr) {
			t.Fatalf("err = %v, want a non-nil *exec.ExitError", err)
		}
		if !strings.Contains(stderr, "FORGECTL_CLAUDE_BIN") {
			t.Errorf("stderr = %q, want it to name the failing FORGECTL_CLAUDE_BIN source", stderr)
		}
	})
}

func TestIntegration_LegacyFallback(t *testing.T) {
	h := newLegacyHarness(t)
	stdout, _ := h.run(t, "which")

	if !strings.Contains(stdout, "sonnet") {
		t.Errorf("which output missing %q (expected fallback to legacy claunch.conf); got:\n%s", "sonnet", stdout)
	}
}

// legacyBakPath returns the backup path an automatic migration renames a
// legacy claunch.conf to.
func legacyBakPath(base string) string {
	return legacyConfigPath(base) + ".bak"
}

// assertLegacyBackedUp asserts the legacy claunch.conf at legacyConfigPath(base)
// was renamed to its .bak sibling (never hard-deleted) by an automatic
// migration.
func assertLegacyBackedUp(t *testing.T, base string) {
	t.Helper()
	legacyPath := legacyConfigPath(base)
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy claunch.conf still present at %s after auto-migration (want it renamed away); stat err = %v", legacyPath, err)
	}
	if _, err := os.Stat(legacyBakPath(base)); err != nil {
		t.Errorf("expected backup file %s, stat error: %v", legacyBakPath(base), err)
	}
}

// TestIntegration_LaunchShadow_WhichAutoMigrates covers #114's fix: a legacy
// claunch.conf present alongside a config.toml [launch] section used to be
// silently shadowed (config.toml wins wholesale, its [[project]] profiles
// ignored, no diagnostic). Now `which` additively merges the legacy file's
// previously-orphaned [[project]] entry into [launch] automatically — no
// conflict here, since config.toml's [launch] carries defaults only — backs
// the legacy file up, and reports a one-line merge notice instead of the old
// recurring "present but ignored" warning.
func TestIntegration_LaunchShadow_WhichAutoMigrates(t *testing.T) {
	h := newShadowHarness(t)
	stdout, stderr := h.run(t, "which")

	if strings.Contains(stderr, "present but ignored") {
		t.Errorf("stderr = %q, still shows the old recurring warning instead of auto-migrating", stderr)
	}
	if !strings.Contains(stderr, "merged 1 addition(s)") || !strings.Contains(stderr, "claunch.conf.bak") {
		t.Errorf("stderr = %q, want the shadow-merge notice", stderr)
	}
	// The legacy [[project]] entry is no longer shadowed — it now matches cwd.
	for _, want := range []string{"sonnet", h.cwd} {
		if !strings.Contains(stdout, want) {
			t.Errorf("which stdout missing %q after auto-merge; got:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "(defaults only)") {
		t.Errorf("which stdout still says %q; the merged project should now match cwd; got:\n%s", "(defaults only)", stdout)
	}

	assertLegacyBackedUp(t, h.base)
}

// TestIntegration_LaunchShadow_DoctorAutoMigrates covers #114 for `doctor`:
// the auto-merge is a routine action, not a failure — doctor must still exit
// 0 and report the merge notice in place of the old warning.
func TestIntegration_LaunchShadow_DoctorAutoMigrates(t *testing.T) {
	h := newShadowHarness(t)
	stdout, _ := h.run(t, "doctor")

	if strings.Contains(stdout, "present but ignored") {
		t.Errorf("doctor stdout still shows the old recurring warning: %q", stdout)
	}
	if !strings.Contains(stdout, "merged 1 addition(s)") {
		t.Errorf("doctor stdout missing the shadow-merge notice; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "1 project profile(s)") {
		t.Errorf("doctor stdout should report the merged-in project profile; got:\n%s", stdout)
	}
}

// TestIntegration_LaunchShadow_ExecAutoMigrates covers #114 for the bare exec
// path: the merge notice fires on stderr, the legacy file is backed up, and
// the previously-shadowed sonnet project — no longer orphaned once merged —
// is what now actually reaches claude.
func TestIntegration_LaunchShadow_ExecAutoMigrates(t *testing.T) {
	h := newShadowHarness(t)
	_, stderr := h.run(t, "-p", "hi")

	if strings.Contains(stderr, "present but ignored") {
		t.Errorf("stderr = %q, still shows the old recurring warning instead of auto-migrating", stderr)
	}
	if !strings.Contains(stderr, "merged 1 addition(s)") {
		t.Errorf("stderr = %q, want the shadow-merge notice", stderr)
	}
	got := h.recordedArgs(t)
	want := []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--model", "sonnet",
		"--effort", "high", // derived from sonnet — the merged-in project now applies
		"-p", "hi",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args = %v, want %v (the merged-in project profile, no longer shadowed)", got, want)
	}
	assertLegacyBackedUp(t, h.base)
}

// TestIntegration_LaunchShadow_DuplicateProjectMatch_NoOverwrite covers the
// additive merge's core safety property: when the legacy file and [launch]
// both define a [[project]] for the same match path, the merge must skip the
// legacy entry rather than duplicate or overwrite the one already configured
// — config.toml's own project (and its model) must survive untouched.
func TestIntegration_LaunchShadow_DuplicateProjectMatch_NoOverwrite(t *testing.T) {
	h := newShadowHarnessWithProject(t)
	cfgPath := childConfigPath(h.base)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml before run: %v", err)
	}

	_, stderr := h.run(t, "-p", "hi")

	if !strings.Contains(stderr, "legacy config fully superseded, removed.") {
		t.Errorf("stderr = %q, want the fully-superseded notice (nothing new to merge)", stderr)
	}

	// Nothing was added, so config.toml is left byte-identical — no rewrite
	// was needed to satisfy "never overwrite or duplicate".
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after run: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("config.toml changed even though the merge had nothing new to add;\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := strings.Count(string(after), `match = "`+h.cwd+`"`); n != 1 {
		t.Errorf("config.toml has %d project entries matching %q, want exactly 1 (no duplication)", n, h.cwd)
	}

	// config.toml's own project (model = "opus") must have won, not the
	// legacy file's conflicting model = "sonnet" for the same match.
	got := h.recordedArgs(t)
	want := []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--model", "opus",
		"--effort", "medium", // derived from opus, not the legacy sonnet entry
		"-p", "hi",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args = %v, want %v (config.toml's own project must win)", got, want)
	}

	assertLegacyBackedUp(t, h.base)
}

// TestIntegration_LaunchFallback_AutoMigrates covers the fallback scenario
// (#109 automated): a legacy claunch.conf and no [launch] section at all. The
// first invocation imports it wholesale (the same logic `launch migrate`
// runs on demand), backs the legacy file up, and reports a one-line notice;
// a second invocation has nothing left to migrate and is a silent no-op.
func TestIntegration_LaunchFallback_AutoMigrates(t *testing.T) {
	h := newLegacyHarness(t)
	stdout, stderr := h.run(t, "which")

	if !strings.Contains(stderr, "migrated 1 profile(s)") || !strings.Contains(stderr, "claunch.conf.bak") {
		t.Errorf("stderr = %q, want the fallback auto-migrate notice", stderr)
	}
	if !strings.Contains(stdout, "sonnet") {
		t.Errorf("which stdout missing %q after auto-migration; got:\n%s", "sonnet", stdout)
	}

	cfgPath := childConfigPath(h.base)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after auto-migration: %v", err)
	}
	for _, want := range []string{"[launch.defaults]", "[[launch.project]]"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.toml missing %q after auto-migration; got:\n%s", want, data)
		}
	}
	assertLegacyBackedUp(t, h.base)

	// Second run: nothing left to migrate.
	_, stderr2 := h.run(t, "which")
	if strings.Contains(stderr2, "migrated") {
		t.Errorf("second run re-ran the migration: stderr = %q", stderr2)
	}
}

// TestIntegration_LaunchFallback_ConcurrentAutoMigrate_NoDuplicateSection is
// the regression test for the concurrency race code review caught: two
// concurrent `forgectl launch` invocations can both observe
// cfg.Launch.IsZero() == true at process start (that check runs once,
// before either process's autoMigrateFallback re-checks anything) and both
// attempt the fallback-scenario migration — each appending a full [launch]
// section, since writeImportedLaunchSection's underlying appendLaunchSection
// is a pure append with no re-check. The resulting duplicate TOML header
// makes config.toml fail to decode for every subsequent invocation of any
// subcommand (BurntSushi's decoder rejects a redefined table outright), with
// only a stderr WARN as evidence and no self-repair — the legacy file is
// already renamed to .bak by the winner, so migration never runs again.
// Reproduced live: 8 concurrent `forgectl launch which` runs against a
// fresh HOME/XDG_CONFIG_HOME produced exactly this corruption before the
// fix. The fix serializes the whole read-decide-write-backup critical
// section behind config.WithFileLock and re-checks immediately before
// writing, so every racer either performs the migration or cleanly observes
// it already done — this asserts both halves of that: no racer errors, and
// the result has exactly one [launch] section that still parses.
func TestIntegration_LaunchFallback_ConcurrentAutoMigrate_NoDuplicateSection(t *testing.T) {
	h := newLegacyHarness(t)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	stderrs := make([]string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, stderr, err := h.exec("which")
			errs[i] = err
			stderrs[i] = stderr
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: forgectl launch which failed: %v\nstderr:\n%s", i, err, stderrs[i])
		}
	}

	cfgPath := childConfigPath(h.base)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after concurrent auto-migration: %v", err)
	}
	body := string(data)
	if got := strings.Count(body, "[launch.defaults]"); got != 1 {
		t.Errorf("config.toml has %d [launch.defaults] headers after %d concurrent auto-migrations, want exactly 1 (duplicate-header corruption):\n%s", got, n, body)
	}
	if got := strings.Count(body, "[[launch.project]]"); got != 1 {
		t.Errorf("config.toml has %d [[launch.project]] headers after %d concurrent auto-migrations, want exactly 1:\n%s", got, n, body)
	}

	// The corruption's signature symptom is that a duplicate header fails the
	// WHOLE decode, not just [launch] — every section silently falls back to
	// built-in defaults. Prove config.toml still parses (rather than just
	// eyeballing the header counts above) by running `which` once more and
	// checking the migrated profile still resolves correctly.
	stdout, _ := h.run(t, "which")
	if !strings.Contains(stdout, "sonnet") {
		t.Errorf("which output missing %q after concurrent auto-migration (config.toml failed to parse); got:\n%s", "sonnet", stdout)
	}

	assertLegacyBackedUp(t, h.base)
}

// TestIntegration_Launch_SkipLegacyMigrateEnv covers the escape hatch:
// FORGECTL_SKIP_LEGACY_MIGRATE=1 disables auto-migration and restores the
// original warn-only behavior byte-for-byte — no config.toml rewrite, no
// backup, the same "present but ignored" notice as before this feature.
func TestIntegration_Launch_SkipLegacyMigrateEnv(t *testing.T) {
	h := newShadowHarness(t)
	cfgPath := childConfigPath(h.base)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml before run: %v", err)
	}

	stdout, stderr := h.runWithEnv(t, []string{"FORGECTL_SKIP_LEGACY_MIGRATE=1"}, "which")

	if !strings.Contains(stderr, "present but ignored") {
		t.Errorf("stderr = %q, want the original warn-only notice preserved when %s=1", stderr, "FORGECTL_SKIP_LEGACY_MIGRATE")
	}
	if !strings.Contains(stdout, "(defaults only)") {
		t.Errorf("which stdout missing %q (the legacy project must stay shadowed); got:\n%s", "(defaults only)", stdout)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after run: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("config.toml changed even though FORGECTL_SKIP_LEGACY_MIGRATE=1 was set;\nbefore:\n%s\nafter:\n%s", before, after)
	}

	legacyPath := legacyConfigPath(h.base)
	if _, err := os.Stat(legacyPath); err != nil {
		t.Errorf("legacy claunch.conf was touched even though FORGECTL_SKIP_LEGACY_MIGRATE=1 was set: %v", err)
	}
	if _, err := os.Stat(legacyBakPath(h.base)); !os.IsNotExist(err) {
		t.Errorf("unexpected backup file created despite FORGECTL_SKIP_LEGACY_MIGRATE=1")
	}
}

// TestIntegration_LaunchMigrate_SameLogicAsFromClaunchAlias covers the
// promoted `forgectl launch migrate` subcommand: it must run the exact same
// import logic as the deprecated `launch init --from-claunch` spelling
// (writing an identical [launch] section, leaving the legacy file in place,
// and refusing a second run the same way).
func TestIntegration_LaunchMigrate_SameLogicAsFromClaunchAlias(t *testing.T) {
	h := newLegacyHarness(t)
	h.run(t, "migrate")

	cfgPath := childConfigPath(h.base)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after migrate: %v", err)
	}
	body := string(data)
	for _, want := range []string{`[launch.defaults]`, `model = "opus"`, `[[launch.project]]`, h.cwd} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml missing %q after `launch migrate`; got:\n%s", want, body)
		}
	}

	// The explicit importer leaves the legacy file in place, same contract as
	// `--from-claunch` — only the automatic migration paths back it up.
	legacyPath := legacyConfigPath(h.base)
	if _, err := os.Stat(legacyPath); err != nil {
		t.Errorf("`launch migrate` unexpectedly touched the legacy file: %v", err)
	}

	stderr, err := h.runExpectErr(t, nil, "migrate")
	if err == nil {
		t.Fatal("second `launch migrate` succeeded, want a refusal error (already has a [launch] section)")
	}
	if !strings.Contains(stderr, "already has a [launch] section") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "already has a [launch] section")
	}
}

// TestIntegration_LegacyFallback_NoShadowWarning is a negative control: when
// config.toml has no [launch] section at all, the legacy file is genuinely
// honored (not shadowed) and must not trigger the #114 warning.
func TestIntegration_LegacyFallback_NoShadowWarning(t *testing.T) {
	h := newLegacyHarness(t)
	_, stderr := h.run(t, "which")

	if strings.Contains(stderr, "present but ignored") {
		t.Errorf("stderr = %q, want no shadow warning when config.toml has no [launch] section", stderr)
	}
}

// TestIntegration_Which_NoShadowWarningWithoutLegacy is a negative control:
// no legacy claunch.conf exists at all, so there's nothing to shadow.
func TestIntegration_Which_NoShadowWarningWithoutLegacy(t *testing.T) {
	h := newHarness(t)
	_, stderr := h.run(t, "which")

	if strings.Contains(stderr, "present but ignored") {
		t.Errorf("stderr = %q, want no shadow warning when no legacy claunch.conf exists", stderr)
	}
}

// newBareHarness builds a harness with no legacy claunch.conf. When
// configBody is non-empty, it's written verbatim as config.toml (used to
// exercise `which`'s terminal fallback branch for a present file lacking
// [launch], including an explicit-but-empty [launch] section); when empty, no
// config.toml is written at all, exercising the truly-absent-file case (#57).
func newBareHarness(t *testing.T, configBody string) *harness {
	t.Helper()

	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve symlinks on temp cwd: %v", err)
	}
	binDir := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "claude.out")
	base := t.TempDir()

	writeStubClaude(t, binDir)

	if configBody != "" {
		cfgPath := childConfigPath(base)
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			t.Fatalf("mkdir config dir: %v", err)
		}
		if err := os.WriteFile(cfgPath, []byte(configBody), 0o644); err != nil {
			t.Fatalf("write config.toml: %v", err)
		}
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

func TestIntegration_Which_ConfigSourceLabel(t *testing.T) {
	t.Run("file present, no [launch] section", func(t *testing.T) {
		h := newBareHarness(t, "[bench]\ntelemetry = true\n")
		stdout, _ := h.run(t, "which")

		if !strings.Contains(stdout, "no [launch] section") {
			t.Errorf("which output missing %q; got:\n%s", "no [launch] section", stdout)
		}
		if strings.Contains(stdout, "(missing") {
			t.Errorf("which output unexpectedly contains %q for a present config; got:\n%s", "(missing", stdout)
		}
	})

	// An explicit-but-empty [launch] header is still authoritative: migration
	// presence is not inferred from the decoded struct's zero value.
	t.Run("file present, explicit empty [launch] section", func(t *testing.T) {
		h := newBareHarness(t, "[launch]\n\n[bench]\ntelemetry = true\n")
		stdout, _ := h.run(t, "which")

		if strings.Contains(stdout, "no [launch] section") {
			t.Errorf("which output misclassified an explicitly present empty table; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, "config.toml") {
			t.Errorf("which output does not identify authoritative config.toml; got:\n%s", stdout)
		}
		if strings.Contains(stdout, "(missing") {
			t.Errorf("which output unexpectedly contains %q for a present config; got:\n%s", "(missing", stdout)
		}
	})

	t.Run("file truly absent", func(t *testing.T) {
		h := newBareHarness(t, "")
		stdout, _ := h.run(t, "which")

		if !strings.Contains(stdout, "missing") {
			t.Errorf("which output missing %q; got:\n%s", "missing", stdout)
		}
	})
}

// TestIntegration_LaunchInit_FromClaunch_RoundTrip covers #109: `launch init
// --from-claunch` migrates an existing legacy claunch.conf into config.toml's
// [launch] section, and the launcher stops falling back to the legacy file
// once the import lands.
func TestIntegration_LaunchInit_FromClaunch_RoundTrip(t *testing.T) {
	h := newLegacyHarness(t)
	h.run(t, "init", "--from-claunch")

	cfgPath := childConfigPath(h.base)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after import: %v", err)
	}
	body := string(data)
	for _, want := range []string{`[launch.defaults]`, `model = "opus"`, `[[launch.project]]`, h.cwd} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml missing %q after import; got:\n%s", want, body)
		}
	}

	stdout, _ := h.run(t, "which")
	if strings.Contains(stdout, "legacy") {
		t.Errorf("which output still labels the profile legacy after import; got:\n%s", stdout)
	}
}

// TestIntegration_LaunchInit_FromClaunch_Idempotent covers the refusal path:
// a second import onto a config.toml that already has a [launch] section
// errors instead of silently duplicating or overwriting it.
func TestIntegration_LaunchInit_FromClaunch_Idempotent(t *testing.T) {
	h := newLegacyHarness(t)
	h.run(t, "init", "--from-claunch")

	stderr, err := h.runExpectErr(t, nil, "init", "--from-claunch")
	if err == nil {
		t.Fatal("second `launch init --from-claunch` succeeded, want a refusal error")
	}
	if !strings.Contains(stderr, "already has a [launch] section") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "already has a [launch] section")
	}
}

// TestIntegration_LaunchInit_FromClaunch_NoLegacy covers the case where there
// is nothing to import: no legacy claunch.conf exists at all.
func TestIntegration_LaunchInit_FromClaunch_NoLegacy(t *testing.T) {
	h := newBareHarness(t, "")

	stderr, err := h.runExpectErr(t, nil, "init", "--from-claunch")
	if err == nil {
		t.Fatal("`launch init --from-claunch` succeeded with no legacy claunch.conf, want an error")
	}
	// fang (the styled-error renderer) capitalizes the message's first letter,
	// so assert on a substring past the sentence-case-sensitive first word.
	if !strings.Contains(stderr, "legacy claunch.conf found") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "legacy claunch.conf found")
	}
}

// --- small local helpers (avoid extra imports for one-line ops) ----------

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// --- Codex-path CLI branches -------------------------------------------
//
// These three branches shipped with no CLI-level coverage: `launch which`'s
// Codex rows, `launch doctor`'s Codex resolution and its invalid-profile
// path, and the exec banner. Each is the only thing that tells an operator
// what posture a Codex launch is actually running under.

func codexHarness(t *testing.T, configBody string) *harness {
	t.Helper()
	h := newBareHarness(t, configBody)
	if err := os.WriteFile(filepath.Join(h.binDir, "codex"), []byte(stubClaude), 0o755); err != nil {
		t.Fatalf("write stub codex: %v", err)
	}
	return h
}

const codexConfig = `[launch.defaults]
harness = "codex"
model = "gpt-5"
approval_policy = "never"
sandbox = "read-only"
`

// TestIntegration_LaunchWhich_ShowsCodexPosture: `which` must surface the
// approval/sandbox pair for a Codex profile, not Claude's permission-mode and
// allow-danger rows — those are meaningless to Codex and would misreport the
// posture the launch actually runs under.
func TestIntegration_LaunchWhich_ShowsCodexPosture(t *testing.T) {
	h := codexHarness(t, codexConfig)
	stdout, _ := h.run(t, "which")

	for _, want := range []string{"harness", "codex", "approval", "never", "sandbox", "read-only"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`launch which` output missing %q:\n%s", want, stdout)
		}
	}
	for _, forbidden := range []string{"permission", "allow danger"} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("`launch which` showed the Claude-only row %q for a Codex profile:\n%s", forbidden, stdout)
		}
	}
}

// TestIntegration_LaunchWhich_WithholdsEnvValues: the unit test proves the
// renderer withholds env values; this proves the wiring from a real config
// file through to real stdout does too — stdout being the surface that
// actually gets pasted somewhere.
func TestIntegration_LaunchWhich_WithholdsEnvValues(t *testing.T) {
	h := newBareHarness(t, "[launch.defaults.env]\nANTHROPIC_API_KEY = \"sk-ant-hunter2\"\nFOO = \"plainbarvalue\"\n")
	stdout, stderr := h.run(t, "which")
	combined := stdout + stderr

	for _, secret := range []string{"sk-ant-hunter2", "plainbarvalue"} {
		if strings.Contains(combined, secret) {
			t.Errorf("`launch which` leaked the env value %q:\n%s", secret, combined)
		}
	}
	for _, want := range []string{"env", "ANTHROPIC_API_KEY", "FOO"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`launch which` output missing %q:\n%s", want, stdout)
		}
	}
}

// TestIntegration_LaunchDoctor_ResolvesCodexBinary: doctor must resolve and
// report the Codex binary, not silently check for claude.
func TestIntegration_LaunchDoctor_ResolvesCodexBinary(t *testing.T) {
	h := codexHarness(t, codexConfig)
	stdout, _ := h.run(t, "doctor")

	if !strings.Contains(stdout, "codex found:") {
		t.Errorf("`launch doctor` did not report the resolved codex binary:\n%s", stdout)
	}
	if strings.Contains(stdout, "claude found:") {
		t.Errorf("`launch doctor` resolved claude for a Codex profile:\n%s", stdout)
	}
}

// TestIntegration_LaunchDoctor_RejectsUnsupportedHarness: a typo'd harness
// must be a doctor failure with a usable message, not a silent fall-through
// to the Claude path.
func TestIntegration_LaunchDoctor_RejectsUnsupportedHarness(t *testing.T) {
	h := codexHarness(t, "[launch.defaults]\nharness = \"gemini\"\n")

	stderr, err := h.runExpectErr(t, nil, "doctor")
	if err == nil {
		t.Fatal("`launch doctor` should fail on an unsupported harness")
	}
	stdout, _, _ := h.exec("doctor")
	combined := stdout + stderr
	for _, want := range []string{"launch profile invalid", "gemini", "want claude or codex"} {
		if !strings.Contains(combined, want) {
			t.Errorf("doctor output missing %q:\n%s", want, combined)
		}
	}
}

// TestIntegration_CodexExec_PrintsBannerToStderr: a Codex launch left no
// record of the argv it ran with — including the approval/sandbox posture,
// the part worth auditing. The banner goes to stderr so piped stdout stays
// byte-clean.
func TestIntegration_CodexExec_PrintsBannerToStderr(t *testing.T) {
	h := codexHarness(t, codexConfig)
	stdout, stderr := h.run(t, "review this")

	for _, want := range []string{"→ codex", "exec", `approval_policy="never"`, "--sandbox", "read-only"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("Codex banner missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stdout, "→ codex") {
		t.Errorf("banner leaked into stdout, breaking byte-clean piping:\n%s", stdout)
	}
}

// TestIntegration_BareLaunch_NoPromptAndBanners covers the branch the removed
// interview used to own. Two things are asserted.
//
// It never prompts: the argv reaches claude directly, in the full interactive
// posture, with no resume/fork mode. Under the test harness stdin is not a TTY
// so the old code path would have skipped the form anyway — the real assertion
// is the argv shape, which no longer has a mode to carry.
//
// And it banners. This is the ONLY path that puts
// --allow-dangerously-skip-permissions into an INTERACTIVE session (the
// scaffold ships allow_danger = true), and once the huh form stopped rendering
// ahead of syscall.Exec it printed nothing at all. stderr, so a piped stdout
// stays clean.
func TestIntegration_BareLaunch_NoPromptAndBanners(t *testing.T) {
	h := newHarness(t)
	stdout, stderr := h.run(t)

	got := h.recordedArgs(t)
	want := []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--ide", "--exclude-dynamic-system-prompt-sections",
		"--model", "sonnet",
		"--effort", "high",
		"--add-dir", h.cwd + "/shared",
	}
	if !equalArgs(got, want) {
		t.Errorf("recorded args = %v, want %v", got, want)
	}
	for _, forbidden := range []string{"--resume", "--fork-session"} {
		if containsArg(got, forbidden) {
			t.Errorf("bare launch must start a NEW session; %q is `forgectl resume`'s job: %v", forbidden, got)
		}
	}
	if !strings.Contains(stderr, "--allow-dangerously-skip-permissions") {
		t.Errorf("bare launch must banner its posture to stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "--effort high") {
		t.Errorf("the banner is the cheapest way to eyeball the resolved effort, got %q", stderr)
	}
	if strings.Contains(stdout, "claude ") {
		t.Errorf("banner leaked into stdout: %q", stdout)
	}
}

// hostileConfigTemplate is nativeConfigTemplate with a model carrying a live
// SGR escape, a DEL, and a single-byte C1 CSI. Profile.Validate allowlists
// effort and the Codex fields; model is not among them, so this reaches the
// banner verbatim.
const hostileConfigTemplate = `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[launch.project]]
match = "%s"
model = "son\u001B[31mnet\u007F\u009BA"
`

// TestIntegration_BareLaunch_SanitizesBannerFromConfig is the end-to-end half
// of #243: config.toml is the untrusted input, `forgectl launch` is the whole
// pipe, and stderr is the terminal. The unit tests in internal/launch prove
// Banner sanitizes; this proves nothing downstream of it un-sanitizes.
func TestIntegration_BareLaunch_SanitizesBannerFromConfig(t *testing.T) {
	h := newHarness(t)
	cfgPath := childConfigPath(h.base)
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(hostileConfigTemplate, h.cwd)), 0o644); err != nil {
		t.Fatalf("write hostile config.toml: %v", err)
	}

	_, stderr := h.run(t)

	if !strings.Contains(stderr, "son") || !strings.Contains(stderr, "net") {
		t.Fatalf("banner did not render the configured model at all: %q", stderr)
	}
	assertInert(t, stderr)
}
