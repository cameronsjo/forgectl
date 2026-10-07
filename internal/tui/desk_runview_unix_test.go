// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// runHarness is a desk harness with the run view reading the same desk.
func runHarness(t *testing.T) *deskHarness {
	t.Helper()
	h := newDeskHarness(t)
	h.m.runs = runview.NewDeskSource(h.d)
	return h
}

// stageRun queues file, claims it and writes lines as its events. With rc
// at 0 or more the run is finished with it; below 0 it is left running,
// owned by this test process.
func stageRun(t *testing.T, h *deskHarness, file, body string, lines []string, rc int) (string, *desk.Run) {
	t.Helper()
	src := filepath.Join(t.TempDir(), file)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := h.d.Add(src, "a test run", "a test", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Claim(a.Name, a.SHA256); err != nil {
		t.Fatal(err)
	}
	run, err := h.d.BeginRun(a.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if err := run.Events.Emit(l); err != nil {
			t.Fatal(err)
		}
	}
	if rc >= 0 {
		if err := run.Finish(rc, "failed"); err != nil {
			t.Fatal(err)
		}
	}
	h.scan()
	return a.Name, run
}

const pipeManifest = "fetch -- true\nbuild after=fetch -- exit 3\nstage after=build -- true\ncheck after=fetch -- true\n"

var pipeLines = []string{
	"STEP-START id=fetch deps=",
	"STEP-END id=fetch rc=0 dur=0.5 reason=ok outputs= log=/x/fetch.log",
	"STEP-START id=build deps=fetch",
	"STEP-START id=check deps=fetch",
	"STEP-END id=build rc=3 dur=1.0 reason=failed outputs= log=/x/build.log",
	"STEP-SKIP id=stage reason=dep-failed:build",
	"STEP-END id=check rc=0 dur=0.2 reason=ok outputs= log=/x/check.log",
}

// send feeds one named or rune key straight to the model and runs what it
// returns.
func (h *deskHarness) send(k string) {
	h.t.Helper()
	var msg tea.KeyPressMsg
	switch k {
	case "left":
		msg = tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		msg = tea.KeyPressMsg{Code: tea.KeyRight}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		msg = key(k)
	}
	out, cmd := h.m.Update(msg)
	h.m = out.(deskModel)
	h.drive(cmd)
}

func (h *deskHarness) screen() string {
	h.t.Helper()
	return ansi.Strip(h.m.View().Content)
}

func TestRunViewShowsTheFlowAndTheEvents(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	h.send("r")
	if h.m.rv == nil {
		t.Fatal("r did not open the run view")
	}
	out := h.screen()
	for _, want := range []string{"run " + itemLabel(name), "✗ exit 1", "✓ fetch", "✗ build", "– stage", "✓ check", " → ", "STEP-SKIP stage", "9 events"} {
		if !strings.Contains(out, want) {
			t.Errorf("run view is missing %q:\n%s", want, out)
		}
	}
	h.send("q")
	if h.m.rv != nil {
		t.Fatal("q did not close the run view")
	}
	if !strings.Contains(h.screen(), "queue") {
		t.Errorf("closing the run view did not return to the dashboard:\n%s", h.screen())
	}
}

// Replay folds a prefix of the events: stepping back shows the run as it
// was, the events after the point dim, and G returns to the live tip.
func TestRunViewReplaysAndReturnsToLive(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	h.send("r")
	h.send("g")
	for range 4 { // RUN-START, fetch start and end, build start
		h.send("right")
	}
	out := h.screen()
	// The run finished with exit 1: mid-replay the header says so, not live
	// (#1106), and G goes to its end.
	for _, want := range []string{"✗ exit 1  replaying 4/9 · G end", "✓ fetch", "◐ build", "· stage", "▸ #4", "g/G start/end"} {
		if !strings.Contains(out, want) {
			t.Errorf("replay at 4 is missing %q:\n%s", want, out)
		}
	}
	if h.m.rv.follow {
		t.Error("a replay still follows the run")
	}
	h.send("G")
	if !h.m.rv.follow || h.m.rv.at != 9 || strings.Contains(h.screen(), "replay") {
		t.Errorf("G did not return to the live tip: at %d follow %v", h.m.rv.at, h.m.rv.follow)
	}
}

// space plays the replay forward one event per tick and follows the run
// again at its end.
func TestRunViewPlays(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	h.send("r")
	out, cmd := h.m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	h.m = out.(deskModel)
	if !h.m.rv.playing || h.m.rv.at != 0 || cmd == nil {
		t.Fatalf("space from the live tip should play from the start: playing %v at %d", h.m.rv.playing, h.m.rv.at)
	}
	for i := 1; i <= 9; i++ {
		out, _ := h.m.Update(deskRunPlayMsg{gen: h.m.rv.gen, seq: h.m.rv.playSeq})
		h.m = out.(deskModel)
		if h.m.rv.at != i {
			t.Fatalf("play step %d: at %d", i, h.m.rv.at)
		}
	}
	if h.m.rv.playing || !h.m.rv.follow {
		t.Error("play did not stop and follow at the end")
	}
	// A tick from an earlier view is dropped.
	h.m.rv.at = 3
	out, _ = h.m.Update(deskRunPlayMsg{gen: h.m.rv.gen + 1, seq: h.m.rv.playSeq})
	if out.(deskModel).rv.at != 3 {
		t.Error("a play tick for another view moved this one")
	}
}

// A running run is polled on the dashboard's tick: new events show without
// reopening the view.
func TestRunViewFollowsALiveRun(t *testing.T) {
	h := runHarness(t)
	name, run := stageRun(t, h, "go.sh", "echo hi\n", nil, -1)
	h.selectItem(name)
	h.send("r")
	if !strings.Contains(h.screen(), "◐ running") {
		t.Fatalf("a live script run should read running:\n%s", h.screen())
	}
	if err := run.Finish(0, "ok"); err != nil {
		t.Fatal(err)
	}
	h.drive(h.m.pollRun())
	out := h.screen()
	if !strings.Contains(out, "✓ ok") || !strings.Contains(out, "✓ script") {
		t.Errorf("the finished run did not show after a poll:\n%s", out)
	}
	if c := h.m.pollRun(); c != nil {
		t.Error("an ended run is still polled")
	}
}

// n and p move through the runs the source lists.
func TestRunViewSwitchesRuns(t *testing.T) {
	h := runHarness(t)
	first, _ := stageRun(t, h, "one.sh", "echo one\n", nil, 0)
	second, _ := stageRun(t, h, "two.sh", "echo two\n", nil, 2)
	h.selectItem(second)
	h.send("r")
	if got := h.m.rv.ref().Name; got != second {
		t.Fatalf("opened on %s, want %s", got, second)
	}
	h.send("n")
	if got := h.m.rv.ref().Name; got != first {
		t.Errorf("n moved to %s, want %s", got, first)
	}
	if !strings.Contains(h.screen(), "run 2 of 2") {
		t.Errorf("the header does not place the run:\n%s", h.screen())
	}
	h.send("p")
	if got := h.m.rv.ref().Name; got != second {
		t.Errorf("p moved to %s, want %s", got, second)
	}
}

// Text from an events file reaches the screen inert.
func TestRunViewDrawsHostileEventTextInert(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest,
		[]string{"STEP-START id=fetch deps=", "STEP-WARN id=fetch msg=\x1b]0;owned\x07\x1b[2Jgone"}, 0)
	h.selectItem(name)
	h.send("r")
	if out := h.screen(); !strings.Contains(out, "STEP-WARN fetch") || !strings.Contains(out, "gone") {
		t.Fatalf("the hostile event did not reach the screen at all:\n%s", out)
	}
	if out := h.m.View().Content; strings.Contains(out, "\x1b]0;") || strings.Contains(out, "\x1b[2J") || strings.ContainsRune(out, 0x07) {
		t.Errorf("an escape sequence from the events file reached the screen: %q", out)
	}
}

