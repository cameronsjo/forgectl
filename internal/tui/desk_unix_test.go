// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func clockAt(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }

// relistDesk runs a list of every source and returns the merged model.
func relistDesk(t *testing.T, m deskModel) deskModel {
	t.Helper()
	m.listBusy = true
	return pumpDesk(t, m, m.listCmd())
}

// screen is the model's view with colour removed.
func screen(t *testing.T, m deskModel) string {
	t.Helper()
	return plainDesk(t, m.View().Content)
}

func mustContain(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("%s: missing %q\n%s", what, w, out)
		}
	}
}

func oneSource(refs []runview.RunRef, deltas map[string][]runview.Delta) *fakeDeskSource {
	return &fakeDeskSource{name: "s", spec: deskSpec(), refs: refs, deltas: deltas}
}

func runRef(name string, updated time.Time) runview.RunRef {
	return runview.RunRef{Source: "s", Name: name, Kind: runview.KindLog, Updated: updated}
}

func optsFor(srcs ...runview.Source) DeskOptions {
	return DeskOptions{Sources: srcs, Theme: theme.Default()}
}

// --- selection and loading ---

func TestSelectionFollowsTheRunNotTheRow(t *testing.T) {
	src := oneSource([]runview.RunRef{runRef("mid", clockAt(11, 0)), runRef("old", clockAt(10, 0))}, nil)
	m := openDesk(t, optsFor(src), 120, 30)
	m = pressDesk(t, m, key("]"))
	want := deskRunKey{"s", "old"}
	if m.selected != want {
		t.Fatalf("selected %v before the reload, want %v", m.selected, want)
	}
	rowBefore := slices.Index(m.order, m.selected)

	// A newer run appears above both: every row index shifts down by one.
	src.mu.Lock()
	src.refs = append([]runview.RunRef{runRef("newest", clockAt(12, 0))}, src.refs...)
	src.mu.Unlock()
	m = relistDesk(t, m)

	if m.order[0].name != "newest" {
		t.Fatalf("order = %v, want newest first", m.order)
	}
	if m.selected != want {
		t.Errorf("selected %v after the reload, want %v: selection followed the row", m.selected, want)
	}
	if rowAfter := slices.Index(m.order, m.selected); rowAfter == rowBefore {
		t.Fatalf("the run stayed on row %d, so this test cannot tell identity from index", rowAfter)
	}
}

func TestATickNeverStartsASecondLoadOfTheSameRun(t *testing.T) {
	src := oneSource([]runview.RunRef{runRef("only", clockAt(11, 0))}, nil)
	m := openDesk(t, optsFor(src), 120, 30)
	before := src.loadCount("only")

	first, cmd1 := m.Update(deskTickMsg{})
	second, cmd2 := first.(deskModel).Update(deskTickMsg{})
	pumpDesk(t, second.(deskModel), cmd2)
	if got := src.loadCount("only"); got != before {
		t.Fatalf("a tick while a load was running started another: %d loads, want %d", got, before)
	}
	pumpDesk(t, second.(deskModel), cmd1)
	if got := src.loadCount("only"); got != before+1 {
		t.Errorf("%d loads after the first tick's load ran, want %d", got, before+1)
	}
}

func TestTheFoldRunsOnlyWhenSomethingChanged(t *testing.T) {
	src := oneSource([]runview.RunRef{runRef("r", clockAt(11, 55))}, map[string][]runview.Delta{
		"r": {{Events: finishedRun("11:55", "0")[:3]}},
	})
	m := openDesk(t, optsFor(src), 120, 30)
	e := m.sel()
	folder, state := e.folder, e.state
	if len(state.Steps) == 0 || state.Steps[0].Status != runview.StepClosed {
		t.Fatalf("the first delta was not folded: %+v", state.Steps)
	}

	m.apply(e, runview.Delta{})
	if e.folder != folder || !sameRunState(e.state, state) {
		t.Error("an empty delta changed the folder or the state")
	}
	m.apply(e, runview.Delta{Events: finishedRun("11:55", "0")[3:5]})
	if e.state.Steps[1].Status != runview.StepClosed {
		t.Errorf("new events were not folded: %+v", e.state.Steps[1])
	}
	m.apply(e, runview.Delta{Reset: true, Events: finishedRun("11:55", "0")[:2]})
	if e.folder == folder {
		t.Error("a reset kept the old folder")
	}
	if len(e.events) != 2 {
		t.Errorf("a reset kept %d events, want the 2 it carried", len(e.events))
	}
	mustContain(t, "after a reset", screen(t, m), "log restarted")
}

