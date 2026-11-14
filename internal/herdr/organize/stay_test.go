// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package organize

import (
	"github.com/cameronsjo/forgectl/internal/herdr"
	"reflect"
	"testing"
)

// With StayInSpace every tab that wants another workspace is a blocked,
// unsupported move, including one into a workspace that does not exist, and the
// layout keeps each tab where it is so ordering still works.
func TestBuildPlan_StayInSpaceBlocksEveryCrossMove(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "misc", 2)},
		[]tabSpec{
			{"w1", "t2", "term2", "/r/forge/b", "b"},
			{"w1", "t1", "term1", "/r/forge/a", "a"},
			{"w2", "t3", "term3", "/r/forge/c", "c"}, // wants forge
			{"w2", "t4", "term4", "/r/home/d", "d"},  // wants home, which does not exist
			{"w2", "t5", "term5", "/r/other", "e"},   // already right
		})
	cfg := testConfig()
	cfg.StayInSpace = true
	plan := BuildPlan(cfg, snap, testRoot)

	if len(plan.Moves) != 2 {
		t.Fatalf("moves = %+v, want the two tabs that want another workspace", plan.Moves)
	}
	for _, m := range plan.Moves {
		if !m.Blocked || !m.Unsupported || m.BlockedReason == "" {
			t.Errorf("move %+v must be blocked and unsupported", m)
		}
	}
	if got := layoutLabels(plan.Layout); !reflect.DeepEqual(got, []string{"forge", "misc"}) {
		t.Errorf("layout workspaces = %v, want no created workspace", got)
	}
	if got := layoutTerms(plan.Layout, "forge"); !reflect.DeepEqual(got, []string{"term1", "term2"}) {
		t.Errorf("forge layout = %v, want tabs sorted in place", got)
	}
	if got := layoutTerms(plan.Layout, "misc"); len(got) != 3 {
		t.Errorf("misc layout = %v, want its three tabs left in place", got)
	}
	// Ordering still plans: forge is out of order.
	re := Reorders(snap, plan)
	if len(re) != 1 || re[0].Workspace != "forge" || len(re[0].Steps) != 1 || re[0].Steps[0].TerminalID != "term1" {
		t.Errorf("reorders = %+v, want one step moving term1 first in forge", re)
	}
}

func TestBuildPlan_WithoutStayInSpaceStillMoves(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "misc", 2)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/a", "a"},
			{"w2", "t3", "term3", "/r/forge/c", "c"},
			{"w2", "t5", "term5", "/r/other", "e"},
		})
	plan := BuildPlan(testConfig(), snap, testRoot)
	if len(plan.Moves) != 1 || plan.Moves[0].Blocked || plan.Moves[0].Unsupported {
		t.Errorf("moves = %+v, want one ordinary pending move", plan.Moves)
	}
}