// A narrow window cannot fit the columns, so the flow becomes a list.
func TestRunFlowFallsBackToAListWhenNarrow(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	h.m.width = 30
	h.send("r")
	out := h.screen()
	if strings.Contains(out, " → ") {
		t.Errorf("a 30-column flow still draws columns:\n%s", out)
	}
	for _, want := range []string{"✓ fetch", "  ✗ build", "    – stage"} {
		if !strings.Contains(out, want) {
			t.Errorf("the list flow is missing %q:\n%s", want, out)
		}
	}
}

func TestRunViewWithNoRunsSaysSo(t *testing.T) {
	h := runHarness(t)
	h.send("r")
	if out := h.screen(); !strings.Contains(out, "no runs yet") {
		t.Errorf("an empty desk's run view should say there are no runs:\n%s", out)
	}
}

func TestStepDepths(t *testing.T) {
	s := runview.Fold(runview.DeskSpec(), []runview.StepDef{
		{ID: "a"}, {ID: "b", After: []string{"a"}}, {ID: "c", After: []string{"a"}}, {ID: "d", After: []string{"b", "c"}},
	}, nil)
	got := stepDepths(s)
	want := []int{0, 1, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("depths = %v, want %v", got, want)
		}
	}
}

// A load still on its way when the view closes must not land in the view
// opened next: each view gets its own generation.
func TestRunViewDropsALoadFromAClosedView(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	out, stale := h.m.Update(key("r")) // the load is not run yet
	h.m = out.(deskModel)
	oldGen := h.m.rv.gen
	h.send("q")
	h.send("r")
	if h.m.rv.gen == oldGen {
		t.Fatalf("a reopened view reused generation %d", oldGen)
	}
	before := h.m.rv.folder.Len()
	h.drive(stale) // the closed view's load lands now
	if got := h.m.rv.folder.Len(); got != before {
		t.Errorf("a closed view's load was applied: %d events, want %d", got, before)
	}
}

