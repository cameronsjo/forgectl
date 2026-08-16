package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the parity pin for `forgectl launch`'s ordinary path: it records
// what the harness process actually received — argv, cwd, environment — and
// what forgectl printed to stderr while getting there, then asserts every one
// of those byte-for-byte.
//
// It exists because the resolve → exec path was refactored behind
// launch.BuildInvocation (forgectl#329), and a refactor of that path is exactly
// where a banner line or an injected env var goes missing without any test
// going red. The other integration tests here assert *shapes* — "the banner
// contains --effort high", "OTEL_EXPORTER is otlp". A dropped `--ide`, a banner
// that lost its arrow, or an env entry that stopped being sorted all survive a
// contains-check. So these assert the whole recorded value.
//
// Deliberately a characterization test: it passed before the refactor and must
// pass after, unchanged. A diff here is a behavior change to justify, not a
// test to update.

// parityStub records everything the harness process can observe about how it
// was invoked. `pwd -P` rather than $PWD: syscall.Exec does not set PWD, so the
// variable would be inherited from whoever launched the test rather than
// reporting the cwd claude was actually handed.
const parityStub = `#!/usr/bin/env bash
{
  echo "CWD=$(pwd -P)"
  for a in "$@"; do echo "ARG=$a"; done
  env | LC_ALL=C sort | sed 's/^/ENV=/'
} > "$FORGECTL_TEST_OUT"
`

// parityHarness is one isolated `forgectl launch` invocation whose harness
// binary is parityStub. Separate from `harness` because that one's stub records
// four named env vars; this needs the whole environment and the cwd.
type parityHarness struct {
	bin     string
	cwd     string
	binDir  string
	outFile string
	base    string
	env     []string
}

// newParityHarness installs parityStub under both `claude` and `codex` (which
// binary runs is the profile's call, and installing both keeps a
// harness-selection bug from presenting as "binary not found") and writes
// configBody, which must carry a %s for the project match path.
func newParityHarness(t *testing.T, configBody string) *parityHarness {
	t.Helper()

	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve symlinks on temp cwd: %v", err)
	}
	binDir := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "harness.out")
	base := t.TempDir()

	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(parityStub), 0o755); err != nil {
			t.Fatalf("write parity stub %s: %v", name, err)
		}
	}

	cfgPath := childConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(configBody, cwd)), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	return &parityHarness{
		bin:     builtBinPath,
		cwd:     cwd,
		binDir:  binDir,
		outFile: outFile,
		base:    base,
		env: []string{
			// binDir first so the stubs win, but the real PATH stays behind it:
			// the stub is a bash script and shells out to env/sort/sed.
			"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + base,
			"XDG_CONFIG_HOME=" + testXDGConfigHome(base),
			"FORGECTL_TEST_OUT=" + outFile,
			// An ordinary inherited entry with no launcher meaning. Its survival
			// is the half of env parity the injected keys cannot prove: a merge
			// that built the child env from scratch instead of overlaying would
			// pass every assertion about OTEL_* and drop this one silently.
			"FORGECTL_PARITY_INHERITED=carried-through",
		},
	}
}

// run execs `forgectl launch <args…>`, failing on a non-zero exit, and returns
// (stdout, stderr).
func (h *parityHarness) run(t *testing.T, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(h.bin, append([]string{"launch"}, args...)...)
	cmd.Dir = h.cwd
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("forgectl launch %v: %v\nstderr:\n%s", args, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// recorded parses the parityStub output into the cwd, argv, and environment the
// harness process received.
func (h *parityHarness) recorded(t *testing.T) (cwd string, argv []string, env map[string]string) {
	t.Helper()
	data, err := os.ReadFile(h.outFile)
	if err != nil {
		t.Fatalf("read parity out file: %v", err)
	}
	env = map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "CWD="):
			cwd = strings.TrimPrefix(line, "CWD=")
		case strings.HasPrefix(line, "ARG="):
			argv = append(argv, strings.TrimPrefix(line, "ARG="))
		case strings.HasPrefix(line, "ENV="):
			k, v, _ := strings.Cut(strings.TrimPrefix(line, "ENV="), "=")
			env[k] = v
		}
	}
	return cwd, argv, env
}

// assertArgv compares recorded argv to want element by element, reporting the
// first divergence with its index — a plain slice dump makes a single dropped
// flag in a fourteen-token argv genuinely hard to spot.
func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) == len(want) {
		diverged := false
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
				diverged = true
			}
		}
		if !diverged {
			return
		}
	} else {
		t.Errorf("argv has %d tokens, want %d", len(got), len(want))
	}
	t.Errorf("full argv:\n got: %q\nwant: %q", got, want)
}