func sameRunState(a, b runview.RunState) bool {
	return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b)
}

func TestTheNewestLiveRunIsSelectedFirstAndLiveRunsSortFirst(t *testing.T) {
	m := openDesk(t, optsFor(func() runview.Source { l, _ := deskFixture(); return l }()), 120, 30)
	want := []string{"alpha-run-7", "beta-run-7", "gamma-run-7", "nightly-b3"}
	var got []string
	for _, k := range m.order {
		got = append(got, k.name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want live first then newest first %v", got, want)
	}
	if m.selected.name != "alpha-run-7" {
		t.Errorf("opened on %q, want the live run", m.selected.name)
	}
	if m.view != viewFlow {
		t.Errorf("opened on view %d with an empty queue, want the flow", m.view)
	}
}

func TestTheQueueOpensFirstWhenItemsWait(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 30)
	if m.view != viewQueue {
		t.Errorf("opened on view %d, want the queue (%d) when items wait", m.view, viewQueue)
	}
	mustContain(t, "header", screen(t, m), "queue: 2 waiting · lost: 1")
}

// --- layout ---

func TestBreakpoints(t *testing.T) {
	cases := []struct {
		w, h int
		want layoutKind
	}{
		{39, 12, layoutSmall}, {40, 11, layoutSmall}, {40, 12, layoutSingle}, {99, 30, layoutSingle},
		{100, 30, layoutMid}, {149, 30, layoutMid}, {150, 30, layoutWide}, {200, 60, layoutWide},
	}
	for _, c := range cases {
		m := openDesk(t, goldenDeskOptions(false), c.w, c.h)
		if got := m.layout().kind; got != c.want {
			t.Errorf("%dx%d: layout %d, want %d", c.w, c.h, got, c.want)
		}
	}
}

