// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package herdr

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// appRunner answers `herdr-app ls` and `hc ls` from the recorded fixtures and
// records every other call.
func appRunner(t *testing.T, doErr error, doOut string) *exec.FakeRunner {
	t.Helper()
	lsOut, hcOut := fixture(t, "herdr_app_ls.json"), fixture(t, "herdr_app_hc_ls.json")
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == AppBinary && len(args) == 1 && args[0] == "ls":
			return lsOut, nil
		case name == HCBinary && len(args) > 0 && args[0] == "ls":
			return hcOut, nil
		case name == AppBinary && len(args) > 0 && args[0] == "do":
			return doOut, doErr
		case name == AppBinary && len(args) == 1 && args[0] == "commands":
			return fixture(t, "herdr_app_commands.json"), nil
		}
		t.Errorf("unexpected call %s %v", name, args)
		return "", nil
	}}
}

func TestAppWorkspacesMapsSpaces(t *testing.T) {
	got, err := NewApp(appRunner(t, nil, "")).Workspaces(context.Background())
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	w := got[0]
	if w.WorkspaceID != "sp8a41fc22e40d665646f4384830f2ff26" || w.Label != "Space 1" || w.Number != 1 || !w.Focused {
		t.Errorf("workspace = %+v", w)
	}
	if w.TabCount != 8 || w.ActiveTabID != "tb23e19309cbf2a46a2b1661ddc4854c32" {
		t.Errorf("tabs = %d active = %q", w.TabCount, w.ActiveTabID)
	}
	if w.PaneCount != 10 {
		t.Errorf("PaneCount = %d, want 10", w.PaneCount)
	}
}

func TestAppTabsKeepsOrderAndPositions(t *testing.T) {
	c := NewApp(appRunner(t, nil, ""))
	tabs, err := c.Tabs(context.Background(), "sp8a41fc22e40d665646f4384830f2ff26")
	if err != nil {
		t.Fatalf("Tabs: %v", err)
	}
	if len(tabs) != 8 {
		t.Fatalf("len = %d, want 8", len(tabs))
	}
	for i, tab := range tabs {
		if tab.Number != i+1 || tab.Label != "tab-"+string(rune('1'+i)) {
			t.Errorf("tab %d = %+v", i, tab)
		}
	}
	if !tabs[6].Focused || tabs[0].Focused {
		t.Errorf("focus flags wrong: %+v / %+v", tabs[0], tabs[6])
	}
	if _, err := c.Tabs(context.Background(), "sp-missing"); err == nil {
		t.Error("an unknown space must fail")
	}
}

func TestAppPanesJoinsFoldersAndFocus(t *testing.T) {
	panes, err := NewApp(appRunner(t, nil, "")).Panes(context.Background())
	if err != nil {
		t.Fatalf("Panes: %v", err)
	}
	if len(panes) != 10 {
		t.Fatalf("len = %d, want 10", len(panes))
	}
	focused := 0
	for _, p := range panes {
		if p.Focused {
			focused++
			if p.TabID != "tb23e19309cbf2a46a2b1661ddc4854c32" {
				t.Errorf("focused pane in tab %q", p.TabID)
			}
		}
		if p.CWD == "" || !strings.HasPrefix(p.CWD, "/Users/example/") {
			t.Errorf("pane %s has CWD %q: hc ls join failed", p.TerminalID, p.CWD)
		}
		if p.TerminalID == "" || p.PaneID == "" || p.TerminalTitleStripped == "" {
			t.Errorf("pane missing identity: %+v", p)
		}
	}
	if focused != 1 {
		t.Errorf("focused panes = %d, want exactly 1", focused)
	}
}

func TestAppReadsFailClosed(t *testing.T) {
	for name, out := range map[string]string{"not json": "nope", "no spaces key": `{"windows":[]}`} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewApp(runnerFor(out, nil)).Workspaces(context.Background()); err == nil {
				t.Error("want an error, got nil: a malformed ls must not read as an empty window")
			}
		})
	}
	boom := errors.New("no window")
	if _, err := NewApp(runnerFor("", boom)).Workspaces(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the runner's error wrapped", err)
	}
	lsOnly := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == AppBinary {
			return fixture(t, "herdr_app_ls.json"), nil
		}
		return "nope", nil
	}}
	if _, err := NewApp(lsOnly).Panes(context.Background()); err == nil {
		t.Error("hc ls output that is not JSON must fail: folders cannot be guessed")
	}
}

