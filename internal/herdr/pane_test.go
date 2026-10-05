package herdr

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func paneCmd(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind:      kind,
		Path:      exec.Secret(testHerdrPath),
		Args:      append([]exec.Arg{exec.MustFixed("pane")}, args...),
		StdoutCap: paneStreamCap,
		StderrCap: paneStreamCap,
	}
}

func replying(out string) *exec.FakeSensitiveRunner {
	return &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte(out), exec.OutputComplete)}, nil
	}}
}

func lastCmd(t *testing.T, run *exec.FakeSensitiveRunner) exec.SensitiveCommand {
	t.Helper()
	got, ok := run.Last()
	if !ok {
		t.Fatal("no command ran")
	}
	return got
}

func TestPaneSplitBuildsTheCommandAndReadsThePane(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Split
		want exec.SensitiveCommand
	}{
		{
			name: "the caller's pane, to the right, with a cwd",
			s:    Split{Direction: SplitRight, Ratio: 0.623, CWD: "/Users/example/Projects/repo-1"},
			want: paneCmd(exec.KindHerdrPaneSplit, exec.MustFixed("split"), exec.MustFixed("--current"),
				exec.MustFixed("--direction"), exec.MustFixed("right"), exec.MustFixed("--ratio"), exec.Opaque("0.62"),
				exec.MustFixed("--cwd"), exec.Opaque("/Users/example/Projects/repo-1"), exec.MustFixed("--no-focus")),
		},
		{
			name: "a named pane, down",
			s:    Split{Pane: "w1:p3", Direction: SplitDown, Ratio: 0.4},
			want: paneCmd(exec.KindHerdrPaneSplit, exec.MustFixed("split"), exec.MustFixed("--pane"), exec.Opaque("w1:p3"),
				exec.MustFixed("--direction"), exec.MustFixed("down"), exec.MustFixed("--ratio"), exec.Opaque("0.40"),
				exec.MustFixed("--no-focus")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := replying(fixture(t, "pane_split.json"))
			p, err := PaneSplit(t.Context(), run, testHerdrPath, tc.s)
			if err != nil {
				t.Fatalf("PaneSplit: %v", err)
			}
			if p.PaneID != "w1:p3" || p.TerminalID != "term_3" {
				t.Errorf("pane = %+v, want w1:p3 / term_3", p)
			}
			if got := lastCmd(t, run); !got.Equal(tc.want) {
				t.Errorf("command = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPaneSplitRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		s    Split
	}{
		{"relative herdr path", "herdr", Split{Direction: SplitRight, Ratio: 0.5}},
		{"no direction", testHerdrPath, Split{Ratio: 0.5}},
		{"ratio 0", testHerdrPath, Split{Direction: SplitRight}},
		{"ratio 1", testHerdrPath, Split{Direction: SplitRight, Ratio: 1}},
		{"relative cwd", testHerdrPath, Split{Direction: SplitRight, Ratio: 0.5, CWD: "rel"}},
		{"flag-shaped pane", testHerdrPath, Split{Pane: "--current", Direction: SplitRight, Ratio: 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := replying(fixture(t, "pane_split.json"))
			if _, err := PaneSplit(t.Context(), run, tc.path, tc.s); err == nil {
				t.Fatal("want an error")
			}
			if len(run.Calls()) != 0 {
				t.Error("a refused split still ran")
			}
		})
	}
}

func TestPaneSplitNeedsAPaneInTheReply(t *testing.T) {
	for _, reply := range []string{
		`{"id":"x","result":{"type":"ok"}}`,
		`{"id":"x","result":{"pane":{"pane_id":"w1:p3"}}}`,
		`{"error":{"code":"pane_not_found","message":"pane not found"}}`,
	} {
		if _, err := PaneSplit(t.Context(), replying(reply), testHerdrPath, Split{Direction: SplitRight, Ratio: 0.5}); err == nil {
			t.Errorf("reply %s: want an error", reply)
		}
	}
}

func TestPaneRenameAndRun(t *testing.T) {
	run := replying(`{"id":"x","result":{"type":"ok"}}`)
	if err := PaneRename(t.Context(), run, testHerdrPath, "w1:p3", "desk"); err != nil {
		t.Fatal(err)
	}
	if got, want := lastCmd(t, run), paneCmd(exec.KindHerdrPaneRename, exec.MustFixed("rename"), exec.Opaque("w1:p3"), exec.Opaque("desk")); !got.Equal(want) {
		t.Errorf("rename = %v, want %v", got, want)
	}
	cmd := "forgectl desk --dir '/tmp/a b'; echo $HOME"
	if err := PaneRun(t.Context(), run, testHerdrPath, "w1:p3", cmd); err != nil {
		t.Fatal(err)
	}
	// One argv element, verbatim: the pane's shell is what reads it.
	if got, want := lastCmd(t, run), paneCmd(exec.KindHerdrPaneRun, exec.MustFixed("run"), exec.Opaque("w1:p3"), exec.Opaque(cmd)); !got.Equal(want) {
		t.Errorf("run = %v, want %v", got, want)
	}
}

func TestPaneRunRefusesUnsafeCommands(t *testing.T) {
	for _, cmd := range []string{"", "-rf", "one\ntwo", "bell\a", "esc\x1b[2J", "c1\u009b", strings.Repeat("x", MaxRunCommandBytes+1), "bad\xff"} {
		run := replying("")
		if err := PaneRun(t.Context(), run, testHerdrPath, "w1:p3", cmd); err == nil {
			t.Errorf("PaneRun(%q) ran", cmd)
		}
		if len(run.Calls()) != 0 {
			t.Errorf("PaneRun(%q): a refused command still ran", cmd)
		}
	}
	if err := PaneRename(t.Context(), replying(""), testHerdrPath, "w1:p3", "--clear"); err == nil {
		t.Error("PaneRename took a flag-shaped label")
	}
}

func TestPaneVerbsReadHerdrRefusal(t *testing.T) {
	envelope := []byte(`{"error":{"code":"pane_not_found","message":"pane not found"}}`)
	run := &exec.FakeSensitiveRunner{RunFunc: func(exec.SensitiveCommand) (exec.SensitiveResult, error) {
		return exec.SensitiveResult{ExitCode: exitFailure, Stderr: exec.BoundedOutputForTest(envelope, exec.OutputComplete)},
			exec.SensitiveErrorForTest(exec.KindHerdrPaneRun, exec.OutcomeExit)
	}}
	err := PaneRun(t.Context(), run, testHerdrPath, "w1:p3", "true")
	var he *Error
	if !errors.As(err, &he) || he.Code != "pane_not_found" {
		t.Fatalf("err = %v, want herdr's pane_not_found refusal", err)
	}
}

func TestCurrentLayoutAndPaneByTerminal(t *testing.T) {
	c := New(runnerFor(fixture(t, "pane_layout.json"), nil))
	l, err := c.CurrentLayout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if l.Area.Width != 175 || l.TabID != "w1:t1" {
		t.Errorf("layout = %+v", l)
	}
	if _, err := New(runnerFor(`{"id":"x","result":{"layout":{"area":{"width":0}}}}`, nil)).CurrentLayout(t.Context()); err == nil {
		t.Error("a layout with no width was accepted")
	}

	c = New(runnerFor(fixture(t, "pane_list.json"), nil))
	panes, err := c.Panes(t.Context())
	if err != nil || len(panes) == 0 {
		t.Fatalf("Panes: %v %v", panes, err)
	}
	want := panes[len(panes)-1]
	got, err := c.PaneByTerminal(t.Context(), want.TerminalID)
	if err != nil || got.PaneID != want.PaneID {
		t.Errorf("PaneByTerminal = %+v, %v; want %s", got, err, want.PaneID)
	}
	if _, err := c.PaneByTerminal(t.Context(), "term_missing"); err == nil {
		t.Error("an unknown terminal resolved")
	}
}
