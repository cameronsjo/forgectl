// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// fakeHerdrTab is one herdr tab for the layout tests: a 175-column tab with
// the caller's pane, where every split adds a pane and renumbers the pane ids
// of every pane (the worst case for a caller that holds an id).
type fakeHerdrTab struct {
	mu        sync.Mutex
	terminals []string // in creation order; pane i is "w1:p<i+gen>"
	gen       int
}

func (f *fakeHerdrTab) paneID(term string) string {
	for i, t := range f.terminals {
		if t == term {
			return fmt.Sprintf("w1:p%d", i+f.gen)
		}
	}
	return ""
}

func (f *fakeHerdrTab) panes() []herdr.Pane {
	out := make([]herdr.Pane, 0, len(f.terminals))
	for _, t := range f.terminals {
		out = append(out, herdr.Pane{PaneID: f.paneID(t), TabID: "w1:t1", WorkspaceID: "w1", TerminalID: t})
	}
	return out
}

// runner answers the Client's reads: `pane layout --current` and `pane list`.
func (f *fakeHerdrTab) runner(t *testing.T) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch strings.Join(args, " ") {
		case "pane layout --current":
			return `{"id":"x","result":{"layout":{"area":{"x":0,"y":0,"width":175,"height":59},"tab_id":"w1:t1","workspace_id":"w1","focused_pane_id":"w1:p0"}}}`, nil
		case "pane list":
			b, _ := json.Marshal(map[string]any{"id": "x", "result": map[string]any{"panes": f.panes()}})
			return string(b), nil
		}
		t.Errorf("unexpected herdr read %v", args)
		return "", fmt.Errorf("unexpected")
	}}
}

// sensitive answers the pane verbs; a split adds a terminal and renumbers.
func (f *fakeHerdrTab) sensitive() *exec.FakeSensitiveRunner {
	return &exec.FakeSensitiveRunner{RunFunc: func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := `{"id":"x","result":{"type":"ok"}}`
		if cmd.Kind == exec.KindHerdrPaneSplit {
			term := fmt.Sprintf("term_%d", len(f.terminals))
			f.terminals = append(f.terminals, term)
			f.gen += 10
			b, _ := json.Marshal(map[string]any{"id": "x", "result": map[string]any{"pane": herdr.Pane{PaneID: f.paneID(term), TerminalID: term}}})
			out = string(b)
			f.gen += 10 // renumber again after the reply, as a later move would
		}
		return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte(out), exec.OutputComplete)}, nil
	}}
}

// inHerdr makes the layout's session check pass: HERDR_ENV=1, a real socket,
// and a pane id.
func inHerdr(t *testing.T, paneID string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "hd") // short: macOS caps socket paths near 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	env := map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": sock}
	if paneID != "" {
		env["HERDR_PANE_ID"] = paneID
	}
	stubLayout(t, env)
}

func stubLayout(t *testing.T, env map[string]string) {
	t.Helper()
	prevEnv, prevPath, prevWd, prevSelf := deskLookupEnv, deskHerdrPath, deskGetwd, deskSelfBinary
	deskLookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	deskHerdrPath = func() (string, error) { return "/opt/test/bin/herdr", nil }
	deskGetwd = func() (string, error) { return "/work/repo", nil }
	deskSelfBinary = func() string { return "forgectl" }
	t.Cleanup(func() { deskLookupEnv, deskHerdrPath, deskGetwd, deskSelfBinary = prevEnv, prevPath, prevWd, prevSelf })
}

func TestDeskLayout_RefusesOutsideHerdr(t *testing.T) {
	newDeskDir(t)
	for _, env := range []map[string]string{
		{},
		{"HERDR_ENV": "0"},
	} {
		stubLayout(t, env)
		run := &exec.FakeSensitiveRunner{}
		_, _, err := deskRun(t, module.Deps{Theme: theme.Default(), SensitiveRunner: run}, "layout")
		wantExit(t, err, deskExitUsage)
		if !strings.Contains(err.Error(), "must run inside a herdr pane") {
			t.Errorf("err = %v", err)
		}
		if len(run.Calls()) != 0 {
			t.Error("a refused layout still called herdr")
		}
	}
	inHerdr(t, "")
	_, _, err := deskRun(t, deskDeps(), "layout")
	wantExit(t, err, deskExitUsage)
}

