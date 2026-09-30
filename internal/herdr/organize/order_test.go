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