// Pausing and playing again leaves one tick chain: the paused play's tick is
// dropped.
func TestRunViewPauseAndResumeRunsOneChain(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	h.send("r")
	space := tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	out, _ := h.m.Update(space) // play: tick A pending
	h.m = out.(deskModel)
	first := h.m.rv.playSeq
	out, _ = h.m.Update(space) // pause
	h.m = out.(deskModel)
	out, _ = h.m.Update(space) // play again: tick B pending
	h.m = out.(deskModel)
	at := h.m.rv.at
	out, _ = h.m.Update(deskRunPlayMsg{gen: h.m.rv.gen, seq: first}) // tick A arrives
	h.m = out.(deskModel)
	if h.m.rv.at != at {
		t.Errorf("the paused play's tick still advanced the replay: at %d, want %d", h.m.rv.at, at)
	}
	out, _ = h.m.Update(deskRunPlayMsg{gen: h.m.rv.gen, seq: h.m.rv.playSeq}) // tick B
	h.m = out.(deskModel)
	if h.m.rv.at != at+1 {
		t.Errorf("the current play's tick did not advance: at %d, want %d", h.m.rv.at, at+1)
	}
}

// With room for one flow row, the list shows the first step rather than only
// a count of the rest.
func TestRunFlowWithOneRowShowsAStep(t *testing.T) {
	st := theme.Default().Styles()
	s := runview.Fold(runview.DeskSpec(), []runview.StepDef{{ID: "fetch"}, {ID: "build"}, {ID: "stage"}}, nil)
	out := runFlow(st, runview.IconGlyphs, s, nil, 12, 1)
	if len(out) != 1 || !strings.Contains(ansi.Strip(out[0]), "fetch") {
		t.Errorf("one row of flow = %q, want the first step", out)
	}
}

// A reset while replaying keeps the replay point inside the events.
func TestRunViewResetClampsTheReplayPoint(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "pipe.manifest", pipeManifest, pipeLines, 1)
	h.selectItem(name)
	h.send("r")
	h.send("left") // replay at 8 of 9
	v := h.m.rv
	out, _ := h.m.Update(deskRunLoadMsg{gen: v.gen, ref: v.ref(), cur: v.cur, delta: runview.Delta{Reset: true, Defs: v.defs,
		Events: []runview.Event{{Seq: 1, Name: "RUN-START"}, {Seq: 2, Name: "STEP-START", Step: "fetch"}}}})
	h.m = out.(deskModel)
	if h.m.rv.at != 2 || h.m.rv.folder.Len() != 2 {
		t.Errorf("after a reset to 2 events: at %d of %d, want 2 of 2", h.m.rv.at, h.m.rv.folder.Len())
	}
	if !h.m.rv.follow {
		t.Error("a replay clamped to the tip should follow the run again")
	}
}

