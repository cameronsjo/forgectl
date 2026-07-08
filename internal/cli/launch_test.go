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

// childConfigPath returns the OS-correct forgectl config.toml path under a
// fake HOME/XDG_CONFIG_HOME base, mirroring os.UserConfigDir()'s resolution:
// darwin ignores XDG and uses Library/Application Support; linux honors XDG.
func childConfigPath(base string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(base, "Library", "Application Support", "forgectl", "config.toml")
	}
	return filepath.Join(base, "forgectl", "config.toml")
}

// legacyConfigPath returns the legacy claunch.conf path under a fake base.
// config.LegacyLaunchPath honors $XDG_CONFIG_HOME directly on every OS (unlike
// os.UserConfigDir, which darwin ignores), so this is base/claunch/claunch.conf
// regardless of GOOS as long as XDG_CONFIG_HOME=base is set in the child env.
func legacyConfigPath(base string) string {
	return filepath.Join(base, "claunch", "claunch.conf")
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
		env: []string{
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + base,
			"XDG_CONFIG_HOME=" + base,
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
		env: []string{
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + base,
			"XDG_CONFIG_HOME=" + base,
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
		env: []string{
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + base,
			"XDG_CONFIG_HOME=" + base,
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

	for _, want := range []string{"sonnet", "plan", h.cwd} {
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
