package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// cliMultiWorkflow is a three-step run-only workflow with one param feeding the
// first step, so a param change alters step 0's input hash without touching the
// file bytes. All steps are `run`, so none composes on an export — resume needs
// no export reconstruction.
const cliMultiWorkflow = `dsl_version = 1
name = "multi"
version = "1.0.0"

[params.flavor]
default = "vanilla"

[[step]]
uses = "run"
cmd = "step-one"
args = ["${flavor}"]

[[step]]
uses = "run"
cmd = "step-two"

[[step]]
uses = "run"
cmd = "step-three"
`

// failOn builds a FakeRunner that errors when a given command name is invoked
// and succeeds otherwise — used to fail a workflow at a chosen step.
func failOn(bad string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == bad {
			return "", fmt.Errorf("simulated failure in %s", name)
		}
		return "", nil
	}}
}

// execRun runs `workflow run <args>` with runner and a pass-through verifier
// already installed by the caller, returning combined output and the error.
func execRun(t *testing.T, runner *exec.FakeRunner, args ...string) (string, error) {
	t.Helper()
	cmd := newWorkflowRunCmd(module.Deps{Runner: runner})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func callNames(r *exec.FakeRunner) []string {
	names := make([]string, len(r.Calls))
	for i, c := range r.Calls {
		names[i] = c.Name
	}
	return names
}

// TestWorkflowRun_FreshRunWritesCheckpoints: a run that fails at step two leaves
// step one checkpointed with the definition hash recorded.
func TestWorkflowRun_FreshRunWritesCheckpoints(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("run should fail at step-two")
	}

	st, ok, err := workflow.LoadState("multi")
	if err != nil || !ok {
		t.Fatalf("expected checkpoint state after a partial run: ok=%v err=%v", ok, err)
	}
	if len(st.Steps) != 1 || st.Steps[0].Index != 0 {
		t.Fatalf("only step 0 should be checkpointed, got %+v", st.Steps)
	}
	if st.DefinitionHash != workflow.DefinitionHash([]byte(cliMultiWorkflow)) {
		t.Errorf("definition hash not recorded / mismatched: %q", st.DefinitionHash)
	}
}

// TestWorkflowRun_ResumeSkipsCompletedSteps: after a partial run, --resume runs
// only the remaining steps and clears state on success.
func TestWorkflowRun_ResumeSkipsCompletedSteps(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}

	resumeRunner := &exec.FakeRunner{}
	out, err := execRun(t, resumeRunner, "multi", "--resume")
	if err != nil {
		t.Fatalf("resume should succeed: %v", err)
	}

	names := callNames(resumeRunner)
	for _, n := range names {
		if n == "step-one" {
			t.Errorf("resume must NOT re-run the completed step-one; ran %v", names)
		}
	}
	if len(names) != 2 || names[0] != "step-two" || names[1] != "step-three" {
		t.Fatalf("resume should run step-two then step-three, got %v", names)
	}
	if !strings.Contains(out, "resuming") {
		t.Errorf("resume should announce itself: %q", out)
	}
	if _, ok, _ := workflow.LoadState("multi"); ok {
		t.Error("state must be cleared after a successful resume")
	}
}

// TestWorkflowRun_InputHashChangeForcesRerun: resuming with a different param
// changes step 0's input hash, so the previously-completed step re-runs.
func TestWorkflowRun_InputHashChangeForcesRerun(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	// Seed a partial run with the default flavor.
	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}

	// Resume with a different flavor — step 0's inputs changed, so it must re-run.
	resumeRunner := &exec.FakeRunner{}
	if _, err := execRun(t, resumeRunner, "multi", "--resume", "--param", "flavor=chocolate"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	names := callNames(resumeRunner)
	if len(names) == 0 || names[0] != "step-one" {
		t.Fatalf("a changed input hash must re-run step-one, got %v", names)
	}
}

// TestWorkflowRun_DefinitionChangeRefusesResume: editing the workflow file after
// a checkpoint makes --resume refuse (a resume never replays across an edit).
func TestWorkflowRun_DefinitionChangeRefusesResume(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	path := cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}

	// Edit the definition (any byte change flips the hash).
	edited := strings.Replace(cliMultiWorkflow, `version = "1.0.0"`, `version = "1.0.1"`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("rewrite workflow: %v", err)
	}

	_, err := execRun(t, &exec.FakeRunner{}, "multi", "--resume")
	if err == nil {
		t.Fatal("resume across an edited definition must be refused")
	}
	if !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "fresh") {
		t.Errorf("refusal should tell the user to run fresh: %v", err)
	}
}

