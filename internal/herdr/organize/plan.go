package organize

import (
	"fmt"
	"slices"
	"sort"

	"github.com/cameronsjo/forgectl/internal/herdr"
)

// entry is one classified tab while the plan is being built.
type entry struct {
	assign   Assignment
	fromWS   string // current workspace id
	toKey    string // destination key: a workspace id, or newKey(label)
	needMove bool
	key      sortKey
}

func newKey(label string) string { return "new:" + label }

// BuildPlan computes what organize would do to the session in snap under cfg.
// projectsRoot is the directory whose first two path parts (wing, repo) order
// tabs. It never fails: a tab or workspace it cannot place is reported in
// Warnings.
func BuildPlan(cfg Config, snap Snapshot, projectsRoot string) Plan {
	plan := Plan{RuleHits: make([]int, len(cfg.Rules))}

	labelOf, canonical, dupWarnings := indexWorkspaces(snap.Workspaces)
	plan.Warnings = append(plan.Warnings, dupWarnings...)

	entries, tabWarnings := classifyTabs(cfg, snap, projectsRoot, labelOf, canonical)
	plan.Warnings = append(plan.Warnings, tabWarnings...)

	moved, blockedIdx := orderMoves(entries, snap)
	blocked := make(map[int]bool, len(blockedIdx))
	for _, i := range blockedIdx {
		blocked[i] = true
	}

	for _, i := range moved {
		plan.Moves = append(plan.Moves, moveFor(entries[i], labelOf, false))
	}
	for _, i := range blockedIdx {
		plan.Moves = append(plan.Moves, moveFor(entries[i], labelOf, true))
	}

	for _, e := range entries {
		plan.Assignments = append(plan.Assignments, e.assign)
		if e.assign.Rule >= 0 {
			plan.RuleHits[e.assign.Rule]++
		} else {
			plan.Unmatched = append(plan.Unmatched, Unmatched{
				TerminalID: e.assign.TerminalID, TabID: e.assign.TabID, Title: e.assign.Title,
				CWD: e.assign.CWD, Key: e.assign.Key,
			})
		}
	}

	plan.Layout = buildLayout(cfg, snap.Workspaces, entries, blocked)
	return plan
}

// indexWorkspaces maps workspace id to label, and label to the id that wins
// when labels repeat (lowest Number).
func indexWorkspaces(wss []herdr.Workspace) (labelOf, canonical map[string]string, warnings []string) {
	labelOf = make(map[string]string, len(wss))
	canonical = make(map[string]string, len(wss))
	byID := make(map[string]herdr.Workspace, len(wss))
	for _, w := range wss {
		labelOf[w.WorkspaceID] = w.Label
		cur, seen := canonical[w.Label]
		if !seen {
			canonical[w.Label] = w.WorkspaceID
			byID[w.WorkspaceID] = w
			continue
		}
		winner, loser := cur, w
		if w.Number < byID[cur].Number {
			winner, loser = w.WorkspaceID, byID[cur]
			canonical[w.Label] = w.WorkspaceID
			byID[w.WorkspaceID] = w
		}
		warnings = append(warnings, fmt.Sprintf(
			"workspace label %q is used by more than one workspace; using %s (lowest number) and ignoring %s",
			w.Label, winner, loser.WorkspaceID))
	}
	return labelOf, canonical, warnings
}

// classifyTabs classifies every tab that has a pane with a terminal id, in
// workspace then tab order.
func classifyTabs(cfg Config, snap Snapshot, root string, labelOf, canonical map[string]string) ([]entry, []string) {
	panesByTab := make(map[string][]herdr.Pane)
	for _, p := range snap.Panes {
		panesByTab[p.TabID] = append(panesByTab[p.TabID], p)
	}
	var entries []entry
	var warnings []string
	for _, w := range snap.Workspaces {
		for _, tab := range snap.Tabs[w.WorkspaceID] {
			panes := panesByTab[tab.TabID]
			if len(panes) == 0 {
				warnings = append(warnings, fmt.Sprintf("tab %q [%s] has no panes and was skipped", tab.Label, tab.TabID))
				continue
			}
			// The first pane's terminal id is the tab's identity across moves.
			// Without one, two such tabs would share an identity, so the tab is
			// left where it is, as a tab with no panes is.
			if panes[0].TerminalID == "" {
				warnings = append(warnings, fmt.Sprintf("tab %q [%s] has no terminal id and was skipped", tab.Label, tab.TabID))
				continue
			}
			rule, target, sp := Classify(cfg, panes)
			title := tab.Label
			if title == "" {
				title = sp.TerminalTitleStripped
			}
			e := entry{
				assign: Assignment{
					TerminalID: panes[0].TerminalID, TabID: tab.TabID, Title: title, CWD: sp.CWD,
					Key: matchKey(sp), Rule: rule, From: labelOf[w.WorkspaceID], To: target,
				},
				fromWS: w.WorkspaceID,
				key:    sortKeyFor(root, sp.CWD, panes[0].TerminalID),
			}
			if id, ok := canonical[target]; ok {
				e.toKey = id
			} else {
				e.toKey = newKey(target)
			}
			e.needMove = e.toKey != e.fromWS
			entries = append(entries, e)
		}
	}
	return entries, warnings
}