func TestEveryScreenIsExactlyTheWindowSize(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {39, 12}, {40, 11}, {60, 30}, {99, 20}, {100, 30}, {149, 40}, {150, 30}, {160, 50}, {200, 12}} {
		m := openDesk(t, goldenDeskOptions(false), sz[0], sz[1])
		for _, v := range []string{"1", "2", "3", "4", "5", "6", "?"} {
			m = pressDesk(t, m, key(v))
			lines := strings.Split(m.View().Content, "\n")
			if len(lines) != sz[1] {
				t.Errorf("%dx%d key %s: %d lines, want %d", sz[0], sz[1], v, len(lines), sz[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > sz[0] {
					t.Errorf("%dx%d key %s line %d is %d wide", sz[0], sz[1], v, i, w)
				}
			}
			if v == "?" {
				m = pressDesk(t, m, key("?"))
			}
		}
	}
}

func TestTooSmallSaysSo(t *testing.T) {
	const want = "terminal too small (needs 40×12) — resize or press q"
	for _, sz := range [][2]int{{39, 12}, {40, 11}, {20, 5}, {80, 8}} {
		m := openDesk(t, goldenDeskOptions(false), sz[0], sz[1])
		got := strings.Join(strings.Fields(screen(t, m)), " ")
		if got != want {
			t.Errorf("%dx%d: %q, want %q", sz[0], sz[1], got, want)
		}
	}
	m := openDesk(t, goldenDeskOptions(false), 40, 12)
	if strings.Contains(screen(t, m), "terminal too small") {
		t.Error("the floor size 40x12 reads as too small")
	}
}

func TestFlowFormFollowsTheHeightFormula(t *testing.T) {
	// Four steps in a chain: full needs 3 + 6*4 + 7 = 34 rows of pane.
	for _, c := range []struct {
		h    int
		full bool
	}{{30, false}, {36, false}, {39, true}, {50, true}} {
		m := openDesk(t, goldenDeskOptions(false), 120, c.h)
		m = pressDesk(t, m, key("1"))
		if hasBoxes := strings.Contains(screen(t, m), "╭ ✓ fetch"); hasBoxes != c.full {
			t.Errorf("height %d: step boxes drawn = %v, want %v", c.h, hasBoxes, c.full)
		}
	}
}

func TestFlowFollowsTheRunningStep(t *testing.T) {
	// The last step is running in a pane too short for all four: it must be
	// on screen.
	src := oneSource([]runview.RunRef{runRef("r", clockAt(11, 59))}, map[string][]runview.Delta{
		"r": {{Events: finishedRun("11:55", "0")[:8]}},
	})
	m := openDesk(t, optsFor(src), 120, 12)
	m = pressDesk(t, m, key("1"))
	mustContain(t, "compact flow at 12 rows", screen(t, m), "ship", "publish the result")
	if m.top[viewFlow] == 0 {
		t.Error("the flow did not scroll to the running step")
	}
	// Moving up pauses the follow; G resumes it.
	m = pressDesk(t, m, key("k"))
	if m.follow[viewFlow] {
		t.Error("k left the flow following")
	}
	m = pressDesk(t, m, key("G"))
	if !m.follow[viewFlow] {
		t.Error("G did not resume following")
	}
}

// --- states the operator can see ---

func TestLoadingBeforeTheFirstList(t *testing.T) {
	m := deskTestModel(t, goldenDeskOptions(false))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	mustContain(t, "before the list", screen(t, next.(deskModel)), "loading runs…")
}

func TestNoRunsHasACauseSpecificLine(t *testing.T) {
	t.Run("no sources", func(t *testing.T) {
		mustContain(t, "no sources", screen(t, openDesk(t, optsFor(), 120, 30)), "no sources: pass --spec")
	})
	t.Run("root unset", func(t *testing.T) {
		src := &fakeDeskSource{name: "s", listErr: fmt.Errorf("%w: $PIPELINE_ROOT is unset", runview.ErrRoot)}
		out := screen(t, openDesk(t, optsFor(src), 120, 30))
		mustContain(t, "root unset", out, "$PIPELINE_ROOT is unset", "1 source failed")
		if strings.Contains(out, "run root:") {
			t.Errorf("the error's own prefix is shown\n%s", out)
		}
	})
	t.Run("no match", func(t *testing.T) {
		mustContain(t, "no match", screen(t, openDesk(t, optsFor(oneSource(nil, nil)), 120, 30)), "no runs match runs/*")
	})
	t.Run("queue empty", func(t *testing.T) {
		m := openDesk(t, optsFor(oneSource(nil, nil)), 120, 30)
		mustContain(t, "queue", screen(t, pressDesk(t, m, key("6"))), "queue empty")
	})
}

func TestBadSpecShowsAChipAndTheDetailInHelp(t *testing.T) {
	opts := goldenDeskOptions(false)
	opts.Failed = []DeskSourceFailure{{Name: "second", Err: errors.New(`run spec "x.toml": steps[1].after: unknown step "zzz"`)}}
	m := openDesk(t, opts, 120, 30)
	mustContain(t, "header", screen(t, m), "1 source failed")
	help := screen(t, pressDesk(t, m, key("?")))
	mustContain(t, "help", help, `run spec "x.toml": steps[1].after: unknown step "zzz"`, "legend", "rej N/M", "skipped")
	if !strings.Contains(screen(t, m), "alpha") {
		t.Error("the other sources stopped loading")
	}
}

func TestLogStates(t *testing.T) {
	noLog := fmt.Errorf("open out/events.jsonl: %w", fs.ErrNotExist)
	cases := []struct {
		name  string
		delta runview.Delta
		want  []string
		not   []string
	}{
		{"no log yet", runview.Delta{Err: noLog}, []string{"no log yet"}, []string{"cannot read"}},
		{"unreadable", runview.Delta{Err: errors.New("permission denied")}, []string{"cannot read out/events.jsonl: permission denied"}, nil},
		{"partial", runview.Delta{Events: finishedRun("11:55", "0")[:2], Partial: true}, []string{"partial: over 32 MiB cap"}, nil},
		{"dropped", runview.Delta{
			Events: append(finishedRun("11:55", "0")[:2],
				ev(3, "11:55:05", "stage_started", "nope"), ev(4, "11:55:06", "stage_started", "nope"),
				ev(5, "11:55:07", "run_finished", "", "rc", "x")),
			Dropped: 3,
		}, []string{"3 lines dropped · 2 unknown steps · 1 bad exit"}, nil},
		{"a vanished summary is not a missing log", runview.Delta{Events: finishedRun("11:55", "0")[:2], Err: noLog}, nil, []string{"no log yet", "cannot read"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := oneSource([]runview.RunRef{runRef("r", clockAt(11, 55))}, map[string][]runview.Delta{"r": {c.delta}})
			m := openDesk(t, optsFor(src), 120, 30)
			out := screen(t, m)
			mustContain(t, c.name, out, c.want...)
			for _, n := range c.not {
				if strings.Contains(out, n) {
					t.Errorf("%s: unexpected %q\n%s", c.name, n, out)
				}
			}
		})
	}
	t.Run("no log yet also in events", func(t *testing.T) {
		src := oneSource([]runview.RunRef{runRef("r", clockAt(11, 55))}, map[string][]runview.Delta{"r": {{Err: noLog}}})
		m := pressDesk(t, openDesk(t, optsFor(src), 60, 30), key("2"))
		mustContain(t, "events", screen(t, m), "no log yet")
	})
}

func TestStaleLostAndGone(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 30)
	m = pressDesk(t, m, key("1"))

	m.selectKey(deskRunKey{"pipeline", "nightly-b3"})
	mustContain(t, "stale", screen(t, m), "stale: no events for 2h (limit 10m)")

	m.selectKey(deskRunKey{"desk", "batch-04"})
	mustContain(t, "lost", screen(t, m), "lost: supervisor died — s to skip")

	// A run the list stops showing keeps its last state, labeled.
	logSrc := m.runs[deskRunKey{"pipeline", "gamma-run-7"}].src.(*fakeDeskSource)
	logSrc.mu.Lock()
	logSrc.refs = slices.DeleteFunc(slices.Clone(logSrc.refs), func(r runview.RunRef) bool { return r.Name == "gamma-run-7" })
	logSrc.mu.Unlock()
	m = relistDesk(t, m)
	m.selectKey(deskRunKey{"pipeline", "gamma-run-7"})
	out := screen(t, m)
	mustContain(t, "gone", out, "gone", "exit 3", "audit rejected")
}

