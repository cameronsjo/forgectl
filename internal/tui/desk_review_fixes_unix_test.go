// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
)

// TestTheOpeningViewIsChosenOnce: a desk with only waiting items opens on
// the queue; a view the operator picks then survives every reload, and so
// does it when the first run appears.
func TestTheOpeningViewIsChosenOnce(t *testing.T) {
	src := &fakeDeskSource{name: "desk", spec: deskSpec(), items: []runview.QueueItem{{Name: "0001-a", Kind: "script", State: "waiting"}}}
	m := openDesk(t, optsFor(src), 60, 30)
	if m.view != viewQueue {
		t.Fatalf("opened on view %d, want the queue", m.view)
	}
	m = pressDesk(t, m, key("1"))
	m = relistDesk(t, m)
	if m.view != viewFlow {
		t.Fatalf("a reload moved the view to %d", m.view)
	}
	src.mu.Lock()
	src.refs = []runview.RunRef{{Source: "desk", Name: "run-1", Kind: runview.KindLog, Updated: deskNow}}
	src.mu.Unlock()
	m = relistDesk(t, m)
	if m.view != viewFlow || !m.hasSel || m.selected.name != "run-1" {
		t.Fatalf("after the first run: view %d, selected %v %v", m.view, m.hasSel, m.selected)
	}
}

// TestAFailedQueueReadKeepsTheQueueAndTheDialog: one scan that fails leaves
// the items as last listed and the approve dialog open.
func TestAFailedQueueReadKeepsTheQueueAndTheDialog(t *testing.T) {
	item := runview.QueueItem{Name: "0001-a", Kind: "script", State: "waiting", SHA256: strings.Repeat("e", 64), Body: []string{"true"}, Marks: []runview.LineMarks{{}}}
	src := &fakeDeskSource{name: "desk", items: []runview.QueueItem{item}}
	m := openDesk(t, optsFor(src), 120, 30)
	m = pressDesk(t, m, key("a"))
	if !confirmOpen(m) {
		t.Fatal("a did not open the dialog")
	}
	src.mu.Lock()
	src.queueErr = errors.New("meta unreadable mid-move")
	src.mu.Unlock()
	m = relistDesk(t, m)
	if !confirmOpen(m) {
		t.Fatalf("a failed scan closed the dialog:\n%s", screen(t, m))
	}
	if len(m.items) != 1 {
		t.Fatalf("a failed scan left %d items", len(m.items))
	}
	if strings.Contains(screen(t, m), "re-review") {
		t.Error("a failed scan says the item changed")
	}
}

// TestADeskRunThatGoesLostReloads: a desk run the operator is not looking
// at reloads when the listing's state for it moves, though its Updated time
// does not.
func TestADeskRunThatGoesLostReloads(t *testing.T) {
	ref := func(name string, live runview.LiveState) runview.RunRef {
		return runview.RunRef{Source: "desk", Name: name, Kind: runview.KindDesk, Updated: deskNow, Live: live}
	}
	src := &fakeDeskSource{name: "desk", refs: []runview.RunRef{ref("01-sel", runview.LiveEnded), ref("02-bg", runview.LiveRunning)},
		deltas: map[string][]runview.Delta{
			"02-bg": {{Live: runview.LiveRunning}, {Live: runview.LiveLost}},
		}}
	m := openDesk(t, optsFor(src), 160, 50)
	if m.selected.name != "01-sel" && m.selected.name != "02-bg" {
		t.Fatalf("selected %v", m.selected)
	}
	if m.selected.name == "02-bg" {
		m = pressDesk(t, m, key("]"))
	}
	before := src.loadCount("02-bg")
	m = relistDesk(t, m)
	if src.loadCount("02-bg") != before {
		t.Fatal("an unchanged run reloaded")
	}
	src.mu.Lock()
	src.refs[1] = ref("02-bg", runview.LiveLost)
	src.mu.Unlock()
	m = relistDesk(t, m)
	if src.loadCount("02-bg") != before+1 {
		t.Fatal("a run whose listed state moved did not reload")
	}
	if e := m.runs[deskRunKey{"desk", "02-bg"}]; e.live != runview.LiveLost {
		t.Fatalf("live = %q, want lost", e.live)
	}
}

