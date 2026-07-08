package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// benchStatusEnv builds a child env with a controlled PATH holding no bench
// tools (docker/chronicle/flux) and none of the bench dir/board env vars set —
// so every component deterministically resolves to not-configured.
func benchStatusEnv(base, binDir string) []string {
	return []string{
		"PATH=" + binDir,
		"HOME=" + base,
		"XDG_CONFIG_HOME=" + base,
	}
}

// writeNoBenchConfig writes a config.toml with no [bench] section under a fake
// config base.
func writeNoBenchConfig(t *testing.T, base string) {
	t.Helper()
	cfgPath := childConfigPath(base)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte("log_level = \"off\"\n"), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

func TestIntegration_BenchStatus_UnconfiguredJSON(t *testing.T) {
	base := t.TempDir()
	binDir := t.TempDir() // empty: no docker/chronicle/flux on PATH
	writeNoBenchConfig(t, base)

	cmd := exec.Command(builtBinPath, "bench", "status", "--json")
	cmd.Env = benchStatusEnv(base, binDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bench status --json exited non-zero: %v\nstderr:\n%s", err, stderr.String())
	}

	var report struct {
		Hearth    struct{ State string } `json:"hearth"`
		Chronicle struct{ State string } `json:"chronicle"`
		Flux      struct{ State string } `json:"flux"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode JSON: %v\nstdout:\n%s", err, stdout.String())
	}
	for name, state := range map[string]string{
		"hearth": report.Hearth.State, "chronicle": report.Chronicle.State, "flux": report.Flux.State,
	} {
		if state != "not-configured" {
			t.Errorf("%s state = %q, want not-configured", name, state)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("--json run wrote to stderr: %q", stderr.String())
	}
}

func TestIntegration_BenchStatus_HumanCard(t *testing.T) {
	base := t.TempDir()
	binDir := t.TempDir()
	writeNoBenchConfig(t, base)

	cmd := exec.Command(builtBinPath, "bench", "status")
	cmd.Env = benchStatusEnv(base, binDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bench status exited non-zero: %v\nstderr:\n%s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"hearth", "chronicle", "flux"} {
		if !strings.Contains(out, want) {
			t.Errorf("human card missing %q; got:\n%s", want, out)
		}
	}
}
