// Package organize plans how to group herdr tabs into workspaces and order
// them. It is pure: it reads a [Snapshot] and returns a [Plan], and never
// calls herdr or touches the filesystem, so every decision is table-testable.
//
// The plan is a target [Layout] (terminal ids, no tab or workspace ids)
// because herdr renumbers a tab when it changes workspace. The command that
// applies the plan re-resolves each terminal id to a live tab id right before
// acting on it.
package organize

import "github.com/cameronsjo/forgectl/internal/herdr"

// Snapshot is one read of the herdr session. Panes are in `pane list` order,
// which decides a tab's identity and which pane classifies it.
type Snapshot struct {
	Workspaces []herdr.Workspace
	Tabs       map[string][]herdr.Tab // keyed by workspace id
	Panes      []herdr.Pane
}

// Rule sends tabs whose match key matches Glob to the workspace labeled
// Workspace.
type Rule struct {
	Glob      string
	Workspace string
}

// Config is the rule set. Rules are tried in order.
type Config struct {
	Default        string
	WorkspaceOrder []string
	Rules          []Rule
}

// Move relocates one tab to another workspace. From and To are workspace
// labels, never ids: apply resolves To to a live workspace right before the
// move. TabID is valid only at plan time.
type Move struct {
	TerminalID    string
	TabID         string
	Title         string
	CWD           string
	From          string
	To            string
	Blocked       bool
	BlockedReason string
}

// LayoutTab is one tab in the desired final arrangement.
type LayoutTab struct {
	TerminalID string
	Title      string
}

// LayoutWorkspace is one workspace, in its final position, with its tabs in
// their final order.
type LayoutWorkspace struct {
	Label string
	Tabs  []LayoutTab
}

// Layout is the desired final arrangement, free of ids that a move renumbers.
type Layout struct {
	Workspaces []LayoutWorkspace
}

// Assignment records how one tab was classified, for --explain.
type Assignment struct {
	TerminalID string
	TabID      string
	Title      string
	CWD        string
	Key        string // match key of the classifying pane
	Rule       int    // index into Config.Rules, or -1 for the default
	From       string // current workspace label
	To         string // target workspace label
}

// Unmatched is a tab that matched no rule and goes to the default workspace.
type Unmatched struct {
	TerminalID string
	TabID      string
	Title      string
	CWD        string
	Key        string
}

// Plan is what organize would do.
type Plan struct {
	Moves       []Move
	Layout      Layout
	Assignments []Assignment
	Unmatched   []Unmatched
	RuleHits    []int // per rule, tabs it classified
	Warnings    []string
}
