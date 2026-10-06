// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// belowStep is one expected mutating herdr call. want builds the command from
// the stub's state at call time (pane ids renumber after every call), reply is
// herdr's answer, and apply is the effect on the stub's tab.
type belowStep struct {
	want  func(f *fakeHerdrTab) exec.SensitiveCommand
	reply func(f *fakeHerdrTab) string
	apply func(f *fakeHerdrTab)
	fail  bool
}

// belowStub is a stub herdr for `desk layout --below`: the reads come from
// fakeHerdrTab, and each mutating call must be the next scripted step. A call
// that is not the next step fails the test and the run, so the sequence is the
// assertion; the stub then applies the step's effect to which tab each
// terminal is in, which the reads report back.
type belowStub struct {
	t     *testing.T
	tab   *fakeHerdrTab
	steps []belowStep
	mu    sync.Mutex
	n     int
}

func (b *belowStub) sensitive() *exec.FakeSensitiveRunner {
	return &exec.FakeSensitiveRunner{RunFunc: func(cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.tab.mu.Lock()
		defer b.tab.mu.Unlock()
		i := b.n
		b.n++
		if i >= len(b.steps) {
			b.t.Errorf("unexpected extra herdr call %d (%s)", i, cmd.Kind)
			return exec.SensitiveResult{}, fmt.Errorf("unexpected call")
		}
		st := b.steps[i]
		if want := st.want(b.tab); !cmd.Equal(want) {
			b.t.Errorf("herdr call %d (%s) is not the expected command", i, cmd.Kind)
			return exec.SensitiveResult{}, fmt.Errorf("unexpected call")
		}
		if st.fail {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(cmd.Kind, exec.OutcomeExit)
		}
		reply := st.reply(b.tab)
		st.apply(b.tab)
		if cmd.Kind == exec.KindHerdrPaneMove || cmd.Kind == exec.KindHerdrPaneSplit {
			b.tab.gen += 10 // a layout change renumbers every pane id
		}
		return exec.SensitiveResult{Stdout: exec.BoundedOutputForTest([]byte(reply), exec.OutputComplete)}, nil
	}}
}

func (b *belowStub) done() {
	b.t.Helper()
	if b.n != len(b.steps) {
		b.t.Errorf("%d herdr calls, want %d", b.n, len(b.steps))
	}
}

func paneCmdOf(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind: kind, Path: exec.Secret("/opt/test/bin/herdr"),
		Args:      append([]exec.Arg{exec.MustFixed("pane")}, args...),
		StdoutCap: 64 << 10, StderrCap: 64 << 10,
	}
}

// moveStep scripts one `pane move` of terminal mover. into is the tab it ends
// in; newTab makes that tab; target (a terminal, "" for none) and keep give the
// split.
func moveStep(mover, target, into string, newTab bool, keep string) belowStep {
	return belowStep{
		want: func(f *fakeHerdrTab) exec.SensitiveCommand {
			args := []exec.Arg{exec.MustFixed("move"), exec.Opaque(f.paneID(mover))}
			if newTab {
				args = append(args, exec.MustFixed("--new-tab"))
			} else {
				args = append(args, exec.MustFixed("--tab"), exec.Opaque(into),
					exec.MustFixed("--target-pane"), exec.Opaque(f.paneID(target)),
					exec.MustFixed("--split"), exec.MustFixed("right"), exec.MustFixed("--ratio"), exec.Opaque(keep))
			}
			return paneCmdOf(exec.KindHerdrPaneMove, append(args, exec.MustFixed("--no-focus"))...)
		},
		reply: func(f *fakeHerdrTab) string {
			mr := map[string]any{"changed": true, "pane": herdr.Pane{PaneID: "w1:p99", TerminalID: mover, TabID: into}}
			if newTab {
				mr["created_tab"] = map[string]any{"tab_id": into}
			}
			b, _ := json.Marshal(map[string]any{"id": "x", "result": map[string]any{"type": "pane_move", "move_result": mr}})
			return string(b)
		},
		apply: func(f *fakeHerdrTab) { f.tabOf[mover] = into },
	}
}

