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

func TestQuarantineCommands_RejectOverlappingTargetsWithoutOutputOrMutation(t *testing.T) {
	want := `quarantine targets ".cursor" (outer) and ".cursor/rules" (inner) overlap: replace the inner entry, do not join it`
	commands := []struct {
		name string
		args []string
	}{
		{"bare hide", nil},
		{"explicit hide", []string{"hide"}},
		{"hide dry-run", []string{"hide", "--dry-run"}},
		{"restore", []string{"restore"}},
		{"restore dry-run", []string{"restore", "--dry-run"}},
		{"status", []string{"status"}},
	}
	for _, command := range commands {
		for _, targets := range [][]string{{".cursor/rules", ".cursor"}, {".cursor", ".cursor/rules"}} {
			t.Run(command.name+"/"+strings.Join(targets, "-then-"), func(t *testing.T) {
				root := t.TempDir()
				writeQuarantineFixture(t, filepath.Join(root, ".cursor", "rules", "r.mdc"), "rules")
				args := append([]string{}, command.args...)
				args = append(args, "--root", root)
				for _, target := range targets {
					args = append(args, "--targets", target)
				}
				cmd := newQuarantineCmd(newQuarantineTestClient())
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(new(bytes.Buffer))
				cmd.SetArgs(args)

				err := cmd.ExecuteContext(context.Background())
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %q", err, want)
				}
				if out.Len() != 0 {
					t.Fatalf("command emitted partial output on refusal: %q", out.String())
				}
				content, readErr := os.ReadFile(filepath.Join(root, ".cursor", "rules", "r.mdc"))
				if readErr != nil || string(content) != "rules" {
					t.Fatalf("command mutated source on refusal: content=%q err=%v", content, readErr)
				}
			})
		}
	}
}

func TestQuarantineCommands_RejectDestinationSourceChainsExactly(t *testing.T) {
	tests := []struct {
		name    string
		scheme  string
		targets []string
		want    string
	}{
		{"suffix", "suffix", []string{"foo", "foo.quarantined"}, `quarantine moves for "foo" and "foo.quarantined" conflict: destination "foo.quarantined" is another source`},
		{"prefix", "prefix", []string{"foo", "_foo"}, `quarantine moves for "_foo" and "foo" conflict: destination "_foo" is another source`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, target := range tc.targets {
				writeQuarantineFixture(t, filepath.Join(root, target), target)
			}
			cmd := newQuarantineCmd(newQuarantineTestClient())
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(new(bytes.Buffer))
			args := []string{"hide", "--dry-run", "--root", root, "--scheme", tc.scheme}
			for _, target := range tc.targets {
				args = append(args, "--targets", target)
			}
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(context.Background()); err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("dry-run emitted output on graph refusal: %q", out.String())
			}
		})
	}
}

func TestQuarantineHide_DefaultTargetsQuarantineUniqueSymlinkedMCPCarrier(t *testing.T) {
	for _, scheme := range []string{"prefix", "suffix"} {
		root := t.TempDir()
		writeQuarantineFixture(t, filepath.Join(root, "real", "mcp.json"), "carrier")
		if err := os.Symlink("real", filepath.Join(root, ".gemini")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		cmd := newQuarantineCmd(newQuarantineTestClient())
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"hide", "--root", root, "--scheme", scheme})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("scheme=%s hide: %v", scheme, err)
		}
		if strings.Contains(out.String(), "no instruction files found") {
			t.Fatalf("scheme=%s reported false success: %q", scheme, out.String())
		}
		decorated := "_mcp.json"
		if scheme == "suffix" {
			decorated = "mcp.json.quarantined"
		}
		if _, err := os.Lstat(filepath.Join(root, "real", decorated)); err != nil {
			t.Fatalf("scheme=%s carrier not quarantined: %v", scheme, err)
		}
	}
}

func TestQuarantineHide_DefaultTargetsRejectEscapingSymlinkedMCPCarrier(t *testing.T) {
	external := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(external, "mcp.json"), "external")
	for _, scheme := range []string{"prefix", "suffix"} {
		root := t.TempDir()
		if err := os.Symlink(external, filepath.Join(root, ".gemini")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		cmd := newQuarantineCmd(newQuarantineTestClient())
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"hide", "--root", root, "--scheme", scheme})
		err := cmd.ExecuteContext(context.Background())
		if err == nil || !strings.Contains(err.Error(), `quarantine target ".gemini/mcp.json" escapes root through its parent`) {
			t.Fatalf("scheme=%s error = %v", scheme, err)
		}
		if out.Len() != 0 {
			t.Fatalf("scheme=%s emitted false-success output: %q", scheme, out.String())
		}
		if got, readErr := os.ReadFile(filepath.Join(external, "mcp.json")); readErr != nil || string(got) != "external" {
			t.Fatalf("scheme=%s external carrier mutated: content=%q err=%v", scheme, got, readErr)
		}
	}
}