// A run that leaves the desk while its view is open stops being polled.
func TestRunViewStopsPollingAGoneRun(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "go.sh", "echo hi\n", nil, -1)
	h.selectItem(name)
	h.send("r")
	v := h.m.rv
	out, _ := h.m.Update(deskRunLoadMsg{gen: v.gen, ref: v.ref(), cur: v.cur, err: desk.ErrNotFound})
	h.m = out.(deskModel)
	if c := h.m.pollRun(); c != nil {
		t.Error("a run the desk no longer has is still polled")
	}
}

// r on an item with no run says so on the dashboard; it does not open
// another item's run, which would read as this item's (#1106).
func TestRunViewDoesNotOpenAnotherItemsRun(t *testing.T) {
	h := runHarness(t)
	stageRun(t, h, "done.sh", "echo done\n", nil, 0)
	h.drop("02-later.sh", "#!/bin/bash\necho later\n")
	h.scan()
	h.selectItem("02-later")
	h.send("r")
	if h.m.rv != nil {
		t.Fatalf("r opened a run view on an item with no run:\n%s", h.screen())
	}
	if out := h.screen(); !strings.Contains(out, "02 later has not run yet") {
		t.Errorf("the dashboard should say the item has not run:\n%s", out)
	}
}

// The dashboard's tick reloads a live run: the hook in the tick handler, not
// only pollRun itself.
func TestRunViewTickPollsALiveRun(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "go.sh", "echo hi\n", nil, -1)
	h.selectItem(name)
	h.send("r")
	if h.m.rv.loading {
		t.Fatal("the first load did not finish")
	}
	out, _ := h.m.Update(deskTickMsg{})
	h.m = out.(deskModel)
	if !h.m.rv.loading {
		t.Error("the dashboard tick did not start a load of the live run")
	}
}

// The key hints keep "q close" at every width.
func TestRunHintsKeepQClose(t *testing.T) {
	for _, finished := range []bool{false, true} {
		for _, w := range []int{80, 60, 40, 20, 10} {
			if h := runHintsFor(w, finished); !strings.HasSuffix(h, "q close") || ansi.StringWidth(h) > max(w, 8) {
				t.Errorf("width %d finished %v: hints %q", w, finished, h)
			}
		}
	}
}

// A view opened before any run exists lists again on the tick, so a run
// that starts later shows without reopening.
func TestRunViewRetriesAnEmptyListing(t *testing.T) {
	h := runHarness(t)
	h.send("r")
	retry := h.m.pollRun()
	if retry == nil {
		t.Fatal("a view with no runs yet is not retried")
	}
	name, _ := stageRun(t, h, "late.sh", "echo late\n", nil, 0) // the run starts before the retry lists
	h.drive(retry)
	if !h.m.rv.loaded || h.m.rv.ref().Name != name {
		t.Errorf("the retry did not pick up the run that started: loaded %v ref %q", h.m.rv.loaded, h.m.rv.ref().Name)
	}
}

// After n or p, a failed first load retries the run shown, not the newest.
func TestRunViewRetriesTheRunShownAfterASwitch(t *testing.T) {
	h := runHarness(t)
	first, _ := stageRun(t, h, "one.sh", "echo one\n", nil, 0)
	second, _ := stageRun(t, h, "two.sh", "echo two\n", nil, 0)
	h.selectItem(second)
	h.send("r")
	out, _ := h.m.Update(key("n")) // switch to first; its load is not run
	h.m = out.(deskModel)
	v := h.m.rv
	out, _ = h.m.Update(deskRunLoadMsg{gen: v.gen, ref: v.ref(), err: errors.New("disk hiccup")})
	h.m = out.(deskModel)
	h.drive(h.m.pollRun())
	if got := h.m.rv.ref().Name; got != first || !h.m.rv.loaded {
		t.Errorf("the retry showed %q (loaded %v), want %s", got, h.m.rv.loaded, first)
	}
}

