//go:build unix

package cli

import (
	"encoding/json"
	"os"
	osExec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func registerProjectCLIMember(test *testing.T, ledger string, member mail.Worker, replace bool) {
	test.Helper()
	arguments := []string{"team", "register", member.Name, "--ledger", ledger, "--harness", string(member.Harness), "--worktree", member.Worktree, "--run-id", member.RunID, "--json"}
	if member.Coordinator {
		arguments = append(arguments, "--coordinator")
	}
	if member.Socket != "" {
		arguments = append(arguments, "--socket", member.Socket)
	}
	if member.ThreadID != "" {
		arguments = append(arguments, "--thread", member.ThreadID)
	}
	if replace {
		arguments = append(arguments, "--replace")
	}
	output, stderr, code := runSurface(test, "", arguments...)
	if code != 0 {
		test.Fatalf("register exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
}

func bindProjectCLIThread(test *testing.T, root, projectID, threadID, member, runID, expected string) projectThreadReport {
	test.Helper()
	output, stderr, code := runSurface(test, "", "project", "--root", root, "thread", "--project", projectID, "bind", threadID, "--member", member, "--run-id", runID, "--expect-run", expected, "--json")
	if code != 0 {
		test.Fatalf("bind exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var report projectThreadReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		test.Fatal(err)
	}
	if report.ProjectID != projectID || report.Thread.ID != threadID || report.Thread.ActiveRunID != runID || report.Observation != nil {
		test.Fatalf("binding identity or unrequested liveness: %#v", report)
	}
	return report
}

func readProjectCLIThread(test *testing.T, root, projectID, threadID string) projectThreadReport {
	test.Helper()
	output, stderr, code := runSurface(test, "", "project", "--root", root, "thread", "--project", projectID, "show", threadID, "--json")
	if code != 0 {
		test.Fatalf("show exit=%d stdout=%s stderr=%s", code, output, stderr)
	}
	var report projectThreadReport
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		test.Fatal(err)
	}
	if report.Observation == nil || report.Observation.ThreadID != threadID || report.Observation.ProcessLiveness != project.ProcessLivenessUnknown || report.Observation.ObservedAt.IsZero() {
		test.Fatalf("missing or overclaimed observation: %#v", report)
	}
	return report
}

func TestSurfaceProjectBindingReplacementKeepsLogicalIdentity(test *testing.T) {
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	created := createCLIProject(test, root, worktree, "Durable coordinator")
	threadID := created.Project.CoordinatorThreadID
	initial := readProjectCLIThread(test, root, created.Project.ID, threadID)
	if initial.Observation.Enrollment != project.EnrollmentUnbound || len(initial.Thread.Runs) != 0 {
		test.Fatalf("initial binding=%#v", initial)
	}
	member := mail.Worker{Name: "coordinator", Harness: mail.HarnessCodex, Coordinator: true, Worktree: worktree, RunID: "codex-run-one"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bound := bindProjectCLIThread(test, root, created.Project.ID, threadID, member.Name, member.RunID, "none")
	observed := readProjectCLIThread(test, root, created.Project.ID, threadID)
	if len(bound.Thread.Runs) != 1 || observed.Observation.Enrollment != project.EnrollmentCurrent || observed.Observation.NativeSessionKnown {
		test.Fatalf("initial enrolled binding=%#v observation=%#v", bound, observed)
	}
	member.ThreadID = "codex-native-one"
	registerProjectCLIMember(test, created.Ledger, member, true)
	stale := readProjectCLIThread(test, root, created.Project.ID, threadID)
	if stale.Observation.Enrollment != project.EnrollmentStale || stale.Observation.NativeSessionKnown || stale.Thread.Runs[0].NativeSessionID != "" {
		test.Fatalf("inspection silently enriched binding: %#v", stale)
	}
	if output, stderr, code := runSurface(test, "", "project", "--root", root, "thread", "--project", created.Project.ID, "context", threadID, "--json"); code != exitFailed || output != "" || !strings.Contains(stderr, "stale") {
		test.Fatalf("stale context exit=%d output=%s stderr=%s", code, output, stderr)
	}
	enriched := bindProjectCLIThread(test, root, created.Project.ID, threadID, member.Name, member.RunID, member.RunID)
	if len(enriched.Thread.Runs) != 1 || enriched.Thread.Runs[0].NativeSessionID != member.ThreadID || !enriched.Thread.Runs[0].BoundAt.Equal(bound.Thread.Runs[0].BoundAt) {
		test.Fatalf("explicit discovery replaced the run: %#v", enriched)
	}
	previousRun := member.RunID
	member.Harness, member.RunID, member.ThreadID = mail.HarnessPi, "pi-run-two", "pi-native-two"
	member.Socket = filepath.Join(test.TempDir(), "pi.sock")
	registerProjectCLIMember(test, created.Ledger, member, true)
	if observed := readProjectCLIThread(test, root, created.Project.ID, threadID); observed.Observation.Enrollment != project.EnrollmentStale {
		test.Fatalf("replacement enrollment did not stale old binding: %#v", observed)
	}
	if output, stderr, code := runSurface(test, "", "project", "--root", root, "thread", "--project", created.Project.ID, "bind", threadID, "--member", member.Name, "--run-id", member.RunID, "--expect-run", "none", "--json"); code != exitFailed || output != "" {
		test.Fatalf("stale CAS exit=%d output=%s stderr=%s", code, output, stderr)
	}
	replaced := bindProjectCLIThread(test, root, created.Project.ID, threadID, member.Name, member.RunID, previousRun)
	if len(replaced.Thread.Runs) != 2 || replaced.Thread.Runs[0] != enriched.Thread.Runs[0] || replaced.Thread.Runs[1].Harness != mail.HarnessPi || replaced.Thread.Role != project.RoleCoordinator {
		test.Fatalf("replacement lost role or history: %#v", replaced)
	}
	observed = readProjectCLIThread(test, root, created.Project.ID, threadID)
	if observed.Observation.Enrollment != project.EnrollmentCurrent || !observed.Observation.NativeSessionKnown {
		test.Fatalf("replacement observation=%#v", observed)
	}
	second := createCLIProject(test, root, worktree, "Same directory, different project")
	if output, stderr, code := runSurface(test, "", "project", "--root", root, "thread", "--project", second.Project.ID, "bind", second.Project.CoordinatorThreadID, "--member", member.Name, "--run-id", member.RunID, "--expect-run", "none", "--json"); code == 0 || output != "" {
		test.Fatalf("borrowed another project's roster: exit=%d output=%s stderr=%s", code, output, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, second.Project.ID)); !os.IsNotExist(err) {
		test.Fatalf("failed cross-project binding created ledger: %v", err)
	}
}

func TestSurfaceProjectContextCapsuleIsReadOnlyAndValuePreserving(test *testing.T) {
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	instructions := "Project instructions\nquoted \"value\"\ncontrol \x1b[31m and \u202e\n"
	brief := "Thread brief\nKeep native histories separate.\n"
	store := project.Store{Root: root}
	_, created, err := store.CreateProject(test.Context(), "Context capsule", worktree, instructions)
	if err != nil {
		test.Fatal(err)
	}
	_, thread, err := store.CreateThread(test.Context(), created.ID, "Worker", brief)
	if err != nil {
		test.Fatal(err)
	}
	openedRoot, err := os.OpenRoot(root)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = openedRoot.Close() }()
	before, err := openedRoot.ReadFile("registry.json")
	if err != nil {
		test.Fatal(err)
	}
	arguments := []string{"project", "--root", root, "thread", "--project", created.ID, "context", thread.ID}
	output, stderr, code := runSurface(test, "", append(arguments, "--json")...)
	if code != 0 || strings.ContainsAny(output, "\x1b\u202e") {
		test.Fatalf("capsule exit=%d output=%q stderr=%s", code, output, stderr)
	}
	var capsule projectContextReport
	if err := json.Unmarshal([]byte(output), &capsule); err != nil {
		test.Fatal(err)
	}
	if capsule.ProjectID != created.ID || capsule.ThreadID != thread.ID || capsule.Role != project.RoleWorker || capsule.ProjectInstructions != instructions || capsule.ThreadBrief != brief || capsule.ActiveRun != nil || capsule.Observation.Enrollment != project.EnrollmentUnbound || capsule.Ledger != projectLedger(root, created.ID) || capsule.Roster != filepath.Join(capsule.Ledger, "roster.json") || !strings.Contains(capsule.Authority, "not operator approval") {
		test.Fatalf("capsule lost context or separation: %#v", capsule)
	}
	output, stderr, code = runSurface(test, "", arguments...)
	if code != 0 || strings.ContainsAny(output, "\x1b\u202e") || !strings.Contains(output, "--json") {
		test.Fatalf("human capsule exit=%d output=%q stderr=%s", code, output, stderr)
	}
	after, err := openedRoot.ReadFile("registry.json")
	if err != nil || string(after) != string(before) {
		test.Fatalf("context read changed registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, created.ID)); !os.IsNotExist(err) {
		test.Fatalf("context read materialized ledger: %v", err)
	}
	member := mail.Worker{Name: "worker", Harness: mail.HarnessCodex, RunID: "worker-run", ThreadID: "worker-native", Worktree: test.TempDir()}
	registerProjectCLIMember(test, capsule.Ledger, member, false)
	bindProjectCLIThread(test, root, created.ID, thread.ID, member.Name, member.RunID, "none")
	output, stderr, code = runSurface(test, "", append(arguments, "--json")...)
	if err := json.Unmarshal([]byte(output), &capsule); err != nil || code != 0 || capsule.ActiveRun == nil || capsule.ActiveRun.Worktree != member.Worktree || capsule.ActiveRun.RunID != member.RunID || capsule.Observation.ProcessLiveness != project.ProcessLivenessUnknown {
		test.Fatalf("bound worker capsule exit=%d output=%s stderr=%s decode=%v", code, output, stderr, err)
	}
}

func TestSurfaceProjectBindingJSONRefusals(test *testing.T) {
	isolateJSONContractEnv(test)
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	created := createCLIProject(test, root, worktree, "Refusals")
	base := []string{"surface", "project", "--root", root, "thread", "--project", created.Project.ID}
	cases := [][]string{
		{"bind", created.Project.CoordinatorThreadID, "--member", "coordinator", "--run-id", "run-one"},
		{"bind", created.Project.CoordinatorThreadID, "--run-id", "run-one", "--expect-run", "none"},
		{"bind", created.Project.CoordinatorThreadID, "--member", "coordinator", "--expect-run", "none"},
		{"bind", created.Project.CoordinatorThreadID, "--member", "coordinator", "--run-id", "run-one", "--expect-run", ""},
		{"bind", created.Project.CoordinatorThreadID, "--member", "coordinator", "--run-id", "none", "--expect-run", "none"},
		{"context", "unknown-thread"},
	}
	for _, arguments := range cases {
		test.Run(strings.Join(arguments, " "), func(test *testing.T) {
			full := append(append([]string{}, base...), arguments...)
			full = append(full, "--json")
			output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), full...)
			assertProjectJSONError(test, output, stderr, err, jsonCodeUsage, exitUsage)
		})
	}
	member := mail.Worker{Name: "coordinator", Harness: mail.HarnessCodex, Coordinator: true, Worktree: worktree, RunID: "run-one"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bindProjectCLIThread(test, root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "none")
	arguments := append(append([]string{}, base...), "bind", created.Project.CoordinatorThreadID, "--member", member.Name, "--run-id", member.RunID, "--expect-run", "none", "--json")
	output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), arguments...)
	assertProjectJSONError(test, output, stderr, err, jsonCodeFailed, exitFailed)
	if err := os.WriteFile(filepath.Join(created.Ledger, "roster.json"), []byte(`{"v":1,"workers":[],"workers":[]}`), 0o600); err != nil {
		test.Fatal(err)
	}
	arguments = append(append([]string{}, base...), "show", created.Project.CoordinatorThreadID, "--json")
	output, stderr, err = runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), arguments...)
	assertProjectJSONError(test, output, stderr, err, jsonCodeFailed, exitFailed)
}

