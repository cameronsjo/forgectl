//go:build unix

package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	osExec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func createCLIProject(test *testing.T, root, worktree, name string, extra ...string) projectReport {
	test.Helper()
	arguments := []string{"project", "--root", root, "create", name, "--worktree", worktree, "--json"}
	output, stderr, code := runSurface(test, "", append(arguments, extra...)...)
	if code != 0 {
		test.Fatalf("create exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var report projectReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		test.Fatal(err)
	}
	if report.Version != project.Version || report.Revision == 0 || report.Project.ID == "" || report.Project.CoordinatorThreadID == "" {
		test.Fatalf("incomplete project response: %#v", report)
	}
	return report
}

func TestSurfaceProjectPersistsLogicalThreads(test *testing.T) {
	parent := test.TempDir()
	root, worktree := filepath.Join(parent, "registry"), test.TempDir()
	instructions := filepath.Join(parent, "instructions.md")
	brief := filepath.Join(parent, "brief.md")
	if err := os.WriteFile(instructions, []byte("Keep traffic on OpenAI.\n"), 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(brief, []byte("Inspect the artifact; do not deploy.\n"), 0o600); err != nil {
		test.Fatal(err)
	}
	first := createCLIProject(test, root, worktree, "Projects α", "--instructions", instructions)
	if first.Project.Instructions != "Keep traffic on OpenAI.\n" || len(first.Project.Threads) != 1 || first.Project.Threads[0].Role != project.RoleCoordinator {
		test.Fatalf("project context or coordinator differs: %#v", first.Project)
	}
	if first.Ledger != filepath.Join(root, first.Project.ID, "team") {
		test.Fatalf("ledger = %q", first.Ledger)
	}
	if _, err := os.Stat(filepath.Join(root, first.Project.ID)); !os.IsNotExist(err) {
		test.Fatalf("logical creation materialized an enrollment directory: %v", err)
	}
	output, stderr, code := runSurface(test, "", "project", "--root", root, "thread", "--project", first.Project.ID, "create", "Review", "--brief", brief, "--json")
	if code != 0 {
		test.Fatalf("thread create exit=%d output=%s stderr=%s", code, output, stderr)
	}
	var created projectThreadReport
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		test.Fatal(err)
	}
	if created.Version != project.Version || created.Revision != first.Revision+1 || created.ProjectID != first.Project.ID || created.Thread.Role != project.RoleWorker || created.Thread.Brief != "Inspect the artifact; do not deploy.\n" {
		test.Fatalf("thread response = %#v", created)
	}
	output, stderr, code = runSurface(test, "", "project", "--root", root, "thread", "--project", first.Project.ID, "show", created.Thread.ID, "--json")
	var reopened projectThreadReport
	if err := json.Unmarshal([]byte(output), &reopened); err != nil || code != 0 || reopened.Thread.ID != created.Thread.ID || reopened.Thread.Brief != created.Thread.Brief {
		test.Fatalf("fresh thread show exit=%d output=%s stderr=%s decode=%v", code, output, stderr, err)
	}
	second := createCLIProject(test, root, worktree, "Projects α")
	if second.Project.ID == first.Project.ID || second.Project.CoordinatorThreadID == first.Project.CoordinatorThreadID || second.Ledger == first.Ledger {
		test.Fatal("two projects sharing a label and directory shared logical identity")
	}
	output, stderr, code = runSurface(test, "", "project", "--root", root, "list", "--json")
	var registry project.Registry
	if err := json.Unmarshal([]byte(output), &registry); err != nil || code != 0 || len(registry.Projects) != 2 || registry.Revision != second.Revision {
		test.Fatalf("list exit=%d output=%s stderr=%s decode=%v", code, output, stderr, err)
	}
	if _, _, code := runSurface(test, "", "project", "--root", root, "thread", "--project", second.Project.ID, "show", created.Thread.ID, "--json"); code != exitUsage {
		test.Fatalf("another project's thread accepted, exit=%d", code)
	}
	output, stderr, code = runSurface(test, "", "project", "--root", root, "show", first.Project.ID, "--json")
	var shown projectReport
	if err := json.Unmarshal([]byte(output), &shown); err != nil || code != 0 || len(shown.Project.Threads) != 2 || shown.Project.Threads[1].ID != created.Thread.ID {
		test.Fatalf("project show exit=%d output=%s stderr=%s decode=%v", code, output, stderr, err)
	}
}

func TestSurfaceProjectReadAbsentWritesNothing(test *testing.T) {
	root := filepath.Join(test.TempDir(), "absent")
	output, stderr, code := runSurface(test, "", "project", "--root", root, "list", "--json")
	var registry project.Registry
	if err := json.Unmarshal([]byte(output), &registry); err != nil || code != 0 || registry.Version != project.Version || registry.Revision != 0 || len(registry.Projects) != 0 || !strings.Contains(output, `"projects": []`) {
		test.Fatalf("absent list exit=%d output=%s stderr=%s decode=%v", code, output, stderr, err)
	}
	if _, _, code := runSurface(test, "", "project", "--root", root, "show", "missing", "--json"); code != exitUsage {
		test.Fatalf("unknown project exit=%d", code)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		test.Fatalf("read created an absent root: %v", err)
	}
}

func TestSurfaceProjectRefusesInvalidCommandInputs(test *testing.T) {
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	cases := [][]string{
		{"project", "list", "--json"},
		{"project", "--root", "relative", "list", "--json"},
		{"project", "--root", "/", "list", "--json"},
		{"project", "--root", root, "create", "name", "--worktree", "relative", "--json"},
		{"project", "--root", root, "create", "bad\nlabel", "--worktree", worktree, "--json"},
		{"project", "--root", root, "thread", "list", "--json"},
		{"project", "--root", root, "thread", "create", "name", "--json"},
		{"project", "--root", root, "thread", "--project", "unknown", "create", "name", "--json"},
		{"project", "--root", root, "not-a-command"},
		{"project", "--root", root, "thread", "not-a-command"},
	}
	for _, arguments := range cases {
		if output, stderr, code := runSurface(test, "", arguments...); code != exitUsage {
			test.Errorf("%v exit=%d output=%s stderr=%s", arguments, code, output, stderr)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		test.Fatalf("invalid input created registry: %v", err)
	}
}

func TestSurfaceProjectRootValidationKeepsPositionalContract(test *testing.T) {
	for _, root := range []string{"", "relative", "/", filepath.Join(test.TempDir(), "registry")} {
		test.Run(root, func(test *testing.T) {
			hookCalled := false
			command := withProjectRoot(&root, &cobra.Command{
				Use:  "list",
				Args: cobra.NoArgs,
				RunE: func(*cobra.Command, []string) error {
					hookCalled = true
					return nil
				},
			})
			if err := command.Args(command, nil); err != nil || menuNeedsArgs(command) {
				test.Fatalf("flag validation changed the positional contract: %v", err)
			}
			err := command.RunE(command, nil)
			valid := filepath.IsAbs(root) && root != "/"
			if valid != (err == nil) || hookCalled != valid {
				test.Fatalf("valid=%t hook_called=%t error=%v", valid, hookCalled, err)
			}
		})
	}
}

func TestSurfaceProjectRefusesInvalidContextBeforeCreation(test *testing.T) {
	parent := test.TempDir()
	root, worktree := filepath.Join(parent, "registry"), test.TempDir()
	oversized, invalid, linked := filepath.Join(parent, "oversized"), filepath.Join(parent, "invalid"), filepath.Join(parent, "linked")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("a", project.MaxContextBytes+1)), 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte{0xff}, 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(invalid, linked); err != nil {
		test.Fatal(err)
	}
	for _, input := range []string{oversized, invalid, linked, worktree, filepath.Join(parent, "missing")} {
		if output, stderr, code := runSurface(test, "", "project", "--root", root, "create", "Test", "--worktree", worktree, "--instructions", input, "--json"); code != exitUsage {
			test.Errorf("input %s exit=%d output=%s stderr=%s", input, code, output, stderr)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		test.Fatalf("invalid context created registry: %v", err)
	}
}

func TestSurfaceProjectCorruptStoreIsNotAnEmptyList(test *testing.T) {
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	createCLIProject(test, root, worktree, "Test")
	registryPath := filepath.Join(root, "registry.json")
	if err := os.WriteFile(registryPath, []byte("{broken"), 0o600); err != nil {
		test.Fatal(err)
	}
	output, stderr, code := runSurface(test, "", "project", "--root", root, "list", "--json")
	if code != exitFailed || output != "" || stderr == "" {
		test.Fatalf("corrupt list exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	openedRoot, err := os.OpenRoot(root)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = openedRoot.Close() }()
	contents, err := openedRoot.ReadFile("registry.json")
	if err != nil || string(contents) != "{broken" {
		test.Fatalf("read repaired or replaced corrupt state: %q %v", contents, err)
	}
}

func TestSurfaceProjectProductionJSONErrors(test *testing.T) {
	isolateJSONContractEnv(test)
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	created := createCLIProject(test, root, worktree, "JSON contract")
	cases := []struct {
		name      string
		arguments []string
		code      string
		exit      int
	}{
		{"missing-root", []string{"list"}, jsonCodeUsage, exitUsage},
		{"relative-root", []string{"--root", "relative", "list"}, jsonCodeUsage, exitUsage},
		{"filesystem-root", []string{"--root", "/", "list"}, jsonCodeUsage, exitUsage},
		{"missing-project", []string{"--root", root, "thread", "list"}, jsonCodeUsage, exitUsage},
		{"unknown-project", []string{"--root", root, "show", "unknown"}, jsonCodeUsage, exitUsage},
		{"unknown-thread", []string{"--root", root, "thread", "--project", created.Project.ID, "show", "unknown"}, jsonCodeUsage, exitUsage},
		{"missing-context", []string{"--root", root, "create", "bad", "--worktree", worktree, "--instructions", filepath.Join(root, "missing")}, jsonCodeUsage, exitUsage},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			arguments := append([]string{"surface", "project"}, scenario.arguments...)
			arguments = append(arguments, "--json")
			output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), arguments...)
			assertProjectJSONError(test, output, stderr, err, scenario.code, scenario.exit)
		})
	}
	if err := os.WriteFile(filepath.Join(root, "registry.json"), []byte("{broken"), 0o600); err != nil {
		test.Fatal(err)
	}
	output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), "surface", "project", "--root", root, "list", "--json")
	assertProjectJSONError(test, output, stderr, err, jsonCodeFailed, exitFailed)
}

