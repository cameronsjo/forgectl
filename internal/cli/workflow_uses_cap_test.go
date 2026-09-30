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

// TestPrintPlan_ArgsKeepElementBoundaries is #816: the review joined a run
// step's args with spaces, so ["a b"] and ["a","b"] printed the same line and
// a hostile file could show the reviewer an argv split it does not run. The
// args also print in full, uncapped (#782).
//
// Mutation: restore strings.Join(s.Args, " ") and the two plans render
// identically; wrap each element in QuoteArgMax and the long arg's tail is cut.
// The globs line gets the same treatment: restore its raw ", " join and
// ["a, b"] and ["a","b"] render identically.
func TestPrintPlan_ArgsKeepElementBoundaries(t *testing.T) {
	render := func(args ...string) string {
		var out bytes.Buffer
		printPlan(&out, workflow.Plan{Name: "n", Version: "1", Steps: []workflow.PlanStep{{Cmd: "tool", Args: args}}})
		return out.String()
	}
	one, two := render("a b"), render("a", "b")
	if one == two {
		t.Fatalf("[\"a b\"] and [\"a\",\"b\"] render identically: %q", one)
	}
	if !strings.Contains(one, `args: "a b"`) || !strings.Contains(two, `args: "a" "b"`) {
		t.Errorf("args not quoted per element:\n%s\n%s", one, two)
	}
	globs := func(g ...string) string {
		var out bytes.Buffer
		printPlan(&out, workflow.Plan{Name: "n", Version: "1", Steps: []workflow.PlanStep{{Uses: "strip", Globs: g}}})
		return out.String()
	}
	if g1, g2 := globs("a, b"), globs("a", "b"); g1 == g2 || !strings.Contains(g2, `globs: "a", "b"`) {
		t.Errorf("globs not quoted per element:\n%s\n%s", g1, g2)
	}
	long := strings.Repeat("x", 300) + "TAIL"
	if got := render(long); !strings.Contains(got, long) {
		t.Errorf("dry-run review cut an arg; it must print args in full: %q", got)
	}
}