func splitStep(from, dir, keep string, newTerm string, fail bool) belowStep {
	return belowStep{
		fail: fail,
		want: func(f *fakeHerdrTab) exec.SensitiveCommand {
			args := []exec.Arg{exec.MustFixed("split")}
			if from == "" {
				args = append(args, exec.MustFixed("--current"))
			} else {
				args = append(args, exec.MustFixed("--pane"), exec.Opaque(f.paneID(from)))
			}
			return paneCmdOf(exec.KindHerdrPaneSplit, append(args,
				exec.MustFixed("--direction"), dirArg(dir), exec.MustFixed("--ratio"), exec.Opaque(keep),
				exec.MustFixed("--cwd"), exec.Opaque("/work/repo"), exec.MustFixed("--no-focus"))...)
		},
		reply: func(f *fakeHerdrTab) string {
			f.terminals = append(f.terminals, newTerm)
			f.tabOf[newTerm] = "w1:t1"
			b, _ := json.Marshal(map[string]any{"id": "x", "result": map[string]any{"pane": herdr.Pane{PaneID: f.paneID(newTerm), TerminalID: newTerm}}})
			return string(b)
		},
		apply: func(*fakeHerdrTab) {},
	}
}

func dirArg(d string) exec.Arg {
	if d == "down" {
		return exec.MustFixed("down")
	}
	return exec.MustFixed("right")
}

func renameStep(term, label string) belowStep {
	return belowStep{
		want: func(f *fakeHerdrTab) exec.SensitiveCommand {
			return paneCmdOf(exec.KindHerdrPaneRename, exec.MustFixed("rename"), exec.Opaque(f.paneID(term)), exec.Opaque(label))
		},
		reply: func(*fakeHerdrTab) string { return `{"id":"x","result":{"type":"ok"}}` },
		apply: func(*fakeHerdrTab) {},
	}
}

func runStep(term, command string) belowStep {
	return belowStep{
		want: func(f *fakeHerdrTab) exec.SensitiveCommand {
			return paneCmdOf(exec.KindHerdrPaneRun, exec.MustFixed("run"), exec.Opaque(f.paneID(term)), exec.Opaque(command))
		},
		reply: func(*fakeHerdrTab) string { return `{"id":"x","result":{"type":"ok"}}` },
		apply: func(*fakeHerdrTab) {},
	}
}

// belowTab is the tab of the issue: the caller and two panes already there.
func belowTab(t *testing.T, others ...string) (*fakeHerdrTab, *belowStub) {
	t.Helper()
	newDeskDir(t)
	inHerdr(t, "w1:p0")
	tab := &fakeHerdrTab{terminals: append([]string{"term_claude"}, others...), tabOf: map[string]string{}}
	return tab, &belowStub{t: t, tab: tab}
}

func TestDeskLayoutBelow_ParksTheOtherPanesAndBringsThemBack(t *testing.T) {
	tab, stub := belowTab(t, "term_a", "term_b")
	stub.steps = []belowStep{
		moveStep("term_a", "", "w1:t2", true, ""),
		moveStep("term_b", "term_a", "w1:t2", false, "0.50"),
		splitStep("", "down", "0.72", "term_desk", false),
		moveStep("term_a", "term_claude", "w1:t1", false, "0.33"),
		moveStep("term_b", "term_a", "w1:t1", false, "0.50"),
		splitStep("term_desk", "right", "0.60", "term_prog", false),
		renameStep("term_desk", "desk"),
		renameStep("term_prog", "progress"),
		runStep("term_desk", "/opt/test/bin/forgectl desk --dir '/state/my desk'"),
		runStep("term_prog", "claude-desk progress"),
	}
	deps := module.Deps{Theme: theme.Default(), Runner: tab.runner(t), SensitiveRunner: stub.sensitive()}
	out, _, err := deskRun(t, deps, "layout", "--below", "--dir", "/state/my desk", "--progress", "claude-desk progress")
	wantExit(t, err, 0)
	stub.done()
	for _, term := range []string{"term_claude", "term_a", "term_b", "term_desk", "term_prog"} {
		if got := tab.tabOf[term]; term != "term_claude" && got != "w1:t1" {
			t.Errorf("%s ended in tab %q, want w1:t1", term, got)
		}
	}
	if !strings.HasSuffix(out, "moved=2\n") || !strings.HasPrefix(out, "desk=") {
		t.Errorf("output = %q", out)
	}
}

