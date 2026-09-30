package herdr

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/cameronsjo/forgectl/internal/redact"
)

// Declined is returned when herdr answers a `tab move` with exit 0 and
// move_result.changed=false. herdr refuses some moves quietly: the last tab of
// a workspace cannot leave it (reason last_tab_in_workspace, measured). A
// caller that ignores this keeps re-issuing a move that never happens.
type Declined struct {
	TabID  string
	Reason string
}

// Error renders Reason through redact.Text before printable, as
// (*Error).Error renders Message (#832): Reason is herdr's text, a reason
// code today, and nothing stops a future herdr from echoing a value there.
// Reason is capped as Message is (#837), and stored redacted as Message is
// (#941), so the exported field never holds herdr's raw text.
func (d *Declined) Error() string {
	return "herdr declined to move tab " + printableMax(d.TabID) + ": " + printableMax(redact.Text(d.Reason))
}

// MoveTarget says where a tab goes. Build one with [ToWorkspace],
// [ToNewWorkspace], or [ToIndex]; the zero value is invalid.
type MoveTarget struct {
	mode        moveMode
	workspaceID string
	label       string
	index       int
}

type moveMode int

const (
	modeNone moveMode = iota
	modeWorkspace
	modeNewWorkspace
	modeIndex
)

// ToWorkspace moves the tab into an existing workspace.
func ToWorkspace(workspaceID string) MoveTarget {
	return MoveTarget{mode: modeWorkspace, workspaceID: workspaceID}
}

// ToNewWorkspace moves the tab into a new workspace. An empty label leaves
// herdr's default.
func ToNewWorkspace(label string) MoveTarget {
	return MoveTarget{mode: modeNewWorkspace, label: label}
}

// ToIndex reorders the tab within its current workspace.
func ToIndex(index int) MoveTarget {
	return MoveTarget{mode: modeIndex, index: index}
}

func (t MoveTarget) args(tabID string) ([]string, error) {
	base := []string{"tab", "move", tabID}
	switch t.mode {
	case modeWorkspace:
		if err := checkID("workspace id", t.workspaceID); err != nil {
			return nil, err
		}
		return append(base, "--workspace", t.workspaceID), nil
	case modeNewWorkspace:
		if t.label == "" {
			return append(base, "--new-workspace"), nil
		}
		if err := checkID("workspace label", t.label); err != nil {
			return nil, err
		}
		return append(base, "--new-workspace", "--label", t.label), nil
	case modeIndex:
		if t.index < 0 {
			return nil, fmt.Errorf("herdr: negative tab index %d", t.index)
		}
		return append(base, "--index", strconv.Itoa(t.index)), nil
	}
	return nil, errors.New("herdr: empty move target; use ToWorkspace, ToNewWorkspace, or ToIndex")
}

// MoveResult describes a completed move. TabID is the tab's id AFTER the move:
// a move between workspaces renumbers the tab (measured w7D:t17 -> w7D:t19), so
// never keep using the id you passed in. Tabs is the destination workspace's
// tab list in its new order, as herdr returns it.
//
// An index move within a workspace carries no move_result in herdr's reply
// (measured); TabID is then the id passed in and WorkspaceID comes from the
// returned tab list.
type MoveResult struct {
	TabID       string
	WorkspaceID string
	Tabs        []Tab
}

// MoveTab moves a tab. A move herdr declines returns a zero MoveResult and a
// *[Declined]. After a move between workspaces, callers re-list, since ids of
// other tabs may shift. An index move is the measured exception: it keeps every
// tab id, and its reply carries the workspace's tab list in the new order, so a
// caller may use Tabs instead of listing again when that list holds the moved
// tab.
func (c *Client) MoveTab(ctx context.Context, tabID string, to MoveTarget) (MoveResult, error) {
	if err := checkID("tab id", tabID); err != nil {
		return MoveResult{}, err
	}
	args, err := to.args(tabID)
	if err != nil {
		return MoveResult{}, err
	}
	r, err := read[struct {
		MoveResult *struct {
			Changed     *bool  `json:"changed"`
			Reason      string `json:"reason"`
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"move_result"`
		Tabs []Tab `json:"tabs"`
	}](ctx, c, args...)
	if err != nil {
		return MoveResult{}, err
	}
	if mr := r.MoveResult; mr != nil {
		switch {
		case mr.Changed == nil:
			return MoveResult{}, fmt.Errorf("herdr %s: move_result has no \"changed\" field", argvText(args))
		case !*mr.Changed:
			return MoveResult{}, &Declined{TabID: tabID, Reason: redact.Text(mr.Reason)}
		case mr.TabID == "" || mr.WorkspaceID == "":
			return MoveResult{}, fmt.Errorf("herdr %s: move_result names no tab or workspace", argvText(args))
		}
		return MoveResult{TabID: mr.TabID, WorkspaceID: mr.WorkspaceID, Tabs: r.Tabs}, nil
	}
	// No move_result. Only an index move is measured to reply that way; for a
	// move between workspaces the passed-in id would be stale, so fail closed.
	if to.mode != modeIndex {
		return MoveResult{}, fmt.Errorf("herdr %s: reply has no move_result", argvText(args))
	}
	for _, t := range r.Tabs {
		if t.TabID == tabID {
			return MoveResult{TabID: tabID, WorkspaceID: t.WorkspaceID, Tabs: r.Tabs}, nil
		}
	}
	return MoveResult{}, fmt.Errorf("herdr %s: tab is not in the reply's tab list", argvText(args))
}

// MoveWorkspace reorders a workspace to index. The reply (the workspace list)
// is not decoded; only failure is reported.
func (c *Client) MoveWorkspace(ctx context.Context, workspaceID string, index int) error {
	if index < 0 {
		return fmt.Errorf("herdr: negative workspace index %d", index)
	}
	return c.act(ctx, "workspace id", workspaceID, "workspace", "move", workspaceID, "--index", strconv.Itoa(index))
}

// FocusWorkspace switches the UI to a workspace. The reply is not decoded.
func (c *Client) FocusWorkspace(ctx context.Context, workspaceID string) error {
	return c.act(ctx, "workspace id", workspaceID, "workspace", "focus", workspaceID)
}

// FocusTab switches the UI to a tab. It is the finest focus grain herdr
// offers by id: `pane focus` is directional only, so there is no FocusPane.
// The reply is not decoded.
func (c *Client) FocusTab(ctx context.Context, tabID string) error {
	return c.act(ctx, "tab id", tabID, "tab", "focus", tabID)
}