func TestRunRowsCarryAGlyphAndAWord(t *testing.T) {
	out := screen(t, pressDesk(t, openDesk(t, goldenDeskOptions(false), 160, 30), key("1")))
	for _, want := range []string{"◐ live", "✓ ok", "✗ 3", "! stale", "? lost"} {
		mustContain(t, "run list", out, want)
	}
	ascii := screen(t, pressDesk(t, openDesk(t, goldenDeskOptions(true), 160, 30), key("1")))
	for _, want := range []string{"* live", "+ ok", "x 3", "! stale", "? lost"} {
		mustContain(t, "ascii run list", ascii, want)
	}
}

func TestEventsHaveTagsAndOnlyTheShownFields(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	m = pressDesk(t, m, key("2"))
	out := screen(t, m)
	for _, tag := range []string{"start", "ok", "rej", "rst"} {
		mustContain(t, "events", out, " "+tag+" ")
	}
	mustContain(t, "events", out, "err=lint", "rc=1")
	if strings.Contains(out, "note=") {
		t.Errorf("a field outside [log] show is drawn\n%s", out)
	}
	// A run that failed, ended and was checked has the other tags.
	m.selectKey(deskRunKey{"pipeline", "gamma-run-7"})
	out = screen(t, m)
	mustContain(t, "events", out, " end ", " chk ")
	m.selectKey(deskRunKey{"pipeline", "alpha-run-7"})
	mustContain(t, "unmapped", screen(t, m), " · ")
}

func TestPausedFollowCountsNewEvents(t *testing.T) {
	src := oneSource([]runview.RunRef{runRef("r", clockAt(11, 59))}, map[string][]runview.Delta{
		"r": {{Events: finishedRun("11:55", "0")[:6]}, {Events: finishedRun("11:55", "0")[6:7]}},
	})
	m := pressDesk(t, openDesk(t, optsFor(src), 120, 30), key("2"))
	m = pressDesk(t, m, key("k"))
	mustContain(t, "paused", screen(t, m), "paused · 0 new · G to follow")
	// The note names the count; deliver one more event.
	next, cmd := m.Update(deskTickMsg{})
	m = pumpDesk(t, next.(deskModel), cmd)
	mustContain(t, "paused", screen(t, m), "paused · 1 new · G to follow")
	m = pressDesk(t, m, key("G"))
	if strings.Contains(screen(t, m), "paused") {
		t.Error("G left the follow paused")
	}
}