// With no other pane the desk is a plain down split: nothing is moved.
func TestDeskLayoutBelow_AloneInTheTabMovesNothing(t *testing.T) {
	tab, stub := belowTab(t)
	stub.steps = []belowStep{
		splitStep("", "down", "0.72", "term_desk", false),
		renameStep("term_desk", "desk"),
		runStep("term_desk", "/opt/test/bin/forgectl desk --dir /state/desk"),
	}
	deps := module.Deps{Theme: theme.Default(), Runner: tab.runner(t), SensitiveRunner: stub.sensitive()}
	out, _, err := deskRun(t, deps, "layout", "--below", "--dir", "/state/desk")
	wantExit(t, err, 0)
	stub.done()
	if !strings.HasSuffix(out, "moved=0\n") {
		t.Errorf("output = %q", out)
	}
}

// A failure after the other panes were parked moves them back before the
// error returns, so no process is left in a temporary tab.
func TestDeskLayoutBelow_RestoresTheParkedPanesWhenTheDeskSplitFails(t *testing.T) {
	tab, stub := belowTab(t, "term_a")
	stub.steps = []belowStep{
		moveStep("term_a", "", "w1:t2", true, ""),
		splitStep("", "down", "0.72", "term_desk", true),
		moveStep("term_a", "term_claude", "w1:t1", false, "0.50"),
	}
	deps := module.Deps{Theme: theme.Default(), Runner: tab.runner(t), SensitiveRunner: stub.sensitive()}
	_, _, err := deskRun(t, deps, "layout", "--below")
	wantExit(t, err, 1)
	if !strings.Contains(err.Error(), "split off the desk pane") {
		t.Errorf("err = %v", err)
	}
	stub.done()
	if got := tab.tabOf["term_a"]; got != "w1:t1" {
		t.Errorf("term_a is in tab %q after the failure, want w1:t1", got)
	}
}

func TestDeskLayoutBelow_DryRunMovesNothing(t *testing.T) {
	tab, stub := belowTab(t, "term_a", "term_b")
	run := stub.sensitive()
	reads := tab.runner(t)
	deps := module.Deps{Theme: theme.Default(), Runner: reads, SensitiveRunner: run}
	out, _, err := deskRun(t, deps, "layout", "--below", "--dry-run", "--dir", "/state/desk", "--progress", "claude-desk progress")
	wantExit(t, err, 0)
	if n := stub.n; n != 0 {
		t.Fatalf("--dry-run made %d mutating herdr calls", n)
	}
	want := `dry-run: no pane is moved, split, renamed or started
move.out=w1:p1 to=new-tab
move.out=w1:p2 to=parked-tab direction=right ratio=0.50
split=desk from=current direction=down ratio=0.72
move.back=w1:p1 to=this-tab direction=right ratio=0.33
move.back=w1:p2 to=this-tab direction=right ratio=0.50
split=progress from=desk direction=right ratio=0.60
rename=desk
rename=progress
run.desk=/opt/test/bin/forgectl desk --dir /state/desk
run.progress=claude-desk progress
moved=2
`
	if out != want {
		t.Errorf("output =\n%s\nwant\n%s", out, want)
	}
}

func TestDeskLayoutBelow_RefusesWidth(t *testing.T) {
	tab, stub := belowTab(t, "term_a")
	deps := module.Deps{Theme: theme.Default(), Runner: tab.runner(t), SensitiveRunner: stub.sensitive()}
	_, _, err := deskRun(t, deps, "layout", "--below", "--width", "70")
	wantExit(t, err, deskExitUsage)
	if stub.n != 0 {
		t.Error("a refused layout still called herdr")
	}
}

func TestEvenKeep(t *testing.T) {
	for _, tc := range []struct {
		i, n int
		want float64
	}{{1, 2, 0.5}, {1, 3, 0.33}, {2, 3, 0.5}, {1, 4, 0.25}, {2, 4, 0.33}, {3, 4, 0.5}} {
		if got := evenKeep(tc.i, tc.n); got != tc.want {
			t.Errorf("evenKeep(%d, %d) = %v, want %v", tc.i, tc.n, got, tc.want)
		}
	}
}
