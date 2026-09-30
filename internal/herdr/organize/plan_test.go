package organize

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/herdr"
)

const testRoot = "/r"

// tabSpec describes one tab and its panes for a test snapshot.
type tabSpec struct {
	ws    string // workspace id
	tab   string // tab id
	term  string // terminal id of the first pane
	cwd   string
	title string
}

func mkSnapshot(wss []herdr.Workspace, tabs []tabSpec) Snapshot {
	s := Snapshot{Workspaces: wss, Tabs: map[string][]herdr.Tab{}}
	for _, t := range tabs {
		s.Tabs[t.ws] = append(s.Tabs[t.ws], herdr.Tab{TabID: t.tab, WorkspaceID: t.ws, Label: t.title})
		s.Panes = append(s.Panes, herdr.Pane{
			PaneID: t.tab + "-p", TabID: t.tab, WorkspaceID: t.ws, TerminalID: t.term,
			CWD: t.cwd, TerminalTitleStripped: t.title,
		})
	}
	return s
}

func ws(id, label string, number int) herdr.Workspace {
	return herdr.Workspace{WorkspaceID: id, Label: label, Number: number}
}

func testConfig() Config {
	return Config{
		Default:        "misc",
		WorkspaceOrder: []string{"forge", "home", "misc"},
		Rules: []Rule{
			{Glob: "/r/forge/* :: *", Workspace: "forge"},
			{Glob: "/r/home/* :: *", Workspace: "home"},
		},
	}
}

func layoutTerms(l Layout, label string) []string {
	for _, w := range l.Workspaces {
		if w.Label == label {
			var out []string
			for _, t := range w.Tabs {
				out = append(out, t.TerminalID)
			}
			return out
		}
	}
	return nil
}

func layoutLabels(l Layout) []string {
	var out []string
	for _, w := range l.Workspaces {
		out = append(out, w.Label)
	}
	return out
}

func TestBuildPlan_AlreadyOrganizedIsEmpty(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "home", 2), ws("w3", "misc", 3)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/a", "a"},
			{"w1", "t2", "term2", "/r/forge/b", "b"},
			{"w2", "t3", "term3", "/r/home/c", "c"},
			{"w3", "t4", "term4", "/r/other/d", "d"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Moves) != 0 || len(p.Warnings) != 0 {
		t.Fatalf("Moves=%v Warnings=%v, want none", p.Moves, p.Warnings)
	}
	if got, want := layoutTerms(p.Layout, "forge"), []string{"term1", "term2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("forge layout = %v, want %v", got, want)
	}
	if got, want := layoutLabels(p.Layout), []string{"forge", "home", "misc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace order = %v, want %v", got, want)
	}
}

func TestBuildPlan_TwoTabsIntoOneMissingWorkspace(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w3", "misc", 1)},
		[]tabSpec{
			{"w3", "t1", "term1", "/r/forge/b", "b"},
			{"w3", "t2", "term2", "/r/forge/a", "a"},
			{"w3", "t3", "term3", "/r/other/x", "x"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", p.Moves)
	}
	for _, m := range p.Moves {
		if m.To != "forge" || m.From != "misc" || m.Blocked {
			t.Errorf("move %+v: want misc -> forge, not blocked", m)
		}
	}
	if got, want := layoutTerms(p.Layout, "forge"), []string{"term2", "term1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("forge layout = %v, want %v (sorted by repo/cwd, a before b)", got, want)
	}
	if got, want := layoutLabels(p.Layout), []string{"forge", "misc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace order = %v, want %v (created label placed by workspace_order; home absent so skipped)", got, want)
	}
	if got, want := p.RuleHits, []int{2, 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("RuleHits = %v, want %v", got, want)
	}
}