// TestWorkflowRun_ResumeWithoutStateRefused: --resume with no saved state is a
// clear usage error, not a silent fresh run.
func TestWorkflowRun_ResumeWithoutStateRefused(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	_, err := execRun(t, &exec.FakeRunner{}, "multi", "--resume")
	if err == nil || !strings.Contains(err.Error(), "no saved run state") {
		t.Fatalf("resume without state should error clearly, got %v", err)
	}
}

// TestWorkflowRun_MissingExportRefusesResume: a workflow whose resumed step needs
// a checkpointed step's ${export} is refused — resume never reconstructs an
// ephemeral step output (and never feeds a blessing-guarded field from the
// unsigned sidecar).
func TestWorkflowRun_MissingExportRefusesResume(t *testing.T) {
	const sandboxed = `dsl_version = 1
name = "sandboxed"
version = "1.0.0"

[[step]]
uses = "worktree"
repo = "/tmp/nonexistent-repo"

[[step]]
uses = "run"
cmd = "echo"
args = ["${workspace}"]

[[step]]
uses = "teardown"
`
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "sandboxed", []byte(sandboxed))
	swapVerifier(t, fakeVerifier{})

	// Seed a checkpoint marking step 0 (worktree) complete with the hash the CLI
	// would compute, so ResumeFrom lands on step 1 — without running any git.
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	src, err := workflow.Load("sandboxed")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wf, err := workflow.Parse(src.Data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := workflow.NewRegistry(stepContributions(deps)...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	plan, err := workflow.BuildPlan(wf, nil, reg)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if err := workflow.WriteState(workflow.RunState{
		Schema:         workflow.StateSchema,
		Workflow:       "sandboxed",
		DefinitionHash: workflow.DefinitionHash(src.Data),
		Steps: []workflow.StepState{
			{Index: 0, Uses: "worktree", InputHash: workflow.HashPlanStep(plan.Steps[0])},
		},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	runner := &exec.FakeRunner{}
	_, err = execRun(t, runner, "sandboxed", "--resume")
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("resume needing a checkpointed export must be refused, got %v", err)
	}
	if len(runner.Calls) != 0 {
		t.Errorf("no step may run when resume is refused, got calls %+v", runner.Calls)
	}
}

// TestWorkflowStatus_RendersCheckpoints: status prints the workflow, the run id,
// and each completed step.
func TestWorkflowStatus_RendersCheckpoints(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}

	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"multi"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	body := out.String()
	for _, want := range []string{"multi", "step(s) complete", "1. run"} {
		if !strings.Contains(body, want) {
			t.Errorf("status output missing %q:\n%s", want, body)
		}
	}
}

// TestWorkflowStatus_NoState: status on a never-run workflow reports the absence
// without erroring.
func TestWorkflowStatus_NoState(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))

	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"multi"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status on a never-run workflow should not error: %v", err)
	}
	if !strings.Contains(out.String(), "no saved run state") {
		t.Errorf("expected a no-state message, got %q", out.String())
	}
}

// TestWorkflowStatus_DefinitionChangedNote: status flags a definition that
// drifted since the checkpointed run.
func TestWorkflowStatus_DefinitionChangedNote(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	path := cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	swapVerifier(t, fakeVerifier{})

	if _, err := execRun(t, failOn("step-two"), "multi"); err == nil {
		t.Fatal("seed run should fail at step-two")
	}
	edited := strings.Replace(cliMultiWorkflow, `version = "1.0.0"`, `version = "2.0.0"`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("rewrite workflow: %v", err)
	}

	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"multi"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), "has changed") {
		t.Errorf("status should note the drifted definition:\n%s", out.String())
	}
}

// TestWorkflowRun_DryRunAndResumeMutuallyExclusive locks the flag guard.
func TestWorkflowRun_DryRunAndResumeMutuallyExclusive(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))

	_, err := execRun(t, &exec.FakeRunner{}, "multi", "--dry-run", "--resume")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("--dry-run --resume should be rejected, got %v", err)
	}
}
