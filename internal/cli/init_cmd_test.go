package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initHarness wires one isolated `forgectl init` invocation: an isolated
// HOME/XDG_CONFIG_HOME so the test never touches a real config.toml. Unlike
// harness (launch_test.go), run does NOT prepend "launch" — `init` is a
// top-level command.
type initHarness struct {
	bin  string
	base string
	env  []string
}

func newInitHarness(t *testing.T) *initHarness {
	t.Helper()
	base := t.TempDir()
	return &initHarness{
		bin:  builtBinPath,
		base: base,
		env: []string{
			"HOME=" + base,
			"XDG_CONFIG_HOME=" + base,
			"PATH=" + os.Getenv("PATH"),
		},
	}
}

// run execs `forgectl init <args…>`, failing the test on a non-zero exit.
// Returns (stdout, stderr).
func (h *initHarness) run(t *testing.T, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(h.bin, append([]string{"init"}, args...)...)
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("forgectl init %v: %v\nstderr:\n%s", args, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

func (h *initHarness) configPath() string {
	return childConfigPath(h.base)
}

// TestIntegration_Init_FreshWritesEverySection covers the empty-file case: a
// fresh `forgectl init` must write the host-scalar preamble plus every
// config.Config struct-kind section, each with its annotated template.
func TestIntegration_Init_FreshWritesEverySection(t *testing.T) {
	h := newInitHarness(t)
	stdout, _ := h.run(t)

	for _, label := range []string{
		"host scalars", "launch", "workflow", "net", "bench",
		"docker", "clean", "sessions", "review", "docs",
	} {
		if !strings.Contains(stdout, "added:            "+label) {
			t.Errorf("stdout missing \"added: %s\"; got:\n%s", label, stdout)
		}
	}

	data, err := os.ReadFile(h.configPath())
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"no_icons  = false", "log_level = \"off\"", "log_file  = \"\"",
		"[launch.defaults]", "[workflow]", "[net]", "[bench]",
		"[docker]", "[clean]", "[sessions]", "[review]", "[docs]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml missing %q after fresh init; got:\n%s", want, body)
		}
	}

	// The host-scalar preamble must be the very first bytes: it's a bare-key
	// block with no table header, so it only parses at document root when it
	// precedes every [section].
	if !strings.HasPrefix(body, "# ── forgectl: global settings") {
		t.Errorf("config.toml does not start with the host-scalar preamble; first line: %q", strings.SplitN(body, "\n", 2)[0])
	}
}

// TestIntegration_Init_PreservesExistingSection covers the append-if-absent
// contract's core case: a hand-written [net] section (with its own comment)
// must survive byte-for-byte, while every missing section — including the
// host-scalar preamble, which must still land ahead of the hand-written
// [net] table — gets added around it.
func TestIntegration_Init_PreservesExistingSection(t *testing.T) {
	h := newInitHarness(t)
	cfgPath := h.configPath()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	handWritten := "# my hand-written net override, do not touch\n[net]\nprobe_host = \"10.0.0.1\"\nprobe_port = 8080\n"
	if err := os.WriteFile(cfgPath, []byte(handWritten), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	stdout, _ := h.run(t)

	if !strings.Contains(stdout, "already present: net") {
		t.Errorf("stdout missing \"already present: net\"; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "added:            net") {
		t.Errorf("stdout claims [net] was added; it was already present; got:\n%s", stdout)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config.toml after init: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, handWritten) {
		t.Errorf("config.toml does not contain the hand-written [net] block verbatim after init; got:\n%s", body)
	}
	if strings.Count(body, "[net]") != 1 {
		t.Errorf("config.toml has %d [net] headers after init, want exactly 1 (no duplicate); got:\n%s", strings.Count(body, "[net]"), body)
	}
	// The host-scalar preamble is missing from the hand-written file, so init
	// must prepend it — ahead of the hand-written [net] block, not folded
	// into it.
	netIdx := strings.Index(body, "[net]")
	scalarIdx := strings.Index(body, "no_icons")
	if scalarIdx == -1 || netIdx == -1 || scalarIdx > netIdx {
		t.Errorf("host-scalar preamble must precede the pre-existing [net] block; scalarIdx=%d netIdx=%d, body:\n%s", scalarIdx, netIdx, body)
	}
}

// TestIntegration_Init_Idempotent covers the no-duplicate-on-rerun contract:
// a second full run over an already-fully-scaffolded config.toml must be a
// no-op — every section reported "already present", byte-identical file.
func TestIntegration_Init_Idempotent(t *testing.T) {
	h := newInitHarness(t)
	h.run(t)

	before, err := os.ReadFile(h.configPath())
	if err != nil {
		t.Fatalf("read config.toml after first init: %v", err)
	}

	stdout, _ := h.run(t)
	for _, label := range []string{
		"host scalars", "launch", "workflow", "net", "bench",
		"docker", "clean", "sessions", "review", "docs",
	} {
		if !strings.Contains(stdout, "already present: "+label) {
			t.Errorf("second run stdout missing \"already present: %s\"; got:\n%s", label, stdout)
		}
	}
	if !strings.Contains(stdout, "0 section(s) added") {
		t.Errorf("second run stdout does not report 0 sections added; got:\n%s", stdout)
	}

	after, err := os.ReadFile(h.configPath())
	if err != nil {
		t.Fatalf("read config.toml after second init: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("config.toml changed on a second, idempotent init run.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