func TestSurfaceProjectBindingBinaryRoundTrip(test *testing.T) {
	isolateJSONContractEnv(test)
	root, worktree := filepath.Join(test.TempDir(), "registry"), test.TempDir()
	invoke := func(arguments ...string) []byte {
		test.Helper()
		//nolint:gosec // G204: TestMain builds this executable from the repository; arguments name test-owned state.
		command := osExec.CommandContext(test.Context(), builtBinPath, append([]string{"surface"}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			test.Fatalf("binary %v failed: %v: %s", arguments, err, output)
		}
		return output
	}
	var created projectReport
	if err := json.Unmarshal(invoke("project", "--root", root, "create", "Persistent binding", "--worktree", worktree, "--json"), &created); err != nil {
		test.Fatal(err)
	}
	var enrollment struct {
		Member mail.Worker `json:"member"`
	}
	if err := json.Unmarshal(invoke("team", "register", "coordinator", "--ledger", created.Ledger, "--coordinator", "--harness", "codex", "--worktree", worktree, "--run-id", "binary-run-one", "--thread", "binary-native-one", "--json"), &enrollment); err != nil {
		test.Fatal(err)
	}
	if enrollment.Member.RunID != "binary-run-one" {
		test.Fatalf("enrollment=%#v", enrollment)
	}
	var bound projectThreadReport
	if err := json.Unmarshal(invoke("project", "--root", root, "thread", "--project", created.Project.ID, "bind", created.Project.CoordinatorThreadID, "--member", "coordinator", "--run-id", enrollment.Member.RunID, "--expect-run", "none", "--json"), &bound); err != nil {
		test.Fatal(err)
	}
	var capsule projectContextReport
	if err := json.Unmarshal(invoke("project", "--root", root, "thread", "--project", created.Project.ID, "context", created.Project.CoordinatorThreadID, "--json"), &capsule); err != nil {
		test.Fatal(err)
	}
	if capsule.ProjectID != created.Project.ID || capsule.ThreadID != bound.Thread.ID || capsule.ActiveRun == nil || capsule.ActiveRun.RunID != enrollment.Member.RunID || capsule.ActiveRun.NativeSessionID != enrollment.Member.ThreadID || capsule.Observation.Enrollment != project.EnrollmentCurrent || capsule.Observation.ProcessLiveness != project.ProcessLivenessUnknown {
		test.Fatalf("fresh process lost binding identity or overstated liveness: %#v", capsule)
	}
}
