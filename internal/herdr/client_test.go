package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// runnerFor returns a FakeRunner that answers every call with out and err.
func runnerFor(out string, err error) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, err }}
}

func TestWorkspacesDecodesFixture(t *testing.T) {
	c := New(runnerFor(fixture(t, "workspace_list.json"), nil))
	got, err := c.Workspaces(context.Background())
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0].WorkspaceID != "w1" || got[0].TabCount != 4 || !got[0].Focused {
		t.Errorf("first workspace = %+v", got[0])
	}
}

func TestTabsPassesWorkspaceAndDecodes(t *testing.T) {
	r := runnerFor(fixture(t, "tab_list.json"), nil)
	got, err := New(r).Tabs(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Tabs: %v", err)
	}
	if len(got) == 0 || got[0].TabID == "" {
		t.Fatalf("tabs = %+v", got)
	}
	want := []string{"tab", "list", "--workspace", "w1"}
	if c := r.Last(); c.Name != Binary || !reflect.DeepEqual(c.Args, want) {
		t.Errorf("argv = %s %v, want %s %v", c.Name, c.Args, Binary, want)
	}
}

func TestPanesAndAgentsDecodeFixtures(t *testing.T) {
	panes, err := New(runnerFor(fixture(t, "pane_list.json"), nil)).Panes(context.Background())
	if err != nil {
		t.Fatalf("Panes: %v", err)
	}
	if len(panes) != 18 {
		t.Fatalf("panes = %d, want 18", len(panes))
	}
	agents, err := New(runnerFor(fixture(t, "agent_list.json"), nil)).Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) == 0 || agents[0].Agent == "" || agents[0].AgentSession == nil || agents[0].TerminalID == "" {
		t.Fatalf("agents[0] = %+v", agents[0])
	}
}

func TestGetsDecodeFixtures(t *testing.T) {
	pane, err := New(runnerFor(fixture(t, "pane_get.json"), nil)).PaneGet(context.Background(), "w1:p1")
	if err != nil || pane.PaneID == "" || pane.TerminalID == "" {
		t.Fatalf("PaneGet = %+v, %v", pane, err)
	}
	tab, err := New(runnerFor(fixture(t, "tab_get.json"), nil)).TabGet(context.Background(), "w1:t2")
	if err != nil || tab.TabID == "" {
		t.Fatalf("TabGet = %+v, %v", tab, err)
	}
}