func TestDeskLayout_BuildsThePanes(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p0")
	tab := &fakeHerdrTab{terminals: []string{"term_claude"}}
	run := tab.sensitive()
	deps := module.Deps{Theme: theme.Default(), Runner: tab.runner(t), SensitiveRunner: run}
	out, _, err := deskRun(t, deps, "layout", "--dir", "/state/my desk", "--progress", "claude-desk progress")
	wantExit(t, err, 0)

	calls := run.Calls()
	var kinds []string
	for _, c := range calls {
		kinds = append(kinds, c.Kind.String())
	}
	want := []string{"herdr.pane-split", "herdr.pane-split", "herdr.pane-rename", "herdr.pane-rename", "herdr.pane-run", "herdr.pane-run"}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("calls = %v, want %v", kinds, want)
	}
	path := exec.Secret("/opt/test/bin/herdr")
	cmd := func(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
		return exec.SensitiveCommand{Kind: kind, Path: path, Args: append([]exec.Arg{exec.MustFixed("pane")}, args...), StdoutCap: 64 << 10, StderrCap: 64 << 10}
	}
	// The ids the fake hands out renumber after every split, so each call
	// below names the id the pane has at that moment, found by terminal.
	wantCalls := []exec.SensitiveCommand{
		// 175 columns, 66 wanted: the caller keeps 1-66/175 = 0.62.
		cmd(exec.KindHerdrPaneSplit, exec.MustFixed("split"), exec.MustFixed("--current"),
			exec.MustFixed("--direction"), exec.MustFixed("right"), exec.MustFixed("--ratio"), exec.Opaque("0.62"),
			exec.MustFixed("--cwd"), exec.Opaque("/work/repo"), exec.MustFixed("--no-focus")),
		cmd(exec.KindHerdrPaneSplit, exec.MustFixed("split"), exec.MustFixed("--pane"), exec.Opaque("w1:p21"),
			exec.MustFixed("--direction"), exec.MustFixed("down"), exec.MustFixed("--ratio"), exec.Opaque("0.40"),
			exec.MustFixed("--cwd"), exec.Opaque("/work/repo"), exec.MustFixed("--no-focus")),
		cmd(exec.KindHerdrPaneRename, exec.MustFixed("rename"), exec.Opaque("w1:p41"), exec.Opaque("desk")),
		cmd(exec.KindHerdrPaneRename, exec.MustFixed("rename"), exec.Opaque("w1:p42"), exec.Opaque("progress")),
		cmd(exec.KindHerdrPaneRun, exec.MustFixed("run"), exec.Opaque("w1:p41"), exec.Opaque("forgectl desk --dir '/state/my desk'")),
		cmd(exec.KindHerdrPaneRun, exec.MustFixed("run"), exec.Opaque("w1:p42"), exec.Opaque("claude-desk progress")),
	}
	for i, w := range wantCalls {
		if !calls[i].Equal(w) {
			t.Errorf("call %d (%s) is not the expected command", i, kinds[i])
		}
	}
	if out != "desk=w1:p41\nprogress=w1:p42\ncolumns=67 of 175\n" {
		t.Errorf("output = %q", out)
	}
}

func TestDeskLayout_WithoutProgressAndBadFlags(t *testing.T) {
	newDeskDir(t)
	inHerdr(t, "w1:p0")
	tab := &fakeHerdrTab{terminals: []string{"term_claude"}}
	run := tab.sensitive()
	deps := module.Deps{Theme: theme.Default(), Runner: tab.runner(t), SensitiveRunner: run}
	_, _, err := deskRun(t, deps, "layout", "--width", "300")
	wantExit(t, err, 0)
	if n := len(run.Calls()); n != 3 {
		t.Errorf("%d herdr calls, want split, rename, run", n)
	}
	for _, args := range [][]string{
		{"layout", "--width", "5"},
		{"layout", "--progress", "two\nlines"},
		{"layout", "--progress", "-x"},
	} {
		_, _, err := deskRun(t, deps, args...)
		wantExit(t, err, deskExitUsage)
	}
}

func TestDeskSplitRatio(t *testing.T) {
	for _, tc := range []struct {
		total, want int
		ratio       float64
	}{
		{175, 66, 0.62},
		{100, 66, 0.45}, // clamped: the caller keeps at least 45%
		{400, 66, 0.8},  // clamped: the desk gets at least 20%
	} {
		if got := deskSplitRatio(tc.total, tc.want); got != tc.ratio {
			t.Errorf("deskSplitRatio(%d, %d) = %v, want %v", tc.total, tc.want, got, tc.ratio)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"forgectl":        "forgectl",
		"/state/desk":     "/state/desk",
		"/state/my desk":  "'/state/my desk'",
		"it's":            `'it'\''s'`,
		"$HOME":           "'$HOME'",
		"/opt/a;rm -rf /": "'/opt/a;rm -rf /'",
	} {
		if got, err := shellQuote(in); err != nil || got != want {
			t.Errorf("shellQuote(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := shellQuote("a\nb"); err == nil {
		t.Error("a newline was quoted")
	}
}
