// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package herdr

import "context"

// Window is the part of a terminal host that `forgectl herdr organize` reads
// and changes: workspaces (herdr-app calls them spaces), their tabs, and the
// panes in them. Two hosts implement it: the herdr 0.9 [Client] and the
// herdr-app [AppClient]. Pick one by [DetectHost].
//
// Move targets are the 0.9 client's [MoveTarget]; a host that cannot honor a
// target (herdr-app cannot move a tab to another space) returns
// [ErrUnsupportedMove].
type Window interface {
	Workspaces(ctx context.Context) ([]Workspace, error)
	Tabs(ctx context.Context, workspaceID string) ([]Tab, error)
	Panes(ctx context.Context) ([]Pane, error)
	MoveTab(ctx context.Context, tabID string, to MoveTarget) (MoveResult, error)
	// MoveWorkspace puts a workspace at a 0-based index.
	MoveWorkspace(ctx context.Context, workspaceID string, index int) error
	FocusWorkspace(ctx context.Context, workspaceID string) error
	FocusTab(ctx context.Context, tabID string) error
}

var (
	_ Window = (*Client)(nil)
	_ Window = (*AppClient)(nil)
)
