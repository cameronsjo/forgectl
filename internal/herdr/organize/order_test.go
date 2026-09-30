package organize

import (
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/herdr"
)

func TestOrderSteps(t *testing.T) {
	tests := []struct {
		name    string
		current []string
		target  []string
		want    []OrderStep
		wantOK  bool
	}{
		{"already ordered", []string{"a", "b", "c"}, []string{"a", "b", "c"}, nil, true},
		{"one tab pulled to the front", []string{"a", "b", "c"}, []string{"c", "a", "b"}, []OrderStep{{TerminalID: "c", Position: 0}}, true},
		{"swap of the first two", []string{"b", "a", "c"}, []string{"a", "b", "c"}, []OrderStep{{TerminalID: "a", Position: 0}}, true},
		{"reversal", []string{"a", "b", "c"}, []string{"c", "b", "a"}, []OrderStep{{TerminalID: "c", Position: 0}, {TerminalID: "b", Position: 1}}, true},
		{"different members", []string{"a", "b"}, []string{"a", "x"}, nil, false},
		{"different lengths", []string{"a", "b"}, []string{"a"}, nil, false},
		{"empty", nil, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := OrderSteps(tt.current, tt.target)
			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("OrderSteps(%v, %v) = (%v, %v), want (%v, %v)", tt.current, tt.target, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestOrderSteps_ReplayReachesTarget(t *testing.T) {
	current := []string{"d", "a", "c", "b", "e"}
	target := []string{"a", "b", "c", "d", "e"}
	steps, ok := OrderSteps(current, target)
	if !ok {
		t.Fatal("ok = false for equal member sets")
	}
	cur := append([]string(nil), current...)
	for _, s := range steps {
		var rest []string
		for _, id := range cur {
			if id != s.TerminalID {
				rest = append(rest, id)
			}
		}
		cur = append(rest[:s.Position], append([]string{s.TerminalID}, rest[s.Position:]...)...)
	}
	if !reflect.DeepEqual(cur, target) {
		t.Errorf("replaying %v on %v gave %v, want %v", steps, current, cur, target)
	}
}

func TestReorders(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/b", "b"},
			{"w1", "t2", "term2", "/r/forge/a", "a"},
		})
	cfg := Config{Default: "forge", Rules: []Rule{{Glob: "*", Workspace: "forge"}}}
	plan := BuildPlan(cfg, snap, testRoot)
	got := Reorders(snap, plan)
	if len(got) != 1 || got[0].Workspace != "forge" || len(got[0].Steps) != 1 {
		t.Fatalf("Reorders = %+v, want one forge reorder with one step", got)
	}
	s := got[0].Steps[0]
	if s.TerminalID != "term2" || s.Position != 0 || s.Title != "a" || s.TabID != "t2" {
		t.Errorf("step = %+v, want term2 (a, t2) to position 0", s)
	}
}

func TestWorkspaceOrderChange(t *testing.T) {
	cfg := testConfig() // order: forge, home, misc
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1), ws("w2", "forge", 2)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/other/a", "a"},
			{"w2", "t2", "term2", "/r/forge/b", "b"},
		})
	from, to, changed := WorkspaceOrderChange(snap, BuildPlan(cfg, snap, testRoot))
	if !changed || !reflect.DeepEqual(from, []string{"misc", "forge"}) || !reflect.DeepEqual(to, []string{"forge", "misc"}) {
		t.Errorf("WorkspaceOrderChange = (%v, %v, %v), want misc,forge -> forge,misc", from, to, changed)
	}

	ordered := mkSnapshot(
		[]herdr.Workspace{ws("w2", "forge", 1), ws("w1", "misc", 2)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/other/a", "a"},
			{"w2", "t2", "term2", "/r/forge/b", "b"},
		})
	if _, _, changed := WorkspaceOrderChange(ordered, BuildPlan(cfg, ordered, testRoot)); changed {
		t.Error("changed = true for workspaces already in order")
	}
}

// TestWorkspaceOrderChange_ADuplicateLabelMovesBehindTheLayout: apply puts a
// non-canonical duplicate behind the layout's workspaces, so the dry run must
// count that as a change rather than dedupe the label away (#732).
func TestWorkspaceOrderChange_ADuplicateLabelMovesBehindTheLayout(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "forge", 2), ws("w3", "misc", 3)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/a", "a"},
			{"w2", "t2", "term2", "/r/forge/b", "b"},
			{"w3", "t3", "term3", "/r/other/c", "c"},
		})
	plan := BuildPlan(testConfig(), snap, testRoot)
	from, to, changed := WorkspaceOrderChange(snap, plan)
	if !changed || !reflect.DeepEqual(from, []string{"forge", "forge", "misc"}) || !reflect.DeepEqual(to, []string{"forge", "misc", "forge"}) {
		t.Errorf("WorkspaceOrderChange = (%v, %v, %v), want forge,forge,misc -> forge,misc,forge", from, to, changed)
	}
	current, target := WorkspaceOrderTarget(snap.Workspaces, plan.Layout)
	if !reflect.DeepEqual(current, []string{"w1", "w2", "w3"}) || !reflect.DeepEqual(target, []string{"w1", "w3", "w2"}) {
		t.Errorf("WorkspaceOrderTarget = (%v, %v), want w1,w2,w3 -> w1,w3,w2 (the canonical forge is the lowest Number)", current, target)
	}
}

func TestReorders_CountsAPanelessTabAsHoldingAPosition(t *testing.T) {
	// [b, X (no panes), a] wants [a, b]; apply sees X as a stand-in that keeps
	// its place at the end, so the dry run must plan the same single move.
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/b", "b"},
			{"w1", "t3", "term3", "/r/forge/a", "a"},
		})
	snap.Tabs["w1"] = []herdr.Tab{snap.Tabs["w1"][0], {TabID: "tX", WorkspaceID: "w1", Label: "ghost"}, snap.Tabs["w1"][1]}
	cfg := Config{Default: "forge", Rules: []Rule{{Glob: "*", Workspace: "forge"}}}
	got := Reorders(snap, BuildPlan(cfg, snap, testRoot))
	if len(got) != 1 || len(got[0].Steps) != 1 || got[0].Steps[0].TerminalID != "term3" || got[0].Steps[0].Position != 0 {
		t.Errorf("Reorders = %+v, want one step moving term3 to position 0", got)
	}
}

func TestReorders_SkipsAWorkspaceWhoseMembersWillChange(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/a", "a"},
			{"w1", "t2", "term2", "/r/other/b", "b"},
		})
	plan := BuildPlan(testConfig(), snap, testRoot)
	if got := Reorders(snap, plan); len(got) != 0 {
		t.Errorf("Reorders = %+v, want none: misc will lose a tab, so its order is not yet knowable", got)
	}
}