// TestAClaimedSkipOffersNoUndo: an operator skip of an item that had been
// claimed cannot be undone (Unskip refuses it), so neither the reason line
// nor u offers it, and the desk is never asked.
func TestAClaimedSkipOffersNoUndo(t *testing.T) {
	src := &fakeDeskSource{name: "desk", items: []runview.QueueItem{{Name: "0006-sweep", Kind: "batch", State: "skipped", SkipReason: desk.SkipOperator, Claimed: true}}}
	m := openDesk(t, optsFor(src), 120, 30)
	if m.sideShows() != viewQueue {
		m = pressDesk(t, m, key("6"))
	}
	verbs := &skipOnlyVerbs{}
	m.verbs = verbs
	if out := screen(t, m); strings.Contains(out, "u to undo") {
		t.Fatalf("a claimed skip offers u:\n%s", out)
	}
	m = pressDesk(t, m, key("u"))
	mustContain(t, "footer", screen(t, m), "claimed before it was skipped")
}

// TestAClickPicksTheBoxUnderIt: in the full flow form a wave's boxes share
// lines, so the column decides which step a click selects, and a second click
// on that box opens it.
func TestAClickPicksTheBoxUnderIt(t *testing.T) {
	src := &fakeDeskSource{name: "desk", refs: []runview.RunRef{{Source: "desk", Name: "01-wave", Kind: runview.KindDesk, Updated: deskNow}},
		deltas: map[string][]runview.Delta{"01-wave": {{
			Defs: []runview.StepDef{{ID: "root"}, {ID: "left-step", After: []string{"root"}}, {ID: "right-step", After: []string{"root"}}},
			Live: runview.LiveRunning,
		}}}}
	m := openDesk(t, optsFor(src), 160, 50)
	lines := strings.Split(screen(t, m), "\n")
	x, y := -1, -1
	for i, l := range lines {
		if c := strings.Index(l, "right-step"); c >= 0 && strings.Contains(l, "left-step") {
			x, y = len([]rune(l[:c])), i
			break
		}
	}
	if y < 0 {
		t.Fatalf("no line shows both boxes:\n%s", strings.Join(lines, "\n"))
	}
	m = send(t, m, click(x, y))
	if m.cur[viewFlow] != 2 {
		t.Fatalf("a click on the right box selected step %d, want 2", m.cur[viewFlow])
	}
	m = send(t, m, click(x, y))
	if md := m.topModal(); md == nil || md.kind != modalStep || md.step != "right-step" {
		t.Fatalf("a second click opened %+v, want the right step", md)
	}
}

// TestTheTabStripShowsEveryNameWhenItFits: the full strip with the shown
// view's brackets is 56 columns; at 56 it is drawn whole.
func TestTheTabStripShowsEveryNameWhenItFits(t *testing.T) {
	m := openDesk(t, optsFor(oneSource(nil, nil)), 56, 20)
	if segs := m.tabSegments(); segs[1].text != "2 events" {
		t.Fatalf("at 56 columns the strip is %+v", segs)
	}
	m = openDesk(t, optsFor(oneSource(nil, nil)), 55, 20)
	if segs := m.tabSegments(); segs[1].text != "2" {
		t.Fatalf("at 55 columns the strip is %+v", segs)
	}
}

// TestAResetEndsTheReplayOfThatRun: the replay counted the replaced log's
// events, so a reset of the replayed run ends it.
func TestAResetEndsTheReplayOfThatRun(t *testing.T) {
	src := oneSource([]runview.RunRef{runRef("r", deskNow)}, map[string][]runview.Delta{
		"r": {{Events: []runview.Event{{Seq: 1, Name: "stage_started", Step: "fetch"}}}, {Reset: true}},
	})
	m := openDesk(t, optsFor(src), 120, 30)
	m = pressDesk(t, m, key("p"))
	if m.replay == nil {
		t.Fatal("p did not start a replay")
	}
	m = pressDesk(t, m, key("r"))
	if m.replay != nil {
		t.Fatal("the replay outlived a reset of its run")
	}
}

// TestTwoFailedSourcesArePlural.
func TestTwoFailedSourcesArePlural(t *testing.T) {
	opts := optsFor(oneSource(nil, nil))
	opts.Failed = []DeskSourceFailure{{Name: "a", Err: errors.New("x")}, {Name: "b", Err: errors.New("y")}}
	mustContain(t, "header", screen(t, openDesk(t, opts, 120, 30)), "2 sources failed")
}
