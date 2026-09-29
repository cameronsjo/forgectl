package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// moveResultOf reads the move_result a fixture claims, so tests compare against
// the fixture rather than a second hard-coded copy of it.
func moveResultOf(t *testing.T, name string) (tabID, workspaceID string) {
	t.Helper()
	var env struct {
		Result struct {
			MoveResult struct {
				TabID       string `json:"tab_id"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"move_result"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(fixture(t, name)), &env); err != nil {
		t.Fatal(err)
	}
	return env.Result.MoveResult.TabID, env.Result.MoveResult.WorkspaceID
}

func TestMoveTabDeclinedReturnsDeclined(t *testing.T) {
	c := New(runnerFor(fixture(t, "move_decline.json"), nil))
	got, err := c.MoveTab(context.Background(), "w9:t1", ToWorkspace("w1"))
	var d *Declined
	if !errors.As(err, &d) {
		t.Fatalf("err = %v, want *Declined", err)
	}
	if d.Reason != "last_tab_in_workspace" || d.TabID != "w9:t1" {
		t.Errorf("declined = %+v", d)
	}
	if !reflect.DeepEqual(got, MoveResult{}) {
		t.Errorf("result on decline = %+v, want zero", got)
	}
}

func TestMoveTabToWorkspaceReturnsPostMoveIDs(t *testing.T) {
	r := runnerFor(fixture(t, "move_workspace.json"), nil)
	const passedIn = "w9:t17"
	got, err := New(r).MoveTab(context.Background(), passedIn, ToWorkspace("w1"))
	if err != nil {
		t.Fatalf("MoveTab: %v", err)
	}
	wantTab, wantWS := moveResultOf(t, "move_workspace.json")
	if got.TabID != wantTab || got.WorkspaceID != wantWS {
		t.Errorf("result = %+v, want tab %s workspace %s", got, wantTab, wantWS)
	}
	if got.TabID == passedIn {
		t.Errorf("result kept the pre-move id %s; herdr renumbers on move", passedIn)
	}
	if len(got.Tabs) == 0 {
		t.Error("destination tab list not returned")
	}
	if want := []string{"tab", "move", passedIn, "--workspace", "w1"}; !reflect.DeepEqual(r.Last().Args, want) {
		t.Errorf("argv = %v, want %v", r.Last().Args, want)
	}
}

func TestMoveTabToNewWorkspaceReturnsTheNewWorkspaceID(t *testing.T) {
	r := runnerFor(fixture(t, "move_new_workspace.json"), nil)
	got, err := New(r).MoveTab(context.Background(), "w9:t3", ToNewWorkspace("tooling"))
	if err != nil {
		t.Fatalf("MoveTab: %v", err)
	}
	_, wantWS := moveResultOf(t, "move_new_workspace.json")
	if got.WorkspaceID != wantWS || got.WorkspaceID == "" {
		t.Errorf("workspace = %q, want %q", got.WorkspaceID, wantWS)
	}
	if want := []string{"tab", "move", "w9:t3", "--new-workspace", "--label", "tooling"}; !reflect.DeepEqual(r.Last().Args, want) {
		t.Errorf("argv = %v, want %v", r.Last().Args, want)
	}
	if _, err := New(r).MoveTab(context.Background(), "w9:t3", ToNewWorkspace("")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"tab", "move", "w9:t3", "--new-workspace"}; !reflect.DeepEqual(r.Last().Args, want) {
		t.Errorf("argv without label = %v, want %v", r.Last().Args, want)
	}
}

func TestMoveTabIndexHasNoMoveResult(t *testing.T) {
	r := runnerFor(fixture(t, "move_index.json"), nil)
	var tabs struct {
		Result struct {
			Tabs []Tab `json:"tabs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "move_index.json")), &tabs); err != nil {
		t.Fatal(err)
	}
	target := tabs.Result.Tabs[0]
	got, err := New(r).MoveTab(context.Background(), target.TabID, ToIndex(0))
	if err != nil {
		t.Fatalf("MoveTab: %v", err)
	}
	if got.TabID != target.TabID || got.WorkspaceID != target.WorkspaceID {
		t.Errorf("result = %+v, want the passed-in tab in its workspace %s", got, target.WorkspaceID)
	}
	if len(got.Tabs) != len(tabs.Result.Tabs) {
		t.Errorf("tabs = %d, want %d", len(got.Tabs), len(tabs.Result.Tabs))
	}
	if want := []string{"tab", "move", target.TabID, "--index", "0"}; !reflect.DeepEqual(r.Last().Args, want) {
		t.Errorf("argv = %v, want %v", r.Last().Args, want)
	}
}

func TestMoveTabFailsClosedOnUnexpectedReplies(t *testing.T) {
	const tabs = `[{"tab_id":"w1:t1","workspace_id":"w1"}]`
	// The passed-in id IS in this list: the index-move fallback would accept it,
	// so a workspace move must be refused on its own, not because the lookup missed.
	const tabsWithPassedInID = `[{"tab_id":"w1:t7","workspace_id":"w1"}]`
	for name, tt := range map[string]struct {
		target MoveTarget
		reply  string
	}{
		"workspace move with no move_result":     {ToWorkspace("w1"), `{"id":"x","result":{"tabs":` + tabsWithPassedInID + `}}`},
		"new workspace move with no move_result": {ToNewWorkspace("t"), `{"id":"x","result":{"tabs":` + tabsWithPassedInID + `}}`},
		"move_result without changed":            {ToWorkspace("w1"), `{"id":"x","result":{"move_result":{"tab_id":"w1:t9","workspace_id":"w1"}}}`},
		"changed true but no tab id":             {ToWorkspace("w1"), `{"id":"x","result":{"move_result":{"changed":true,"workspace_id":"w1"}}}`},
		"changed true but no workspace id":       {ToWorkspace("w1"), `{"id":"x","result":{"move_result":{"changed":true,"tab_id":"w1:t9"}}}`},
		"index move, tab missing from the list":  {ToIndex(0), `{"id":"x","result":{"tabs":` + tabs + `}}`},
		"index move, empty reply":                {ToIndex(0), `{"id":"x","result":{}}`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := New(runnerFor(tt.reply, nil)).MoveTab(context.Background(), "w1:t7", tt.target)
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			var d *Declined
			if errors.As(err, &d) {
				t.Errorf("reported as a decline: %v", err)
			}
		})
	}
}

func TestMoveTabRefusesBadInput(t *testing.T) {
	c := New(runnerFor("", nil))
	ctx := context.Background()
	for name, fn := range map[string]func() error{
		"zero target":        func() error { _, err := c.MoveTab(ctx, "w1:t1", MoveTarget{}); return err },
		"empty tab id":       func() error { _, err := c.MoveTab(ctx, "", ToIndex(0)); return err },
		"flag-shaped tab id": func() error { _, err := c.MoveTab(ctx, "--index", ToIndex(0)); return err },
		"empty workspace":    func() error { _, err := c.MoveTab(ctx, "w1:t1", ToWorkspace("")); return err },
		"flag-shaped ws":     func() error { _, err := c.MoveTab(ctx, "w1:t1", ToWorkspace("--x")); return err },
		"flag-shaped label":  func() error { _, err := c.MoveTab(ctx, "w1:t1", ToNewWorkspace("--x")); return err },
		"negative index":     func() error { _, err := c.MoveTab(ctx, "w1:t1", ToIndex(-1)); return err },
		"ws move negative":   func() error { return c.MoveWorkspace(ctx, "w1", -1) },
		"ws move empty id":   func() error { return c.MoveWorkspace(ctx, "", 0) },
		"focus ws empty":     func() error { return c.FocusWorkspace(ctx, "") },
		"focus tab flag":     func() error { return c.FocusTab(ctx, "-h") },
	} {
		if fn() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFocusAndWorkspaceMoveArgv(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		call func(*Client) error
		want []string
	}{
		{"focus workspace", func(c *Client) error { return c.FocusWorkspace(ctx, "w2") }, []string{"workspace", "focus", "w2"}},
		{"focus tab", func(c *Client) error { return c.FocusTab(ctx, "w2:t3") }, []string{"tab", "focus", "w2:t3"}},
		{"move workspace", func(c *Client) error { return c.MoveWorkspace(ctx, "w2", 1) }, []string{"workspace", "move", "w2", "--index", "1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := runnerFor(`{"id":"x","result":{"type":"ok"}}`, nil)
			if err := tt.call(New(r)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.Last().Args, tt.want) {
				t.Errorf("argv = %v, want %v", r.Last().Args, tt.want)
			}
		})
	}
}

func TestMutationsSurfaceHerdrErrors(t *testing.T) {
	ce := &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: `{"error":{"code":"tab_not_found","message":"tab w1:t9 not found"}}`}
	c := New(runnerFor("", ce))
	ctx := context.Background()
	var he *Error
	if _, err := c.MoveTab(ctx, "w1:t9", ToIndex(0)); !errors.As(err, &he) || he.Code != "tab_not_found" {
		t.Errorf("MoveTab err = %v", err)
	}
	if err := c.FocusTab(ctx, "w1:t9"); !errors.As(err, &he) || he.Code != "tab_not_found" {
		t.Errorf("FocusTab err = %v", err)
	}
}
