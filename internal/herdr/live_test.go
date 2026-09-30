package herdr

import (
	"context"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestLiveSession runs the probe and the read calls against the herdr session
// this test runs inside. It is read-only and skipped unless HERDR_LIVE=1:
//
//	HERDR_LIVE=1 go test ./internal/herdr/ -run TestLiveSession -v
func TestLiveSession(t *testing.T) {
	if os.Getenv("HERDR_LIVE") != "1" {
		t.Skip("set HERDR_LIVE=1 inside a herdr pane to run against the live session")
	}
	ctx := context.Background()
	r := exec.OSRunner{}
	if err := Probe(ctx, r, os.LookupEnv); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	c := New(r)
	workspaces, err := c.Workspaces(ctx)
	if err != nil || len(workspaces) == 0 {
		t.Fatalf("Workspaces = %d rows, %v", len(workspaces), err)
	}
	tabs, err := c.Tabs(ctx, workspaces[0].WorkspaceID)
	if err != nil || len(tabs) == 0 {
		t.Fatalf("Tabs = %d rows, %v", len(tabs), err)
	}
	panes, err := c.Panes(ctx)
	if err != nil || len(panes) == 0 {
		t.Fatalf("Panes = %d rows, %v", len(panes), err)
	}
	if panes[0].TerminalID == "" || panes[0].TabID == "" || panes[0].WorkspaceID == "" {
		t.Errorf("pane missing ids: %+v", panes[0])
	}
	if _, err := c.Agents(ctx); err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if got, err := c.PaneGet(ctx, panes[0].PaneID); err != nil || got.TerminalID != panes[0].TerminalID {
		t.Fatalf("PaneGet = %+v, %v", got, err)
	}
	if got, err := c.TabGet(ctx, tabs[0].TabID); err != nil || got.TabID != tabs[0].TabID {
		t.Fatalf("TabGet = %+v, %v", got, err)
	}
	if _, err := c.ReadPane(ctx, panes[0].PaneID, ReadVisible, 3); err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	_, err = c.Tabs(ctx, "wNOPE")
	if he, ok := err.(*Error); !ok || he.Code != "workspace_not_found" {
		t.Fatalf("Tabs(wNOPE) err = %v, want *Error workspace_not_found", err)
	}
}