func assertProjectJSONError(test *testing.T, output, stderr string, err error, code string, exit int) {
	test.Helper()
	if err == nil || ExitCode(err) != exit || output != "" {
		test.Fatalf("exit=%d error=%v stdout=%q stderr=%q", ExitCode(err), err, output, stderr)
	}
	object := decodeOneStderrObject(test, stderr)
	if object["code"] != code {
		test.Fatalf("error code=%v, want %s", object["code"], code)
	}
	decoder := json.NewDecoder(strings.NewReader(stderr))
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		test.Fatal(err)
	}
	if err := decoder.Decode(&decoded); !errors.Is(err, io.EOF) {
		test.Fatalf("stderr has trailing data: %q (%v)", stderr, err)
	}
}

func TestSurfaceProjectBinaryRoundTrip(test *testing.T) {
	isolateJSONContractEnv(test)
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	invoke := func(arguments ...string) []byte {
		test.Helper()
		//nolint:gosec // G204: TestMain builds this executable from the repository; arguments name test-owned paths.
		command := osExec.CommandContext(test.Context(), builtBinPath, append([]string{"surface", "project", "--root", root}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			test.Fatalf("binary %v failed: %v: %s", arguments, err, output)
		}
		return output
	}
	var created, reopened projectReport
	if err := json.Unmarshal(invoke("create", "Across processes", "--worktree", worktree, "--json"), &created); err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(invoke("show", created.Project.ID, "--json"), &reopened); err != nil {
		test.Fatal(err)
	}
	if reopened.Project.ID != created.Project.ID || reopened.Project.CoordinatorThreadID != created.Project.CoordinatorThreadID || reopened.Revision != created.Revision {
		test.Fatal("fresh process lost project or coordinator identity")
	}
	//nolint:gosec // G204: TestMain builds this executable from the repository; arguments name test-owned paths.
	command := osExec.CommandContext(test.Context(), builtBinPath, "surface", "project", "--root", root, "show", "unknown", "--json")
	output, err := command.CombinedOutput()
	var exitError *osExec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != exitUsage {
		test.Fatalf("unknown ID exit=%v output=%s", err, output)
	}
	object := decodeOneStderrObject(test, string(output))
	if object["code"] != jsonCodeUsage {
		test.Fatalf("unknown ID error=%v", object)
	}
}