func lastDo(t *testing.T, r *exec.FakeRunner) []string {
	t.Helper()
	for i := len(r.Calls) - 1; i >= 0; i-- {
		if r.Calls[i].Name == AppBinary && len(r.Calls[i].Args) > 0 && r.Calls[i].Args[0] == "do" {
			return r.Calls[i].Args
		}
	}
	t.Fatal("no herdr-app do call recorded")
	return nil
}

func TestAppMoveTabSendsOneBasedIndex(t *testing.T) {
	r := appRunner(t, nil, `{"ok":true}`)
	res, err := NewApp(r).MoveTab(context.Background(), "tbabc", ToIndex(0))
	if err != nil {
		t.Fatalf("MoveTab: %v", err)
	}
	if res.TabID != "tbabc" {
		t.Errorf("result = %+v", res)
	}
	want := []string{"do", "tab.move", `{"id":"tbabc","index":1}`}
	if got := lastDo(t, r); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestAppMoveTabRefusesCrossSpaceBeforeRunning(t *testing.T) {
	r := &exec.FakeRunner{}
	c := NewApp(r)
	for _, to := range []MoveTarget{ToWorkspace("sp1"), ToNewWorkspace("x"), {}} {
		if _, err := c.MoveTab(context.Background(), "tbabc", to); !errors.Is(err, ErrUnsupportedMove) {
			t.Errorf("target %+v: err = %v, want ErrUnsupportedMove", to, err)
		}
	}
	if len(r.Calls) != 0 {
		t.Errorf("calls = %+v, want none", r.Calls)
	}
}

func TestAppDoSendsTheRightCommands(t *testing.T) {
	r := appRunner(t, nil, "")
	c := NewApp(r)
	ctx := context.Background()
	steps := []struct {
		run  func() error
		want []string
	}{
		{func() error { return c.MoveWorkspace(ctx, "sp1", 2) }, []string{"do", "space.move", `{"id":"sp1","index":3}`}},
		{func() error { return c.FocusWorkspace(ctx, "sp1") }, []string{"do", "space.select", `{"id":"sp1"}`}},
		{func() error { return c.FocusTab(ctx, "tb1") }, []string{"do", "tab.select", `{"id":"tb1"}`}},
	}
	for _, s := range steps {
		if err := s.run(); err != nil {
			t.Fatalf("%v: %v", s.want, err)
		}
		if got := lastDo(t, r); !reflect.DeepEqual(got, s.want) {
			t.Errorf("argv = %v, want %v", got, s.want)
		}
	}
}

func TestAppDoReportsRefusals(t *testing.T) {
	ctx := context.Background()
	// Exit 0 with an error object is a refusal, not a success.
	if err := NewApp(appRunner(t, nil, `{"error":{"message":"no such tab"}}`)).FocusTab(ctx, "tb1"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("err = %v, want a refusal", err)
	}
	boom := errors.New("exit 1")
	if err := NewApp(appRunner(t, boom, "")).FocusTab(ctx, "tb1"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the runner's error", err)
	}
	// Bad operands never reach the runner.
	r := &exec.FakeRunner{}
	c := NewApp(r)
	if err := c.FocusTab(ctx, "--help"); err == nil {
		t.Error("a flag-like id must be refused")
	}
	if err := c.MoveWorkspace(ctx, "sp1", -1); err == nil {
		t.Error("a negative index must be refused")
	}
	if len(r.Calls) != 0 {
		t.Errorf("calls = %+v, want none", r.Calls)
	}
}

func TestCheckApp(t *testing.T) {
	if err := CheckApp(context.Background(), appRunner(t, nil, "")); err != nil {
		t.Errorf("recorded command list should pass: %v", err)
	}
	// A list without tab.move is refused.
	err := CheckApp(context.Background(), runnerFor(`[{"name":"space.move"},{"name":"tab.select"},{"name":"space.select"}]`, nil))
	if err == nil || !strings.Contains(err.Error(), "tab.move") {
		t.Errorf("err = %v, want a refusal naming tab.move", err)
	}
	if err := CheckApp(context.Background(), runnerFor("garbage", nil)); err == nil {
		t.Error("a non-JSON list must fail")
	}
}
