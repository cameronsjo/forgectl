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
// tools (docker/chronicle) and none of the bench dir env vars set — so every
// component deterministically resolves to not-configured.
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
	binDir := t.TempDir() // empty: no docker/chronicle on PATH
	writeNoBenchConfig(t, base)

	cmd := exec.Command(builtBinPath, "bench", "status", "--json")
	cmd.Env = benchStatusEnv(base, binDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bench status --json exited non-zero: %v\nstderr:\n%s", err, stderr.String())
	}

	var report map[string]struct{ State string }
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode JSON: %v\nstdout:\n%s", err, stdout.String())
	}
	if len(report) != 2 {
		t.Fatalf("JSON component count = %d, want exactly 2 (hearth and chronicle); report = %s", len(report), stdout.String())
	}
	for _, name := range []string{"hearth", "chronicle"} {
		component, ok := report[name]
		if !ok {
			t.Errorf("JSON report missing %q; report = %s", name, stdout.String())
			continue
		}
		state := component.State
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
	for _, want := range []string{"hearth", "chronicle"} {
		if !strings.Contains(out, want) {
			t.Errorf("human card missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "fl"+"ux") {
		t.Errorf("human card names retired board component; got:\n%s", out)
	}
}

func TestIntegration_BenchHelp_OmitsRetiredBoard(t *testing.T) {
	for _, args := range [][]string{{"bench", "--help"}, {"bench", "status", "--help"}} {
		cmd := exec.Command(builtBinPath, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s exited non-zero: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
		}
		if strings.Contains(strings.ToLower(stdout.String()), "fl"+"ux") {
			t.Errorf("%s help names retired board component; got:\n%s", strings.Join(args, " "), stdout.String())
		}
	}
}