func TestSummaryIsATree(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	m.selectKey(deskRunKey{"pipeline", "beta-run-7"})
	m = pressDesk(t, m, key("5"))
	out := screen(t, m)
	mustContain(t, "summary", out, "result:", "  status: accepted", "timing:", "  build:", "    secs: 19", "version: 2")
	m.selectKey(deskRunKey{"pipeline", "gamma-run-7"})
	mustContain(t, "no summary", screen(t, m), "no summary yet")
}

func TestMetricsShowTotalsAndErrors(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	out := screen(t, pressDesk(t, m, key("4")))
	mustContain(t, "metrics", out, "5 lines", "1 errors", "in", "total 1260", "out", "total 490")
	if !strings.ContainsAny(out, "▁▂▃▄▅▆▇█") {
		t.Error("no sparkline")
	}
}

func TestTimelineMarksRestarts(t *testing.T) {
	src := oneSource([]runview.RunRef{runRef("r", clockAt(11, 55))}, map[string][]runview.Delta{"r": {{Events: numberEvents([]runview.Event{
		ev(0, "11:50:00", "worker_boot", ""),
		ev(0, "11:50:01", "stage_started", "fetch"),
		ev(0, "11:50:30", "stage_done", "fetch"),
		ev(0, "11:51:00", "worker_boot", ""),
		ev(0, "11:51:10", "stage_started", "build"),
	})}}})
	out := screen(t, pressDesk(t, openDesk(t, optsFor(src), 100, 30), key("3")))
	mustContain(t, "timeline", out, "⟲ restart at 11:51:00", "fetch", "build")
	if !strings.ContainsAny(out, "█▓░") {
		t.Error("no bar")
	}
}

func TestQueueRowsShowTheirReason(t *testing.T) {
	m := pressDesk(t, openDesk(t, goldenDeskOptions(false), 160, 30), key("6"))
	mustContain(t, "queue", screen(t, m),
		"waiting (2)", "why: nightly refresh", "cannot approve: recorded hash does not match the content",
		"! stale", "lost (1)", "skipped: superseded by 0007")
}

// --- keys and mouse ---

func TestKeys(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	m = pressDesk(t, m, key("1"))

	if v := pressDesk(t, m, key("3")).view; v != viewTimeline {
		t.Errorf("3 picked view %d", v)
	}
	// tab moves pane focus and never the view.
	view := m.view
	if f := pressDesk(t, m, namedKey(tea.KeyTab)).focus; f == m.focus {
		t.Error("tab did not move the focus")
	}
	if pressDesk(t, m, namedKey(tea.KeyTab)).view != view {
		t.Error("tab changed the view")
	}
	// [ and ] move between runs, < and > too.
	next := pressDesk(t, m, key("]"))
	if next.selected == m.selected {
		t.Error("] did not select the next run")
	}
	if back := pressDesk(t, next, key("[")); back.selected != m.selected {
		t.Error("[ did not come back")
	}
	if got := pressDesk(t, m, key(">")).selected; got != next.selected {
		t.Error("> is not the same as ]")
	}
	if got := pressDesk(t, next, key("<")).selected; got != m.selected {
		t.Error("< is not the same as [")
	}
	// a, s and u point at the queue outside it. Inside it, none of these keys
	// selects another run or hands the caller an action by itself.
	mustContain(t, "approve outside the queue", screen(t, pressDesk(t, m, key("a"))), "approve is in the queue (6)")
	q := pressDesk(t, m, key("6"))
	for _, k := range []string{"a", "s", "u", "o", "p"} {
		before := q.selected
		q2 := pressDesk(t, q, key(k))
		if q2.selected != before || q2.action != nil {
			t.Errorf("%s changed the selection or set an action", k)
		}
	}
	// q and ctrl+c quit.
	if _, cmd := m.Update(key("q")); cmd == nil || cmd() != tea.Quit() {
		t.Error("q does not quit")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil || cmd() != tea.Quit() {
		t.Error("ctrl+c does not quit")
	}
}

func TestEscClosesOneLevel(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	m = pressDesk(t, m, key("/"))
	for _, k := range []string{"g", "a", "m"} {
		m = pressDesk(t, m, key(k))
	}
	if !m.filtering || string(m.filter) != "gam" {
		t.Fatalf("filtering=%v filter=%q", m.filtering, string(m.filter))
	}
	if got := m.visibleOrder(); len(got) != 1 || got[0].name != "gamma-run-7" {
		t.Errorf("the run list is not filtered: %v", got)
	}
	if got := screen(t, m); strings.Contains(got, "beta") || strings.Contains(got, "y-b3") {
		t.Errorf("the drawn run list is not filtered\n%s", got)
	}
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	if m.filtering || len(m.filter) != 3 {
		t.Errorf("enter should keep the filter and stop typing: filtering=%v filter=%q", m.filtering, string(m.filter))
	}
	m = pressDesk(t, m, namedKey(tea.KeyEscape))
	if len(m.filter) != 0 {
		t.Error("esc did not clear the filter")
	}
	h := pressDesk(t, m, key("?"))
	if !h.showHelp {
		t.Fatal("? did not open help")
	}
	if pressDesk(t, h, namedKey(tea.KeyEscape)).showHelp {
		t.Error("esc did not close help")
	}
}

func TestEventFilterInTheSinglePane(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 60, 30)
	m = pressDesk(t, m, key("1"))
	m = pressDesk(t, m, key("2"))
	for _, k := range []string{"/", "r", "e", "f"} {
		m = pressDesk(t, m, key(k))
	}
	out := screen(t, m)
	mustContain(t, "filtered events", out, "stage_refused")
	if strings.Contains(out, "stage_started") {
		t.Errorf("the events filter kept a non-matching event\n%s", out)
	}
}

