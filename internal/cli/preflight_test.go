package cli

// Test plan for preflight.go
//
// newPreflightCmdForConfig / RunE (Classification: CLI wiring — temp HOME +
// temp cwd fixtures, mirrors resolveDocsRoots' t.Setenv("HOME")/t.Chdir()
// pattern)
//   [x] Happy: aligned project → exit 0, "aligned" on stdout
//   [x] Happy: misaligned project, no --apply → exit 1, change-set printed,
//       no file written
//   [x] Happy: --apply on a misaligned project → exit 0, writes
//       .claude/settings.local.json with the complete target set
//   [x] Happy: --json emits the machine-readable report
//   [x] Sad: no locatable catalog → exit 2

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/preflight"
)

// preflightFixtureCatalog declares one core-tier plugin — enough for the
// CLI-level tests, which exercise wiring and exit codes, not catalog
// parsing breadth (that's catalog_test.go's job).
const preflightFixtureCatalog = `## cadence · tier: core · id: cadence@workbench
`

// setupPreflightHome writes a minimal catalog directly at the configured
// override path and returns a PreflightConfig pointing at it, so tests
// don't need to fake installed_plugins.json or the cache glob.
func setupPreflightHome(t *testing.T) config.PreflightConfig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	catalogPath := filepath.Join(home, "catalog.md")
	if err := os.WriteFile(catalogPath, []byte(preflightFixtureCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.PreflightConfig{CatalogPath: catalogPath}
}

func runPreflight(t *testing.T, cfg config.PreflightConfig, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newPreflightCmdForConfig(cfg)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestPreflight_Aligned(t *testing.T) {
	cfg := setupPreflightHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := preflight.WriteLocal(dir, map[string]bool{"cadence@workbench": true}, nil); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runPreflight(t, cfg)
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}
	if !bytes.Contains([]byte(stdout), []byte("aligned")) {
		t.Errorf("stdout = %q, want it to report aligned", stdout)
	}
}

func TestPreflight_Misaligned_NoApply(t *testing.T) {
	cfg := setupPreflightHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	stdout, _, err := runPreflight(t, cfg)
	if err == nil {
		t.Fatal("Execute() = nil error, want exit 1 for a misaligned project")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
	if !bytes.Contains([]byte(stdout), []byte("cadence@workbench")) {
		t.Errorf("stdout = %q, want the change-set to name cadence@workbench", stdout)
	}
	if _, statErr := os.Stat(preflight.LocalPath(dir)); !os.IsNotExist(statErr) {
		t.Error("a dry-run (no --apply) must not write settings.local.json")
	}
}

func TestPreflight_Apply(t *testing.T) {
	cfg := setupPreflightHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	_, stderr, err := runPreflight(t, cfg, "--apply")
	if err != nil {
		t.Fatalf("Execute() with --apply: %v (exit %d)", err, ExitCode(err))
	}
	if !bytes.Contains([]byte(stderr), []byte("wrote")) {
		t.Errorf("stderr = %q, want a confirmation that it wrote the local settings file", stderr)
	}

	doc, err := preflight.ReadDocument(preflight.LocalPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.EnabledPlugins["cadence@workbench"] {
		t.Errorf("written EnabledPlugins = %v, want cadence@workbench=true", doc.EnabledPlugins)
	}

	// Re-running immediately must now report aligned.
	stdout, _, err := runPreflight(t, cfg)
	if err != nil {
		t.Errorf("re-run after --apply: %v (exit %d)", err, ExitCode(err))
	}
	if !bytes.Contains([]byte(stdout), []byte("aligned")) {
		t.Errorf("re-run stdout = %q, want aligned", stdout)
	}
}

func TestPreflight_JSON(t *testing.T) {
	cfg := setupPreflightHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	stdout, _, err := runPreflight(t, cfg, "--json")
	if err == nil {
		t.Fatal("Execute() = nil error, want exit 1 for a misaligned project")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}

	var report preflightJSON
	if jsonErr := json.Unmarshal([]byte(stdout), &report); jsonErr != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", jsonErr, stdout)
	}
	if report.Aligned {
		t.Error("report.Aligned = true, want false")
	}
	if len(report.Enable) != 1 || report.Enable[0] != "cadence@workbench" {
		t.Errorf("report.Enable = %v, want [cadence@workbench]", report.Enable)
	}
}

func TestPreflight_NoLocatableCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, err := runPreflight(t, config.PreflightConfig{})
	if err == nil {
		t.Fatal("Execute() = nil error, want exit 2 when no catalog can be located")
	}
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode = %d, want 2", ExitCode(err))
	}
}