func TestBuildPlan_DuplicateLabelsPickLowestNumber(t *testing.T) {
	// Two workspaces are labeled forge; w2 has the lower Number, so it is the
	// canonical one. Tabs in the duplicate w9 are planned as moves into it, and
	// the tab already in w2 stays put.
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w9", "forge", 5), ws("w2", "forge", 2), ws("w3", "misc", 3)},
		[]tabSpec{
			{"w9", "t5", "term5", "/r/forge/x", "x"},
			{"w9", "t6", "term6", "/r/forge/y", "y"},
			{"w2", "t4", "term4", "/r/forge/w", "w"},
			{"w3", "t1", "term1", "/r/forge/a", "a"},
			{"w3", "t2", "term2", "/r/other/b", "b"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	var moved []string
	for _, m := range p.Moves {
		if m.Blocked {
			continue
		}
		moved = append(moved, m.TerminalID)
		if m.To != "forge" {
			t.Errorf("move %+v, want every move to go to forge", m)
		}
	}
	// term5 would be blocked with a lone tab; w9 holds two, so one leaves and
	// the last one is blocked (herdr will not empty w9).
	if want := []string{"term5", "term1"}; !reflect.DeepEqual(moved, want) {
		t.Errorf("unblocked moves = %v, want %v: w9's tabs join the canonical w2, w2's own tab stays", moved, want)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "forge") || !strings.Contains(p.Warnings[0], "w9") {
		t.Errorf("Warnings = %v, want one naming the duplicate label and the ignored id", p.Warnings)
	}
}

// TestBuildPlan_EqualCWDTabsKeepTheirOrderAcrossRenumbering: two tabs with the
// same cwd tie on wing, repo, and cwd. The tie breaks on the terminal id, which
// no move changes, so the order the layout asks for does not flip when a move
// renumbers the tabs (#732).
func TestBuildPlan_EqualCWDTabsKeepTheirOrderAcrossRenumbering(t *testing.T) {
	cfg := Config{Default: "forge", Rules: []Rule{{Glob: "*", Workspace: "forge"}}}
	before := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1)},
		[]tabSpec{
			{"w1", "t1", "termB", "/r/forge/a", "b"},
			{"w1", "t2", "termA", "/r/forge/a", "a"},
		})
	// The same two tabs after a move gave them fresh ids in the other order.
	after := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1)},
		[]tabSpec{
			{"w1", "t9", "termB", "/r/forge/a", "b"},
			{"w1", "t3", "termA", "/r/forge/a", "a"},
		})
	want := []string{"termA", "termB"}
	for name, snap := range map[string]Snapshot{"before": before, "after": after} {
		if got := layoutTerms(BuildPlan(cfg, snap, testRoot).Layout, "forge"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s renumbering: forge layout = %v, want %v", name, got, want)
		}
	}
}

func TestBuildPlan_WorkspaceOrderLabelAbsentIsSkipped(t *testing.T) {
	cfg := testConfig()
	cfg.WorkspaceOrder = []string{"ghost", "misc", "forge"}
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "misc", 2)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/a", "a"},
			{"w2", "t2", "term2", "/r/other/b", "b"},
		})
	p := BuildPlan(cfg, snap, testRoot)
	if got, want := layoutLabels(p.Layout), []string{"misc", "forge"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace order = %v, want %v", got, want)
	}
}

func TestBuildPlan_UnlistedWorkspacesKeepOrderAfterListed(t *testing.T) {
	cfg := testConfig()
	cfg.WorkspaceOrder = []string{"misc"}
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "zeta", 1), ws("w2", "alpha", 2), ws("w3", "misc", 3)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/other/a", "a"},
			{"w2", "t2", "term2", "/r/other/b", "b"},
			{"w3", "t3", "term3", "/r/other/c", "c"},
		})
	p := BuildPlan(cfg, snap, testRoot)
	if got, want := layoutLabels(p.Layout), []string{"misc", "zeta", "alpha"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace order = %v, want %v", got, want)
	}
}