func TestRefreshKeepsAFifteenSecondGap(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 30)
	m = pressDesk(t, m, key("1"))
	m = pressDesk(t, m, key("r"))
	mustContain(t, "first r", screen(t, m), "refreshing alpha-run-7…")
	m = pressDesk(t, m, key("r"))
	mustContain(t, "second r", screen(t, m), "refreshed moments ago; try again in 15s")
	now := deskNow.Add(16 * time.Second)
	m.now = func() time.Time { return now }
	m = pressDesk(t, m, key("r"))
	mustContain(t, "third r", screen(t, m), "refreshing alpha-run-7…")

	m = pressDesk(t, m, key("R"))
	mustContain(t, "R", screen(t, m), "refreshing everything…")
	m = pressDesk(t, m, key("R"))
	mustContain(t, "second R", screen(t, m), "refreshed moments ago")
}

func TestMouseCaptureToggles(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 30)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want cell motion", got)
	}
	m = pressDesk(t, m, key("m"))
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("after m, mouse mode = %v, want none", got)
	}
	if !m.viewState().MouseOff {
		t.Error("the view state forgot the mouse is off")
	}
	m = pressDesk(t, m, key("m"))
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("after m twice, mouse mode = %v, want cell motion", got)
	}
}

func TestClickSelectsAndTheWheelScrolls(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	m = pressDesk(t, m, key("1"))
	// The run list starts on screen row 2 (header, then the pane border).
	next, _ := m.Update(tea.MouseClickMsg{X: 3, Y: 3, Button: tea.MouseLeft})
	got := next.(deskModel)
	if want := m.order[1]; got.selected != want {
		t.Errorf("a click on the second list row selected %v, want %v", got.selected, want)
	}

	// The wheel scrolls the pane under it and pauses the events follow.
	s := openDesk(t, goldenDeskOptions(false), 40, 12)
	s = pressDesk(t, s, key("1"))
	s = pressDesk(t, s, key("2"))
	top := s.top[viewEvents]
	if top == 0 {
		t.Fatal("the events view did not follow to the end; the wheel test needs a scrolled pane")
	}
	up, _ := s.Update(tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelUp})
	if us := up.(deskModel); us.top[viewEvents] >= top || us.follow[viewEvents] {
		t.Errorf("wheel up: top %d -> %d, follow %v", top, us.top[viewEvents], us.follow[viewEvents])
	}
}