// A lost run's step reads interrupted, not running, and a changed item's run
// reads changed (the queue's word) with a reason for having no events
// (#1106).
func TestRunViewLostAndChanged(t *testing.T) {
	st := theme.Default().Styles()
	ref := runview.RunRef{Source: "desk", Name: "01-long", Kind: runview.KindDesk}
	defs := []runview.StepDef{{ID: runview.ScriptStep}}
	events := []runview.Event{{Name: "RUN-START", Seq: 1}, {Name: "STEP-START", Step: runview.ScriptStep, Seq: 2}, {Name: "RUN-LOST", Seq: 3}}
	f := newRunFolder(ref, defs, events)
	lost := &deskRunView{refs: []runview.RunRef{ref}, folder: f, defs: defs, delta: runview.Delta{Live: runview.LiveLost}, loaded: true, follow: true, at: f.Len()}
	out := ansi.Strip(lost.render(st, 80, 20))
	if !strings.Contains(out, "? lost") || !strings.Contains(out, "⊘") || strings.Contains(out, "◐") {
		t.Errorf("lost run view:\n%s", out)
	}

	cref := runview.RunRef{Source: "desk", Name: "02-s2", Kind: runview.KindDesk}
	cf := newRunFolder(cref, defs, nil)
	changed := &deskRunView{refs: []runview.RunRef{cref}, folder: cf, defs: defs, delta: runview.Delta{Live: runview.LiveChanged}, loaded: true, follow: true}
	out = ansi.Strip(changed.render(st, 80, 20))
	if !strings.Contains(out, "! changed") || !strings.Contains(out, "never ran: it changed after it was queued") || strings.Contains(out, "skipped") {
		t.Errorf("changed run view:\n%s", out)
	}
}

// A run the desk no longer has is not "finished": one that vanished while
// running keeps the live replay wording.
func TestRunViewGoneRunIsNotFinished(t *testing.T) {
	st := theme.Default().Styles()
	ref := runview.RunRef{Source: "desk", Name: "01-a", Kind: runview.KindDesk}
	defs := []runview.StepDef{{ID: runview.ScriptStep}}
	f := newRunFolder(ref, defs, []runview.Event{{Name: "RUN-START", Seq: 1}, {Name: "STEP-START", Step: runview.ScriptStep, Seq: 2}})
	v := &deskRunView{refs: []runview.RunRef{ref}, folder: f, defs: defs, delta: runview.Delta{Live: runview.LiveRunning}, loaded: true, gone: true, at: 1}
	out := ansi.Strip(v.render(st, 80, 20))
	if strings.Contains(out, "G end") || strings.Contains(out, "start/end") || !strings.Contains(out, "replay 1/2 · G live") {
		t.Errorf("a vanished running run reads finished:\n%s", out)
	}
}

// A load that cannot find the selected item's run closes the view and says
// so; it never shows another item's run as this one's.
func TestRunViewSelectedRunGoneSaysSo(t *testing.T) {
	h := runHarness(t)
	name, _ := stageRun(t, h, "a.sh", "echo a\n", nil, 0)
	h.selectItem(name)
	h.m.runGen++
	h.m.rv = &deskRunView{gen: h.m.runGen, want: "09-gone", follow: true, loading: true}
	other := runview.RunRef{Source: "desk", Name: name, Kind: runview.KindDesk}
	out, _ := h.m.Update(deskRunLoadMsg{gen: h.m.runGen, refs: []runview.RunRef{other}, ref: other, cur: &runview.Cursor{}})
	h.m = out.(deskModel)
	if h.m.rv != nil {
		t.Fatalf("the view opened %s for a selected item with no run", name)
	}
	if !strings.Contains(ansi.Strip(h.m.footer()), "09 gone has no run to show") {
		t.Errorf("footer = %q", ansi.Strip(h.m.footer()))
	}
}