func TestBuildPlan_SoleTabMoveIsBlocked(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "misc", 2)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/other/a", "a"}, // only tab in forge, belongs in misc
			{"w2", "t2", "term2", "/r/other/b", "b"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Moves) != 1 || !p.Moves[0].Blocked {
		t.Fatalf("Moves = %+v, want one Blocked move", p.Moves)
	}
	m := p.Moves[0]
	if !strings.Contains(m.BlockedReason, "only tab in forge") || !strings.Contains(m.BlockedReason, "will not empty a workspace") {
		t.Errorf("BlockedReason = %q, want it to name the workspace and the rule", m.BlockedReason)
	}
	if got := layoutTerms(p.Layout, "forge"); !reflect.DeepEqual(got, []string{"term1"}) {
		t.Errorf("a blocked tab stays where it is in Layout; forge = %v", got)
	}
	if got := layoutTerms(p.Layout, "misc"); !reflect.DeepEqual(got, []string{"term2"}) {
		t.Errorf("misc = %v, want only term2", got)
	}
}

func TestBuildPlan_ArrivalUnblocksLeavingTab(t *testing.T) {
	// forge holds one tab (t1) that belongs in misc; misc holds t2 (stays) and
	// t3, which belongs in forge. t1 may leave only after t3 arrives, so the
	// planner must order t3 first and block nothing.
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "forge", 1), ws("w2", "misc", 2)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/other/a", "a"},
			{"w2", "t2", "term2", "/r/other/x", "x"},
			{"w2", "t3", "term3", "/r/forge/b", "b"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", p.Moves)
	}
	for _, m := range p.Moves {
		if m.Blocked {
			t.Errorf("move %+v blocked, want none: t3 arrives in forge before t1 leaves", m)
		}
	}
	if p.Moves[0].TerminalID != "term3" || p.Moves[1].TerminalID != "term1" {
		t.Errorf("move order = %s, %s; want term3 (arrival) before term1 (departure)", p.Moves[0].TerminalID, p.Moves[1].TerminalID)
	}
}

func TestBuildPlan_TabWithNoPanesIsSkippedWithWarning(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1)},
		[]tabSpec{{"w1", "t1", "term1", "/r/other/a", "a"}})
	snap.Tabs["w1"] = append(snap.Tabs["w1"], herdr.Tab{TabID: "t2", WorkspaceID: "w1", Label: "ghost"})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "ghost") {
		t.Errorf("Warnings = %v, want one naming the pane-less tab", p.Warnings)
	}
	if got := layoutTerms(p.Layout, "misc"); !reflect.DeepEqual(got, []string{"term1"}) {
		t.Errorf("misc layout = %v, want only term1", got)
	}
}

func TestBuildPlan_TabIdentityIsFirstPaneInListOrder(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1)},
		[]tabSpec{{"w1", "t1", "first", "/r/other/a", "a"}})
	snap.Panes = append(snap.Panes, herdr.Pane{PaneID: "p2", TabID: "t1", WorkspaceID: "w1", TerminalID: "second", CWD: "/r/other/z"})
	p := BuildPlan(testConfig(), snap, testRoot)
	if got := layoutTerms(p.Layout, "misc"); !reflect.DeepEqual(got, []string{"first"}) {
		t.Errorf("layout = %v, want the tab identified by its first pane", got)
	}
}

func TestBuildPlan_UnmatchedAndAssignments(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/other/a", "a"},
			{"w1", "t2", "term2", "/r/forge/b", "b"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Unmatched) != 1 || p.Unmatched[0].TerminalID != "term1" || p.Unmatched[0].Key != "/r/other/a :: a" {
		t.Errorf("Unmatched = %+v, want term1 with its match key", p.Unmatched)
	}
	if len(p.Assignments) != 2 {
		t.Fatalf("Assignments = %+v, want 2", p.Assignments)
	}
}