func TestQuarantine_DefaultTargetsCoveredDescendantAliasHideRestore(t *testing.T) {
	for _, scheme := range []string{"prefix", "suffix"} {
		root := t.TempDir()
		writeQuarantineFixture(t, filepath.Join(root, ".claude", "sub", "mcp.json"), "covered descendant")
		if err := os.Symlink(filepath.Join(".claude", "sub"), filepath.Join(root, ".gemini")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		hide := newQuarantineCmd(newQuarantineTestClient())
		var hideOut bytes.Buffer
		hide.SetOut(&hideOut)
		hide.SetErr(new(bytes.Buffer))
		hide.SetArgs([]string{"hide", "--root", root, "--scheme", scheme})
		if err := hide.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("scheme=%s hide: %v", scheme, err)
		}
		decorated := "_.claude"
		if scheme == "suffix" {
			decorated = ".claude.quarantined"
		}
		if _, err := os.Lstat(filepath.Join(root, decorated, "sub", "mcp.json")); err != nil {
			t.Fatalf("scheme=%s covered root not hidden: %v", scheme, err)
		}

		restore := newQuarantineCmd(newQuarantineTestClient())
		var restoreOut bytes.Buffer
		restore.SetOut(&restoreOut)
		restore.SetErr(new(bytes.Buffer))
		restore.SetArgs([]string{"restore", "--root", root, "--scheme", scheme})
		if err := restore.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("scheme=%s restore: %v", scheme, err)
		}
		if got, err := os.ReadFile(filepath.Join(root, ".claude", "sub", "mcp.json")); err != nil || string(got) != "covered descendant" {
			t.Fatalf("scheme=%s restored content=%q err=%v", scheme, got, err)
		}
	}
}

func TestQuarantineCommands_RejectFinalSymlinkOuterAndDescendantWithoutOutputOrMutation(t *testing.T) {
	commands := []struct {
		name string
		args []string
	}{
		{"bare hide", nil},
		{"explicit hide", []string{"hide"}},
		{"hide dry-run", []string{"hide", "--dry-run"}},
		{"restore", []string{"restore"}},
		{"restore dry-run", []string{"restore", "--dry-run"}},
		{"status", []string{"status"}},
	}
	want := `quarantine targets "alias" (outer) and "alias/child" (inner) overlap: replace the inner entry, do not join it`
	for _, scheme := range []string{"prefix", "suffix"} {
		for _, command := range commands {
			for _, targets := range [][]string{{"alias", "alias/child"}, {"alias/child", "alias"}} {
				t.Run(scheme+"/"+command.name+"/"+strings.Join(targets, "-then-"), func(t *testing.T) {
					root := t.TempDir()
					writeQuarantineFixture(t, filepath.Join(root, "real", "child", "sentinel"), "unchanged")
					if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
						t.Skipf("symlink unsupported: %v", err)
					}
					args := append([]string{}, command.args...)
					args = append(args, "--root", root, "--scheme", scheme)
					for _, target := range targets {
						args = append(args, "--targets", target)
					}
					cmd := newQuarantineCmd(newQuarantineTestClient())
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(new(bytes.Buffer))
					cmd.SetArgs(args)
					err := cmd.ExecuteContext(context.Background())
					if err == nil || err.Error() != want {
						t.Fatalf("error = %v, want %q", err, want)
					}
					if out.Len() != 0 {
						t.Fatalf("command emitted output on refusal: %q", out.String())
					}
					if got, readErr := os.ReadFile(filepath.Join(root, "real", "child", "sentinel")); readErr != nil || string(got) != "unchanged" {
						t.Fatalf("command mutated child on refusal: content=%q err=%v", got, readErr)
					}
					if info, statErr := os.Lstat(filepath.Join(root, "alias")); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("command mutated outer symlink: info=%v err=%v", info, statErr)
					}
				})
			}
		}
	}
}

func TestQuarantineHide_RelativeRootPlansNestedTarget(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	relRoot, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatalf("Rel root: %v", err)
	}
	writeQuarantineFixture(t, filepath.Join(root, "nested", "target"), "x")
	cmd := newQuarantineCmd(newQuarantineTestClient())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"hide", "--dry-run", "--root", relRoot, "--targets", "nested/target"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("relative-root dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "nested/target") {
		t.Fatalf("relative-root dry-run omitted target: %q", out.String())
	}
	if got, readErr := os.ReadFile(filepath.Join(root, "nested", "target")); readErr != nil || string(got) != "x" {
		t.Fatalf("relative-root dry-run mutated target: content=%q err=%v", got, readErr)
	}
}

func TestQuarantineStatus_NoPhantomNestedRowUnderCoveredRoot(t *testing.T) {
	root := t.TempDir()
	writeQuarantineFixture(t, filepath.Join(root, ".claude", "CLAUDE.md"), "covered")
	writeQuarantineFixture(t, filepath.Join(root, "packages", "api", "CLAUDE.md"), "uncovered")

	cmd := newQuarantineCmd(newQuarantineTestClient())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--root", root, "--scheme", "suffix"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	body := out.String()
	if strings.Contains(body, filepath.ToSlash(filepath.Join(".claude", "CLAUDE.md"))+":") {
		t.Fatalf("status reported a phantom nested row under covered .claude: %s", body)
	}
	for _, want := range []string{".claude: present", filepath.ToSlash(filepath.Join("packages", "api", "CLAUDE.md")) + ": present"} {
		if !strings.Contains(filepath.ToSlash(body), want) {
			t.Fatalf("status missing %q: %s", want, body)
		}
	}
}
