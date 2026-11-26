package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConsumeLaunchFlags(t *testing.T) {
	rest, f, err := consumeLaunchFlags([]string{"--role", "cos", "--dry-run", "--here", "--allow-guarded-plugin-disable", "--", "--resume", "abc"})
	if err != nil {
		t.Fatalf("consumeLaunchFlags: %v", err)
	}
	if f.role != "cos" || !f.dryRun || !f.here || !f.allowGuardedPluginDisable {
		t.Errorf("unexpected flags: %+v", f)
	}
	if !slices.Equal(rest, []string{"--", "--resume", "abc"}) {
		t.Errorf("rest = %v, want [-- --resume abc]", rest)
	}

	// Stops at -- so harness flags after -- are not consumed
	rest, f, err = consumeLaunchFlags([]string{"--", "--role", "not-ours"})
	if err != nil {
		t.Fatalf("after --: %v", err)
	}
	if f.role != "" || !slices.Equal(rest, []string{"--", "--role", "not-ours"}) {
		t.Errorf("got flags=%+v rest=%v", f, rest)
	}

	// --role=name syntax
	rest, f, err = consumeLaunchFlags([]string{"--role=cos", "-p", "hi"})
	if err != nil {
		t.Fatalf("--role=cos: %v", err)
	}
	if f.role != "cos" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("got flags=%+v rest=%v", f, rest)
	}

	// Missing value fails
	if _, _, err := consumeLaunchFlags([]string{"--role"}); err == nil {
		t.Error("expected error for bare --role")
	}
	if _, _, err := consumeLaunchFlags([]string{"--role="}); err == nil {
		t.Error("expected error for empty --role=")
	}
}

func TestLaunchRole_DryRunResumeAndWhich(t *testing.T) {
	tmp := t.TempDir()
	cfgHome := filepath.Join(tmp, "xdg")
	appDir := filepath.Join(cfgHome, "forgectl")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	charter := filepath.Join(tmp, "charter.md")
	if err := os.WriteFile(charter, []byte("# Role Charter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeClaude := filepath.Join(tmp, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeClaude, 0o700); err != nil {
		t.Fatal(err)
	}
	configTOML := "[launch.defaults]\nbinary_path = " + jsonString(t, fakeClaude) + "\n\n" +
		"[launch.roles.chief-of-staff]\n" +
		"charter_file = " + jsonString(t, charter) + "\n" +
		"posture = \"autonomous\"\n" +
		"exclude_claude_md = true\n" +
		"session_name = \"chief-of-staff\"\n"
	if err := os.WriteFile(filepath.Join(appDir, "config.toml"), []byte(configTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// macOS uses ~/Library/Application Support/forgectl/config.toml when HOME is set;
	// write both XDG and macOS paths under our temp HOME so config.Load finds it on any OS.
	macAppDir := filepath.Join(tmp, "Library", "Application Support", "forgectl")
	if err := os.MkdirAll(macAppDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(macAppDir, "config.toml"), []byte(configTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	runCLI := func(args ...string) (string, string, error) {
		cmd := exec.CommandContext(context.Background(), builtBinPath, args...) //nolint:gosec // G204: built test binary
		cmd.Dir = tmp
		cmd.Env = []string{
			"HOME=" + tmp,
			"XDG_CONFIG_HOME=" + cfgHome,
			"PATH=" + os.Getenv("PATH"),
			"TERM=xterm-256color",
		}
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		err := cmd.Run()
		return out.String(), errBuf.String(), err
	}

	// 1. forgectl launch --role chief-of-staff --dry-run -- --resume sess-1
	out, stderr, err := runCLI("launch", "--role", "chief-of-staff", "--dry-run", "--", "--resume", "sess-1")
	if err != nil {
		t.Fatalf("launch --role --dry-run failed: %v\nstderr: %s", err, stderr)
	}
	for _, want := range []string{
		"dry_run=true",
		"role=chief-of-staff",
		"posture=autonomous",
		"env=FORGECTL_ROLE=chief-of-staff",
		"env=FORGECTL_POSTURE=autonomous",
		"--append-system-prompt-file " + charter,
		"--name chief-of-staff",
		"--settings ",
		"--resume sess-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	// 2. forgectl launch which --role chief-of-staff --json
	out, stderr, err = runCLI("launch", "which", "--role", "chief-of-staff", "--json")
	if err != nil {
		t.Fatalf("launch which --role --json failed: %v\nstderr: %s", err, stderr)
	}
	var whichDoc struct {
		Role *struct {
			Name             string   `json:"name"`
			Posture          string   `json:"posture"`
			CharterFile      string   `json:"charter_file"`
			ExcludeClaudeMD  bool     `json:"exclude_claude_md"`
			ClaudeMDExcludes []string `json:"claude_md_excludes"`
		} `json:"role"`
	}
	if err := json.Unmarshal([]byte(out), &whichDoc); err != nil {
		t.Fatalf("unmarshal launch which --json: %v\nraw: %s", err, out)
	}
	if whichDoc.Role == nil {
		t.Fatalf("expected role object in launch which --json, got:\n%s", out)
	}
	if whichDoc.Role.Name != "chief-of-staff" || whichDoc.Role.Posture != "autonomous" || whichDoc.Role.CharterFile != charter || !whichDoc.Role.ExcludeClaudeMD {
		t.Errorf("unexpected role json: %+v", whichDoc.Role)
	}

	// 3. Unknown role exits non-zero
	if _, _, err := runCLI("launch", "which", "--role", "nonexistent"); err == nil {
		t.Error("expected launch which --role nonexistent to fail")
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