// renumber returns a copy of the snapshot with every workspace, tab, and pane
// id rewritten, as herdr does after moves. Terminal ids are the stable key.
func renumber(s Snapshot) Snapshot {
	out := Snapshot{Tabs: map[string][]herdr.Tab{}}
	for _, w := range s.Workspaces {
		w.WorkspaceID = "N" + w.WorkspaceID
		out.Workspaces = append(out.Workspaces, w)
	}
	for id, tabs := range s.Tabs {
		for _, tb := range tabs {
			tb.TabID = "N" + tb.TabID
			tb.WorkspaceID = "N" + tb.WorkspaceID
			out.Tabs["N"+id] = append(out.Tabs["N"+id], tb)
		}
	}
	for _, p := range s.Panes {
		p.PaneID = "N" + p.PaneID
		p.TabID = "N" + p.TabID
		p.WorkspaceID = "N" + p.WorkspaceID
		out.Panes = append(out.Panes, p)
	}
	return out
}

func TestBuildPlan_LayoutIsIndependentOfIDs(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w3", "misc", 1)},
		[]tabSpec{
			{"w3", "t1", "term1", "/r/forge/b", "b"},
			{"w3", "t2", "term2", "/r/forge/a", "a"},
			{"w3", "t3", "term3", "/r/other/x", "x"},
		})
	before := BuildPlan(testConfig(), snap, testRoot)
	// Pre-move snapshot with renumbered ids; layout must not change.
	after := BuildPlan(testConfig(), renumber(snap), testRoot)
	if !reflect.DeepEqual(before.Layout, after.Layout) {
		t.Errorf("layout differs after renumbering:\n before %+v\n after  %+v", before.Layout, after.Layout)
	}
}

func TestBuildPlan_MoveCarriesIdentityFields(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1)},
		[]tabSpec{
			{"w1", "t1", "term1", "/r/forge/a", "alpha"},
			{"w1", "t2", "term2", "/r/other/b", "b"},
		})
	p := BuildPlan(testConfig(), snap, testRoot)
	if len(p.Moves) != 1 {
		t.Fatalf("Moves = %+v", p.Moves)
	}
	m := p.Moves[0]
	if m.TerminalID != "term1" || m.TabID != "t1" || m.Title != "alpha" || m.CWD != "/r/forge/a" ||
		m.From != "misc" || m.To != "forge" {
		t.Errorf("move = %+v", m)
	}
}

func TestMissingFromOrder(t *testing.T) {
	cfg := Config{
		Default:        "misc",
		WorkspaceOrder: []string{"forge"},
		Rules:          []Rule{{Glob: "*", Workspace: "forge"}, {Glob: "x", Workspace: "home"}, {Glob: "y", Workspace: "home"}},
	}
	got := MissingFromOrder(cfg)
	want := []string{"home", "misc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MissingFromOrder = %v, want %v", got, want)
	}
}

// TestBuildPlan_ATabWithNoTerminalIDIsSkipped: a terminal id is a tab's
// identity across moves, so two tabs without one would tie and merge. Each is
// skipped with a warning, as a tab with no panes is (#945).
func TestBuildPlan_ATabWithNoTerminalIDIsSkipped(t *testing.T) {
	snap := mkSnapshot(
		[]herdr.Workspace{ws("w1", "misc", 1)},
		[]tabSpec{
			{"w1", "t1", "", "/r/forge/a", "a"},
			{"w1", "t2", "", "/r/forge/a", "a"},
			{"w1", "t3", "term3", "/r/other/c", "c"},
		})
	plan := BuildPlan(testConfig(), snap, testRoot)
	for _, a := range plan.Assignments {
		if a.TerminalID == "" {
			t.Errorf("assignment %+v has no terminal id", a)
		}
	}
	if len(plan.Moves) != 0 {
		t.Errorf("Moves = %+v, want none", plan.Moves)
	}
	n := 0
	for _, w := range plan.Warnings {
		if strings.Contains(w, "has no terminal id and was skipped") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("Warnings = %q, want one skip warning per tab without a terminal id", plan.Warnings)
	}
}
