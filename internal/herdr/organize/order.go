package organize

import "slices"

// OrderStep is one index move that brings a tab to its place: move the tab
// identified by TerminalID to Position (0-based) in its workspace.
type OrderStep struct {
	TerminalID string
	TabID      string // valid at plan time only
	Title      string
	Position   int
}

// OrderSteps lists the index moves that turn current into target, both
// ordered terminal ids of the same tabs. It walks the target from the front;
// where the tab in place is not the wanted one, it moves the wanted tab there.
// A workspace already in order yields no steps. It reports false when the two
// lists do not hold the same tabs, because their order cannot be compared.
func OrderSteps(current, target []string) ([]OrderStep, bool) {
	if len(current) != len(target) {
		return nil, false
	}
	cur := slices.Clone(current)
	var steps []OrderStep
	for i, want := range target {
		if cur[i] == want {
			continue
		}
		j := slices.Index(cur[i:], want)
		if j < 0 {
			return nil, false
		}
		j += i
		cur = slices.Delete(cur, j, j+1)
		cur = slices.Insert(cur, i, want)
		steps = append(steps, OrderStep{TerminalID: want, Position: i})
	}
	return steps, true
}

// Reorder is the tab-order work for one workspace.
type Reorder struct {
	Workspace string // label
	Steps     []OrderStep
}

// Reorders computes, from the snapshot as it is now, the tab-order steps each
// workspace needs to match plan.Layout. It is exact only when no move will
// change a workspace's members, so a workspace whose tabs differ from the
// layout's (a move is pending into or out of it) is left out; the order of
// such a workspace is only knowable after the moves run.
func Reorders(snap Snapshot, plan Plan) []Reorder {
	_, canonical, _ := indexWorkspaces(snap.Workspaces)
	terminalOf := make(map[string]string, len(plan.Assignments))
	tabOf := make(map[string]string, len(plan.Assignments))
	for _, a := range plan.Assignments {
		terminalOf[a.TabID] = a.TerminalID
		tabOf[a.TerminalID] = a.TabID
	}
	var out []Reorder
	for _, lw := range plan.Layout.Workspaces {
		id, ok := canonical[lw.Label]
		if !ok {
			continue
		}
		var current []string
		for _, tab := range snap.Tabs[id] {
			if term, ok := terminalOf[tab.TabID]; ok {
				current = append(current, term)
			}
		}
		target := make([]string, 0, len(lw.Tabs))
		titles := make(map[string]string, len(lw.Tabs))
		for _, t := range lw.Tabs {
			target = append(target, t.TerminalID)
			titles[t.TerminalID] = t.Title
		}
		steps, ok := OrderSteps(current, target)
		if !ok || len(steps) == 0 {
			continue
		}
		for i := range steps {
			steps[i].Title = titles[steps[i].TerminalID]
			steps[i].TabID = tabOf[steps[i].TerminalID]
		}
		out = append(out, Reorder{Workspace: lw.Label, Steps: steps})
	}
	return out
}

// WorkspaceOrderChange compares the workspaces' current left-to-right order
// with the order plan.Layout wants, over the labels that exist now. A
// workspace the plan creates is not counted: its position is only known once
// it exists. Repeated labels count once, at their first position.
func WorkspaceOrderChange(snap Snapshot, plan Plan) (from, to []string, changed bool) {
	seen := map[string]bool{}
	for _, w := range snap.Workspaces {
		if !seen[w.Label] {
			seen[w.Label] = true
			from = append(from, w.Label)
		}
	}
	for _, lw := range plan.Layout.Workspaces {
		if seen[lw.Label] {
			to = append(to, lw.Label)
		}
	}
	return from, to, !slices.Equal(from, to)
}