// orderMoves decides the order in which moves can run and which cannot run at
// all. herdr refuses to empty a workspace, so a move whose source would be
// left with no tabs waits for an arrival that keeps the source populated. It
// returns the indices of entries that can move, in order, and the indices that
// never can.
func orderMoves(entries []entry, snap Snapshot) (moved, blocked []int) {
	counts := make(map[string]int)
	for id, tabs := range snap.Tabs {
		counts[id] = len(tabs)
	}
	var pending []int
	for i, e := range entries {
		if e.needMove {
			pending = append(pending, i)
		}
	}
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var still []int
		for _, i := range pending {
			e := entries[i]
			if counts[e.fromWS] > 1 {
				counts[e.fromWS]--
				counts[e.toKey]++
				moved = append(moved, i)
				progress = true
				continue
			}
			still = append(still, i)
		}
		pending = still
	}
	return moved, pending
}

func moveFor(e entry, labelOf map[string]string, blocked bool) Move {
	m := Move{
		TerminalID: e.assign.TerminalID, TabID: e.assign.TabID, Title: e.assign.Title, CWD: e.assign.CWD,
		From: e.assign.From, To: e.assign.To,
	}
	if blocked {
		m.Blocked = true
		m.BlockedReason = fmt.Sprintf("it is the only tab in %s, and herdr will not empty a workspace", labelOf[e.fromWS])
	}
	return m
}

// buildLayout groups tabs by their final workspace label and orders both the
// workspaces and the tabs in each. A blocked tab stays in the workspace it is
// in.
func buildLayout(cfg Config, wss []herdr.Workspace, entries []entry, blocked map[int]bool) Layout {
	groups := make(map[string][]int)
	var createdOrder []string
	existing := make(map[string]bool, len(wss))
	for _, w := range wss {
		existing[w.Label] = true
	}
	for i, e := range entries {
		label := e.assign.To
		if blocked[i] {
			label = e.assign.From
		}
		groups[label] = append(groups[label], i)
		if !existing[label] && !slices.Contains(createdOrder, label) {
			createdOrder = append(createdOrder, label)
		}
	}

	present := make(map[string]bool)
	for _, w := range wss {
		present[w.Label] = true
	}
	for _, l := range createdOrder {
		present[l] = true
	}

	var order []string
	seen := make(map[string]bool)
	add := func(l string) {
		if !seen[l] {
			seen[l] = true
			order = append(order, l)
		}
	}
	for _, l := range cfg.WorkspaceOrder {
		if present[l] {
			add(l)
		}
	}
	for _, w := range wss {
		add(w.Label)
	}
	for _, l := range createdOrder {
		add(l)
	}

	var layout Layout
	for _, label := range order {
		idx := groups[label]
		sort.SliceStable(idx, func(a, b int) bool { return entries[idx[a]].key.less(entries[idx[b]].key) })
		lw := LayoutWorkspace{Label: label}
		for _, i := range idx {
			lw.Tabs = append(lw.Tabs, LayoutTab{TerminalID: entries[i].assign.TerminalID, Title: entries[i].assign.Title})
		}
		layout.Workspaces = append(layout.Workspaces, lw)
	}
	return layout
}

// MissingFromOrder lists the workspace labels the rules and the default send
// tabs to that WorkspaceOrder does not name, in first-use order. Such a
// workspace keeps whatever position it has, which is rarely what the operator
// meant.
func MissingFromOrder(cfg Config) []string {
	var missing []string
	consider := func(l string) {
		if l != "" && !slices.Contains(cfg.WorkspaceOrder, l) && !slices.Contains(missing, l) {
			missing = append(missing, l)
		}
	}
	for _, r := range cfg.Rules {
		consider(r.Workspace)
	}
	consider(cfg.Default)
	return missing
}
