package cli

// A step's uses is capped wherever the human workflow views print it (#778
// item 6): --dry-run renders before the registry check, and status reads it
// back from a state file, so in both it is unvetted text of any length.
//
//   [x] printPlan caps uses
//   [x] workflow status caps a checkpointed step's uses

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/workflow"
)

var longUses = strings.Repeat("u", 300)

func TestPrintPlan_CapsUses(t *testing.T) {
	var out bytes.Buffer
	printPlan(&out, workflow.Plan{Name: "n", Version: "1", Steps: []workflow.PlanStep{{Uses: longUses}}})
	if strings.Contains(out.String(), longUses[:81]) || !strings.Contains(out.String(), "[truncated]") {
		t.Errorf("printPlan echoes uses uncapped: %q", out.String())
	}
}

func TestWorkflowStatus_CapsUses(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "multi", []byte(cliMultiWorkflow))
	if err := workflow.WriteState(workflow.RunState{
		Schema:   workflow.StateSchema,
		Workflow: "multi",
		Steps:    []workflow.StepState{{Index: 0, Uses: longUses}},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	cmd := newWorkflowStatusCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"multi"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out.String(), longUses[:81]) || !strings.Contains(out.String(), "[truncated]") {
		t.Errorf("status echoes uses uncapped: %q", out.String())
	}
}