// parityClaudeConfig pins every knob the Claude argv builder reads, so the
// recorded argv exercises each branch rather than defaulting through them:
// permission mode, allow_danger, model, a derived effort, and an add_dir. The
// profile env entry proves the profile layer of the env merge.
const parityClaudeConfig = `[launch.defaults]
model = "opus"
permission_mode = "plan"
allow_danger = true

[[launch.project]]
match = "%s"
model = "sonnet"
env = { FORGECTL_PARITY_PROFILE = "profile-value" }
add_dir = ["/tmp/parity-shared"]
`

// TestParity_ClaudeOrdinaryLaunch pins the whole observable result of a bare
// `forgectl launch` under a Claude profile.
func TestParity_ClaudeOrdinaryLaunch(t *testing.T) {
	h := newParityHarness(t, parityClaudeConfig)
	stdout, stderr := h.run(t)

	gotCWD, gotArgv, gotEnv := h.recorded(t)

	wantArgv := []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--ide", "--exclude-dynamic-system-prompt-sections",
		"--model", "sonnet",
		"--effort", "high",
		"--add-dir", "/tmp/parity-shared",
	}
	assertArgv(t, gotArgv, wantArgv)

	if gotCWD != h.cwd {
		t.Errorf("harness cwd = %q, want %q — launch execs in place and must not move", gotCWD, h.cwd)
	}

	// The full banner line, not a substring. The arrow, the binary name, and the
	// single-space joining are all part of what an operator reads to confirm the
	// posture before the session starts.
	wantBanner := "→ claude " + strings.Join(wantArgv, " ") + "\n"
	if stderr != wantBanner {
		t.Errorf("stderr = %q, want exactly %q", stderr, wantBanner)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty — anything here breaks `forgectl launch … | jq`", stdout)
	}

	assertEnvEntry(t, gotEnv, "FORGECTL_PARITY_PROFILE", "profile-value")
	assertEnvEntry(t, gotEnv, "FORGECTL_PARITY_INHERITED", "carried-through")
	assertEnvEntry(t, gotEnv, "HOME", h.base)
}

// parityCodexConfig is the Codex analogue: approval policy and sandbox are the
// Codex-only knobs, and no --effort is emitted because Codex has no such flag.
const parityCodexConfig = `[launch.defaults]
harness = "codex"
approval_policy = "never"
sandbox = "read-only"

[[launch.project]]
match = "%s"
env = { FORGECTL_PARITY_PROFILE = "profile-value" }
add_dir = ["/tmp/parity-shared"]
`

// TestParity_CodexOrdinaryLaunch is the Codex half. It is a separate test
// rather than a table row because the two harnesses differ in banner writer
// (HarnessBanner vs Banner) as well as argv, and a shared table would have to
// carry both as parameters — at which point the table is the assertion.
func TestParity_CodexOrdinaryLaunch(t *testing.T) {
	h := newParityHarness(t, parityCodexConfig)
	stdout, stderr := h.run(t)

	gotCWD, gotArgv, gotEnv := h.recorded(t)

	wantArgv := []string{
		"--ask-for-approval", "never",
		"--sandbox", "read-only",
		"--add-dir", "/tmp/parity-shared",
	}
	assertArgv(t, gotArgv, wantArgv)

	if gotCWD != h.cwd {
		t.Errorf("harness cwd = %q, want %q — launch execs in place and must not move", gotCWD, h.cwd)
	}

	wantBanner := "→ codex " + strings.Join(wantArgv, " ") + "\n"
	if stderr != wantBanner {
		t.Errorf("stderr = %q, want exactly %q", stderr, wantBanner)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}

	assertEnvEntry(t, gotEnv, "FORGECTL_PARITY_PROFILE", "profile-value")
	assertEnvEntry(t, gotEnv, "FORGECTL_PARITY_INHERITED", "carried-through")
}

// TestParity_ClaudeBuilderLaunch pins the passthrough posture: user args land
// verbatim after the injected flags, the interactive-only flags are absent, and
// this branch prints no banner at all.
func TestParity_ClaudeBuilderLaunch(t *testing.T) {
	h := newParityHarness(t, parityClaudeConfig)
	stdout, stderr := h.run(t, "-p", "summarize this")

	_, gotArgv, _ := h.recorded(t)

	assertArgv(t, gotArgv, []string{
		"--permission-mode", "plan",
		"--allow-dangerously-skip-permissions",
		"--model", "sonnet",
		"--effort", "high",
		"--add-dir", "/tmp/parity-shared",
		"-p", "summarize this",
	})

	if stderr != "" {
		t.Errorf("stderr = %q, want empty — the builder path banners nothing", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// assertEnvEntry reports a missing or wrong environment entry as one message
// naming the key, rather than leaving the reader to diff two whole maps.
func assertEnvEntry(t *testing.T, env map[string]string, key, want string) {
	t.Helper()
	got, ok := env[key]
	switch {
	case !ok:
		t.Errorf("harness environment has no %s; want %q", key, want)
	case got != want:
		t.Errorf("harness environment %s = %q, want %q", key, got, want)
	}
}