func TestClickingATabPicksTheView(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 100-1, 30)
	m = pressDesk(t, m, key("1"))
	// "1 flow" is bracketed; "2 events" begins after it.
	next, _ := m.Update(tea.MouseClickMsg{X: 9, Y: 1, Button: tea.MouseLeft})
	if v := next.(deskModel).view; v != viewEvents {
		t.Errorf("a click on the events tab picked view %d", v)
	}
}

func TestViewStateRestores(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 30)
	m = pressDesk(t, m, key("1"))
	m = pressDesk(t, m, key("]"))
	m = pressDesk(t, m, key("3"))
	m = pressDesk(t, m, key("m"))
	st := m.viewState()
	if st.Run == "" || st.View != viewTimeline || !st.MouseOff {
		t.Fatalf("state = %+v", st)
	}

	opts := goldenDeskOptions(false)
	opts.Restore = &st
	r := openDesk(t, opts, 120, 30)
	if r.selected != m.selected || r.view != viewTimeline || !r.mouseOff {
		t.Errorf("restored selected=%v view=%d mouseOff=%v, want %v %d true", r.selected, r.view, r.mouseOff, m.selected, viewTimeline)
	}
}

func TestNoIconsUsesTheAsciiSet(t *testing.T) {
	m := pressDesk(t, openDesk(t, goldenDeskOptions(true), 160, 50), key("1"))
	out := screen(t, m)
	for _, g := range []string{"◐", "◓", "✓", "✗", "↺", "⟲", "–"} {
		if strings.Contains(out, g) {
			t.Errorf("no-icons screen carries %q", g)
		}
	}
	mustContain(t, "ascii", out, "* build", "r rej 2/3")
}

func TestTheRunSpinAdvancesWithTheTick(t *testing.T) {
	m := pressDesk(t, openDesk(t, goldenDeskOptions(false), 160, 50), key("1"))
	a := screen(t, m)
	next, _ := m.Update(deskTickMsg{})
	b := screen(t, next.(deskModel))
	if a == b {
		t.Error("the running mark did not change between frames")
	}
}

// valueSource is a Source that is not comparable with ==, which the
// dashboard must not rely on.
type valueSource struct{ refs []runview.RunRef }

func (valueSource) Name() string { return "v" }
func (v valueSource) List() ([]runview.RunRef, error) {
	return v.refs, nil
}
func (valueSource) Load(runview.RunRef, *runview.Cursor) (runview.Delta, error) {
	return runview.Delta{}, nil
}

func TestSourcesNeedNotBeComparable(t *testing.T) {
	ref := runview.RunRef{Source: "v", Name: "one", Kind: runview.KindLog, Updated: clockAt(11, 0)}
	m := openDesk(t, optsFor(valueSource{refs: []runview.RunRef{ref}}), 120, 30)
	m = relistDesk(t, m)
	if len(m.order) != 1 {
		t.Errorf("order = %v", m.order)
	}
}

// speclessSource hides its wrapped source's Spec method, so the dashboard has
// no spec to fold its runs with.
type speclessSource struct{ f *fakeDeskSource }

func (s speclessSource) Name() string                    { return s.f.Name() }
func (s speclessSource) List() ([]runview.RunRef, error) { return s.f.List() }
func (s speclessSource) Load(r runview.RunRef, c *runview.Cursor) (runview.Delta, error) {
	return s.f.Load(r, c)
}

func TestARunFoldsWithItsSourcesSpec(t *testing.T) {
	// A SpecProvider source: step state and the gate are drawn.
	f, _ := deskFixture()
	out := screen(t, openDesk(t, optsFor(f), 120, 30))
	mustContain(t, "with a spec", out, "rej 2/3", "gate: accepted?")

	// Without the spec the events still show, but no step state is drawn.
	f, _ = deskFixture()
	out = screen(t, openDesk(t, optsFor(speclessSource{f: f}), 120, 30))
	mustContain(t, "without a spec", out, "no steps defined for this run")
	if strings.Contains(out, "rej 2/3") {
		t.Error("the gate text appeared without a spec")
	}

	// The no-runs line names the source's pattern only when it has the spec.
	mustContain(t, "pattern", screen(t, openDesk(t, optsFor(&fakeDeskSource{name: "pipeline", spec: deskSpec()}), 120, 30)),
		"no runs match runs/*")
}