func TestDecodeEdgeCases(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name    string
		out     string
		wantLen int
		wantErr bool
	}{
		{"empty list", `{"id":"x","result":{"type":"pane_list","panes":[]}}`, 0, false},
		{"list member missing", `{"id":"x","result":{"type":"pane_list"}}`, 0, false},
		{"null cwd and agent", `{"id":"x","result":{"panes":[{"pane_id":"w1:p1","cwd":null,"agent":null,"agent_session":null}]}}`, 1, false},
		{"unknown extra fields", `{"id":"x","extra":1,"result":{"panes":[{"pane_id":"w1:p1","brand_new_field":{"a":1}}],"more":true}}`, 1, false},
		{"result missing", `{"id":"x"}`, 0, true},
		{"result null", `{"id":"x","result":null}`, 0, true},
		{"not json", `not json`, 0, true},
		{"empty output", ``, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(runnerFor(tt.out, nil)).Panes(ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestNullFieldsDecodeToZeroValues(t *testing.T) {
	out := `{"id":"x","result":{"panes":[{"pane_id":"w1:p1","cwd":null,"agent":null,"agent_session":null}]}}`
	got, err := New(runnerFor(out, nil)).Panes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].CWD != "" || got[0].Agent != "" || got[0].AgentSession != nil {
		t.Errorf("pane = %+v", got[0])
	}
}

func TestReadPaneReturnsTextAndPinsArgv(t *testing.T) {
	r := runnerFor(fixture(t, "pane_read.txt"), nil)
	got, err := New(r).ReadPane(context.Background(), "w1:p1", ReadRecent, 2)
	if err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	if !strings.HasPrefix(got, "synthetic pane output line one") {
		t.Errorf("text = %q", got)
	}
	want := []string{"pane", "read", "w1:p1", "--source", "recent", "--format", "text", "--lines", "2"}
	if !reflect.DeepEqual(r.Last().Args, want) {
		t.Errorf("argv = %v, want %v", r.Last().Args, want)
	}
	if _, err := New(r).ReadPane(context.Background(), "w1:p1", ReadRecent, 0); err != nil || strings.Contains(strings.Join(r.Last().Args, " "), "--lines") {
		t.Errorf("lines <= 0 must omit --lines; argv = %v, err = %v", r.Last().Args, err)
	}
}

func TestReadPaneRefusesBadInput(t *testing.T) {
	c := New(runnerFor("", nil))
	ctx := context.Background()
	if _, err := c.ReadPane(ctx, "w1:p1", ReadSource("bogus"), 1); err == nil {
		t.Error("unknown source accepted")
	}
	for _, id := range []string{"", "-h", "--source"} {
		if _, err := c.ReadPane(ctx, id, ReadRecent, 1); err == nil {
			t.Errorf("id %q accepted", id)
		}
		if _, err := c.PaneGet(ctx, id); err == nil {
			t.Errorf("PaneGet id %q accepted", id)
		}
		if _, err := c.TabGet(ctx, id); err == nil {
			t.Errorf("TabGet id %q accepted", id)
		}
		if _, err := c.Tabs(ctx, id); err == nil {
			t.Errorf("Tabs id %q accepted", id)
		}
	}
}

func TestReadPaneFailurePathCarriesEnvelope(t *testing.T) {
	ce := &exec.CommandError{Name: Binary, ExitCode: 1, Stderr: `{"error":{"code":"pane_not_found","message":"pane w1:p9 not found"},"id":"cli:pane:read"}`}
	_, err := New(runnerFor("", ce)).ReadPane(context.Background(), "w1:p9", ReadRecent, 1)
	var he *Error
	if !errors.As(err, &he) || he.Code != "pane_not_found" {
		t.Fatalf("err = %v, want *Error pane_not_found", err)
	}
}

func TestFixturesJoin(t *testing.T) {
	ctx := context.Background()
	ws, _ := New(runnerFor(fixture(t, "workspace_list.json"), nil)).Workspaces(ctx)
	panes, _ := New(runnerFor(fixture(t, "pane_list.json"), nil)).Panes(ctx)
	agents, _ := New(runnerFor(fixture(t, "agent_list.json"), nil)).Agents(ctx)
	tabs, _ := New(runnerFor(fixture(t, "tab_list.json"), nil)).Tabs(ctx, "w1")
	pane, _ := New(runnerFor(fixture(t, "pane_get.json"), nil)).PaneGet(ctx, "w1:p1")
	tab, _ := New(runnerFor(fixture(t, "tab_get.json"), nil)).TabGet(ctx, "w1:t2")

	wsIDs, tabIDs, paneByID := map[string]bool{}, map[string]bool{}, map[string]Pane{}
	for _, w := range ws {
		wsIDs[w.WorkspaceID] = true
	}
	for _, p := range panes {
		tabIDs[p.TabID] = true
		paneByID[p.PaneID] = p
		if !wsIDs[p.WorkspaceID] || !strings.HasPrefix(p.TabID, p.WorkspaceID+":") || !strings.HasPrefix(p.PaneID, p.WorkspaceID+":") {
			t.Errorf("pane %+v does not join its workspace", p)
		}
	}
	for _, w := range ws {
		if !tabIDs[w.ActiveTabID] {
			t.Errorf("workspace %s active tab %s is in no pane", w.WorkspaceID, w.ActiveTabID)
		}
	}
	for _, tb := range tabs {
		if !tabIDs[tb.TabID] {
			t.Errorf("tab %s is in no pane", tb.TabID)
		}
	}
	for _, a := range agents {
		if p, ok := paneByID[a.PaneID]; !ok || p.TerminalID != a.TerminalID || p.TabID != a.TabID {
			t.Errorf("agent %+v does not join pane_list", a)
		}
	}
	if p, ok := paneByID[pane.PaneID]; !ok || p.TerminalID != pane.TerminalID {
		t.Errorf("pane_get %+v does not join pane_list", pane)
	}
	if !tabIDs[tab.TabID] {
		t.Errorf("tab_get %+v does not join pane_list", tab)
	}
}

func TestFixturesContainNoLiveValues(t *testing.T) {
	forbidden := []string{"cameron", "sjomba", "a86d73e4", "/Users/cam"}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, f := range forbidden {
			if strings.Contains(s, f) {
				t.Errorf("%s contains %q", e.Name(), f)
			}
		}
		for _, re := range liveShapes {
			if m := re.FindString(s); m != "" {
				t.Errorf("%s matches live-value shape %s: %q", e.Name(), re, m)
			}
		}
	}
}
