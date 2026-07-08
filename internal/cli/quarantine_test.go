package cli

// Test plan for quarantine.go
//
// newQuarantineCmd (Classification: API handler / cobra command, bare invoke)
//   [x] Happy: bare invoke (no subcommand) acts as hide on the default targets
//
// newQuarantineHideCmd (Classification: API handler / cobra command)
//   [x] Happy: hides the default target list under --root
//   [x] Happy: --targets overrides the default list
//   [x] Happy: --dry-run reports the planned move without renaming
//   [x] Happy: --scheme suffix renames with the .quarantined suffix
//   [x] Unhappy: an unknown --scheme value returns an error
//
// newQuarantineRestoreCmd (Classification: API handler / cobra command)
//   [x] Happy: restore renames a quarantined target back to its original name
//
// newQuarantineStatusCmd (Classification: API handler / cobra command)
//   [x] Happy: status reports present/quarantined/absent per target

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/quarantine"
)

func newQuarantineTestClient() *quarantine.Client {
	return quarantine.New(&exec.FakeRunner{})
}

func writeQuarantineFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestQuarantineCmd_BareInvoke_ActsAsHide(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "CLAUDE.md"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--root", root})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "_CLAUDE.md")); err != nil {
		t.Errorf("bare invoke should hide CLAUDE.md, stat err = %v", err)
	}
}

func TestQuarantineHideCmd_HidesDefaultTargets(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "CLAUDE.md"), "x")
	writeQuarantineFixture(t, filepath.Join(root, "AGENTS.md"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"hide", "--root", root})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"_CLAUDE.md", "_AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("%s should exist after hide, stat err = %v", want, err)
		}
	}
}

func TestQuarantineHideCmd_TargetsFlagOverridesDefaults(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "CLAUDE.md"), "x")
	writeQuarantineFixture(t, filepath.Join(root, "custom.txt"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"hide", "--root", root, "--targets", "custom.txt"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md should be untouched (not in --targets), stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "_custom.txt")); err != nil {
		t.Errorf("custom.txt should be hidden, stat err = %v", err)
	}
}

func TestQuarantineHideCmd_DryRunMakesNoFSChanges(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "CLAUDE.md"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"hide", "--root", root, "--dry-run"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Errorf("dry-run must not rename CLAUDE.md, stat err = %v", err)
	}
	if !strings.Contains(out.String(), "CLAUDE.md") {
		t.Errorf("dry-run output should mention the planned move: %q", out.String())
	}
}

func TestQuarantineHideCmd_SuffixScheme(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "CLAUDE.md"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"hide", "--root", root, "--scheme", "suffix"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md.quarantined")); err != nil {
		t.Errorf("suffix scheme should produce CLAUDE.md.quarantined, stat err = %v", err)
	}
}

func TestQuarantineHideCmd_UnknownSchemeErrors(t *testing.T) {
	root := t.TempDir()
	cmd := newQuarantineCmd(newQuarantineTestClient())
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"hide", "--root", root, "--scheme", "bogus"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unknown --scheme value, got nil")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error = %q, want it to mention scheme", err.Error())
	}
}

func TestQuarantineRestoreCmd_RenamesBack(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "_CLAUDE.md"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"restore", "--root", root, "--targets", "CLAUDE.md"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md should be restored, stat err = %v", err)
	}
}

func TestQuarantineStatusCmd_ReportsPerTargetState(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, "CLAUDE.md"), "x")
	writeQuarantineFixture(t, filepath.Join(root, "_AGENTS.md"), "x")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--root", root, "--targets", "CLAUDE.md", "--targets", "AGENTS.md", "--targets", "missing.md"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := out.String()
	for _, want := range []string{"CLAUDE.md: present", "AGENTS.md: quarantined", "missing.md: absent"} {
		if !strings.Contains(body, want) {
			t.Errorf("status output missing %q, got:\n%s", want, body)
		}
	}
}
