// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/runview"
)

// The read dialogs, the run switcher, replay, and the key table.

func modalKindOf(m deskModel) modalKind {
	if md := m.topModal(); md != nil {
		return md.kind
	}
	return 0
}

func click(x, y int) tea.MouseClickMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
}

func send(t *testing.T, m deskModel, msg tea.Msg) deskModel {
	t.Helper()
	next, cmd := m.Update(msg)
	return pumpDesk(t, next.(deskModel), cmd)
}

// TestEnterOpensAStepThenAnEventAndEscClosesOneLevel: the step dialog shows
// the step's attempts, rejects and events; enter on an event nests the event
// dialog; esc closes one level at a time.
func TestEnterOpensAStepThenAnEventAndEscClosesOneLevel(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 40)
	m = pressDesk(t, m, key("1"))
	m.follow[viewFlow] = false
	m.cur[viewFlow] = 1 // build: rejected twice, on its third attempt
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	if modalKindOf(m) != modalStep {
		t.Fatalf("enter on a step opened %v", modalKindOf(m))
	}
	mustContain(t, "step dialog", screen(t, m), "step build", "attempt 3", "rejects 2/3", "events (5)", "stage_refused")

	m = pressDesk(t, m, key("j"))
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	if modalKindOf(m) != modalEvent || len(m.modals) != 2 {
		t.Fatalf("enter in the step dialog: %d dialogs, top %v", len(m.modals), modalKindOf(m))
	}
	mustContain(t, "event dialog", screen(t, m), "event stage_refused", "step build", "err: lint", "try: 1")

	m = pressDesk(t, m, namedKey(tea.KeyEscape))
	if modalKindOf(m) != modalStep {
		t.Fatalf("esc closed more than one level: %v", modalKindOf(m))
	}
	m = pressDesk(t, m, key("q")) // q closes a dialog; it does not quit
	if len(m.modals) != 0 {
		t.Fatal("q did not close the step dialog")
	}
}

// TestTheStepDialogFollowsALiveRun: a new event for the step shows up in the
// open dialog.
func TestTheStepDialogFollowsALiveRun(t *testing.T) {
	logSrc, deskSrc := deskFixture()
	m := openDesk(t, optsFor(logSrc, deskSrc), 120, 40)
	m = pressDesk(t, m, key("1"))
	m.follow[viewFlow] = false
	m.cur[viewFlow] = 1
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	logSrc.mu.Lock()
	logSrc.deltas["alpha-run-7"] = []runview.Delta{{Events: []runview.Event{ev(10, "11:59:59", "stage_done", "build", "try", "3")}}}
	logSrc.mu.Unlock()
	m = pumpDesk(t, m, m.loadCmd(m.selLive()))
	mustContain(t, "live step", screen(t, m), "events (6)", "stage_done")
}

// TestAClickSelectsAndASecondClickOpens, in the events pane and inside the
// step dialog; a click outside a read dialog closes it.
func TestAClickSelectsAndASecondClickOpens(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 60, 30)
	m = pressDesk(t, m, key("1"))
	m = pressDesk(t, m, key("2"))
	l := m.layout()
	row := l.y0 + 1 + 2 // the third event row
	m = send(t, m, click(5, row))
	if len(m.modals) != 0 || m.cur[viewEvents] != 2 {
		t.Fatalf("first click: %d dialogs, cursor %d", len(m.modals), m.cur[viewEvents])
	}
	m = send(t, m, click(5, row))
	if modalKindOf(m) != modalEvent {
		t.Fatalf("second click opened %v", modalKindOf(m))
	}
	m = send(t, m, click(0, 0))
	if len(m.modals) != 0 {
		t.Fatal("a click outside did not close the event dialog")
	}

	m = pressDesk(t, m, key("1"))
	m.follow[viewFlow] = false
	m.cur[viewFlow] = 1
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	md := m.topModal()
	bw, _ := m.modalBodySize(md)
	_, rowLine := m.stepLines(md, bw)
	_, y, _, _ := m.modalRect()
	at := y + 1 + rowLine[2] - md.vp.YOffset()
	m = send(t, m, click(10, at))
	if m.topModal().cur != 2 || len(m.modals) != 1 {
		t.Fatalf("a click in the step dialog: cursor %d, %d dialogs", m.topModal().cur, len(m.modals))
	}
	m = send(t, m, click(10, at))
	if modalKindOf(m) != modalEvent {
		t.Fatalf("a second click in the step dialog opened %v", modalKindOf(m))
	}
}

// TestAClickOutsideNeverClosesTheApproveDialog, while one outside a read
// dialog does.
func TestAClickOutsideNeverClosesTheApproveDialog(t *testing.T) {
	opts := goldenDeskOptions(false)
	opts.Sources[1].(*fakeDeskSource).items[0].SHA256 = strings.Repeat("d", 64)
	m := openDesk(t, opts, 120, 30)
	m = pressDesk(t, m, key("6"))
	m = pressDesk(t, m, key("a"))
	if modalKindOf(m) != modalConfirm {
		t.Fatalf("a opened %v", modalKindOf(m))
	}
	for _, at := range [][2]int{{0, 0}, {119, 29}, {0, 15}, {60, 0}} {
		m = send(t, m, click(at[0], at[1]))
		if modalKindOf(m) != modalConfirm {
			t.Fatalf("a click at %v closed the approve dialog", at)
		}
	}
	m = pressDesk(t, m, key("n"))
	if len(m.modals) != 0 {
		t.Fatal("n did not cancel")
	}
}

// TestDialogsAreCentredBoundedAndFollowAResize: at most 80% of the window,
// at least 36×8, centred, and laid out again on a resize.
func TestDialogsAreCentredBoundedAndFollowAResize(t *testing.T) {
	cases := []struct{ w, h, x, y, mw, mh int }{
		{120, 30, 12, 3, 96, 24},
		{160, 50, 16, 5, 128, 40},
		{40, 12, 2, 1, 36, 9},
		{60, 30, 6, 3, 48, 24},
	}
	m := openDesk(t, goldenDeskOptions(false), 160, 50)
	m = pressDesk(t, m, key("1"))
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	for _, c := range cases {
		m = send(t, m, tea.WindowSizeMsg{Width: c.w, Height: c.h})
		x, y, w, h := m.modalRect()
		if x != c.x || y != c.y || w != c.mw || h != c.mh {
			t.Errorf("%dx%d: rect (%d,%d %dx%d), want (%d,%d %dx%d)", c.w, c.h, x, y, w, h, c.x, c.y, c.mw, c.mh)
		}
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) != c.h {
			t.Errorf("%dx%d: %d lines", c.w, c.h, len(lines))
		}
		for i, l := range lines {
			if lw := ansi.StringWidth(l); lw > c.w {
				t.Errorf("%dx%d: line %d is %d wide", c.w, c.h, i, lw)
			}
		}
		if top := ansi.Strip(lines[y]); !strings.Contains(top, "╭ step") {
			t.Errorf("%dx%d: the dialog's top border is not on row %d: %q", c.w, c.h, y, top)
		}
		if _, bh := m.modalBodySize(m.topModal()); m.topModal().vp.Height() != bh {
			t.Errorf("%dx%d: viewport height %d, want %d", c.w, c.h, m.topModal().vp.Height(), bh)
		}
	}
}

// TestTheRunSwitcherFiltersAndPicks: o lists every run; / filters; enter
// keeps the filter, then picks; esc clears the filter, then closes.
func TestTheRunSwitcherFiltersAndPicks(t *testing.T) {
	m := openDesk(t, goldenDeskOptions(false), 120, 30)
	m = pressDesk(t, m, key("1"))
	m = pressDesk(t, m, key("o"))
	if modalKindOf(m) != modalSwitcher {
		t.Fatalf("o opened %v", modalKindOf(m))
	}
	mustContain(t, "switcher", screen(t, m), "switch run", "alpha-run-7", "gamma-run-7", "batch-04")
	for _, k := range []string{"/", "g", "a", "m"} {
		m = pressDesk(t, m, key(k))
	}
	out := screen(t, m)
	mustContain(t, "filtered", out, "gamma-run-7")
	if strings.Contains(out, "beta-run-7") {
		t.Errorf("the filter did not narrow the list\n%s", out)
	}
	m = pressDesk(t, m, namedKey(tea.KeyEnter)) // keep the filter
	m = pressDesk(t, m, namedKey(tea.KeyEnter)) // pick
	if len(m.modals) != 0 || m.selected.name != "gamma-run-7" {
		t.Fatalf("after picking: %d dialogs, selected %v", len(m.modals), m.selected)
	}

	m = pressDesk(t, m, key("o"))
	m = pressDesk(t, m, namedKey(tea.KeyEscape))
	if len(m.modals) != 0 {
		t.Fatal("esc did not close the switcher")
	}
	// A click picks too: the first selects, the second picks.
	m = pressDesk(t, m, key("o"))
	_, y, _, _ := m.modalRect()
	m = send(t, m, click(20, y+2)) // the first run's row
	m = send(t, m, click(20, y+2))
	if len(m.modals) != 0 || m.selected != m.order[0] {
		t.Fatalf("clicks: %d dialogs, selected %v, want %v", len(m.modals), m.selected, m.order[0])
	}
}

// TestReplayStaysInBounds: the replay starts at 0, steps and jumps within
// [0, N], autoplays to N and stops, and counts events that arrive meanwhile.
func TestReplayStaysInBounds(t *testing.T) {
	logSrc, deskSrc := deskFixture()
	m := openDesk(t, optsFor(logSrc, deskSrc), 120, 30)
	m = pressDesk(t, m, key("1"))
	var ticks []int
	m.replayTick = func(gen int) tea.Cmd { ticks = append(ticks, gen); return nil }
	n := m.selLive().folder.Len()
	if m.selected.name != "alpha-run-7" || n != 9 {
		t.Fatalf("selected %v with %d events", m.selected, n)
	}
	m = pressDesk(t, m, key("p"))
	mustContain(t, "header", screen(t, m), "REPLAY 0/9 · +0 new · by time")
	if s := m.sel().state.Steps[0].Status; s != runview.StepPending {
		t.Errorf("at 0 the first step is %s, want pending", s)
	}

	press := func(k tea.KeyPressMsg) { m = pressDesk(t, m, k) }
	press(namedKey(tea.KeyLeft))
	if m.replay.i != 0 {
		t.Errorf("← at 0 moved to %d", m.replay.i)
	}
	press(key("G"))
	press(namedKey(tea.KeyRight))
	if m.replay.i != 9 {
		t.Errorf("→ at the end moved to %d", m.replay.i)
	}
	press(key("g"))
	press(namedKey(tea.KeyRight))
	press(namedKey(tea.KeyRight))
	if m.replay.i != 2 || m.sel().state.Steps[0].Status != runview.StepRunning {
		t.Errorf("at 2: i %d, fetch %s", m.replay.i, m.sel().state.Steps[0].Status)
	}
	if got, want := m.sel().state, m.selLive().folder.At(2, deskNow); got.Steps[0].Status != want.Steps[0].Status || len(m.sel().events) != 2 {
		t.Errorf("the replayed state is not Folder.At(2): %d events", len(m.sel().events))
	}

	press(namedKey(tea.KeySpace))
	if !m.replay.playing || len(ticks) != 1 {
		t.Fatalf("space: playing %v, %d ticks armed", m.replay.playing, len(ticks))
	}
	for range 20 {
		m = send(t, m, deskReplayTickMsg{gen: m.replay.gen})
	}
	if m.replay.i != 9 || m.replay.playing {
		t.Errorf("autoplay ended at %d, playing %v; want 9 and stopped", m.replay.i, m.replay.playing)
	}
	stale := m.replay.gen - 1
	m = send(t, m, deskReplayTickMsg{gen: stale})
	if m.replay.i != 9 {
		t.Error("a stale tick moved the replay")
	}

	logSrc.mu.Lock()
	logSrc.deltas["alpha-run-7"] = []runview.Delta{{Events: []runview.Event{ev(10, "11:59:59", "heartbeat", "")}}}
	logSrc.mu.Unlock()
	m = pumpDesk(t, m, m.loadCmd(m.selLive()))
	mustContain(t, "new events", screen(t, m), "REPLAY 9/9 · +1 new · by time")

	// The filter prompt and help own their keys while replay is on.
	at := m.replay.i
	press(key("/"))
	press(key("g"))
	if m.replay.i != at || string(m.filter) != "g" {
		t.Errorf("typing g in the filter: replay at %d (was %d), filter %q", m.replay.i, at, string(m.filter))
	}
	press(namedKey(tea.KeyEscape))
	press(key("?"))
	press(namedKey(tea.KeyEscape))
	if m.showHelp || m.replay == nil {
		t.Errorf("esc in help: help %v, replay on %v; want help closed, replay on", m.showHelp, m.replay != nil)
	}

	press(key("p"))
	if m.replay != nil {
		t.Fatal("p did not end the replay")
	}
	press(key("p"))
	press(namedKey(tea.KeyEscape))
	if m.replay != nil {
		t.Fatal("esc did not end the replay")
	}

	// A desk run's events carry no time.
	m = pressDesk(t, m, key("o"))
	m.topModal().list.Select(len(m.order) - 1)
	for i, k := range m.order {
		if k.name == "batch-04" {
			m.topModal().list.Select(i)
		}
	}
	m = pressDesk(t, m, namedKey(tea.KeyEnter))
	m = pressDesk(t, m, key("p"))
	mustContain(t, "desk replay", screen(t, m), "by event")
}

// TestEveryKeyInTheTable walks the key table: each key does what the table
// says, or, for keys covered in depth elsewhere, at least something visible.
func TestEveryKeyInTheTable(t *testing.T) {
	base := func() deskModel {
		m := openDesk(t, goldenDeskOptions(false), 160, 50)
		m.replayTick = func(int) tea.Cmd { return nil } // autoplay is stepped by hand
		return pressDesk(t, m, key("1"))
	}
	m := base()
	checks := []struct {
		keys string
		in   []tea.KeyPressMsg
		ok   func(before, after deskModel) bool
	}{
		{"]", []tea.KeyPressMsg{key("]")}, func(b, a deskModel) bool { return a.selected != b.selected }},
		{">", []tea.KeyPressMsg{key(">")}, func(b, a deskModel) bool { return a.selected != b.selected }},
		{"[", []tea.KeyPressMsg{key("]"), key("[")}, func(b, a deskModel) bool { return a.selected == b.selected }},
		{"<", []tea.KeyPressMsg{key("]"), key("<")}, func(b, a deskModel) bool { return a.selected == b.selected }},
		{"o", []tea.KeyPressMsg{key("o")}, func(_, a deskModel) bool { return modalKindOf(a) == modalSwitcher }},
		{"1-6", []tea.KeyPressMsg{key("4")}, func(_, a deskModel) bool { return a.view == viewMetrics }},
		{"tab", []tea.KeyPressMsg{namedKey(tea.KeyTab)}, func(b, a deskModel) bool { return a.focus != b.focus && a.view == b.view }},
		{"j", []tea.KeyPressMsg{key("g"), key("j")}, func(_, a deskModel) bool { return a.cur[viewFlow] == 1 }},
		{"↓", []tea.KeyPressMsg{key("g"), namedKey(tea.KeyDown)}, func(_, a deskModel) bool { return a.cur[viewFlow] == 1 }},
		{"k", []tea.KeyPressMsg{key("k")}, func(_, a deskModel) bool { return !a.follow[viewFlow] }},
		{"↑", []tea.KeyPressMsg{namedKey(tea.KeyUp)}, func(_, a deskModel) bool { return !a.follow[viewFlow] }},
		{"g", []tea.KeyPressMsg{key("g")}, func(_, a deskModel) bool { return a.cur[viewFlow] == 0 && !a.follow[viewFlow] }},
		{"G", []tea.KeyPressMsg{key("g"), key("G")}, func(_, a deskModel) bool { return a.follow[viewFlow] }},
		{"enter", []tea.KeyPressMsg{namedKey(tea.KeyEnter)}, func(_, a deskModel) bool { return modalKindOf(a) == modalStep }},
		{"p", []tea.KeyPressMsg{key("p")}, func(_, a deskModel) bool { return a.replay != nil }},
		{"←", []tea.KeyPressMsg{key("p"), key("G"), namedKey(tea.KeyLeft)}, func(_, a deskModel) bool { return a.replay.i == a.replay.n-1 }},
		{"→", []tea.KeyPressMsg{key("p"), namedKey(tea.KeyRight)}, func(_, a deskModel) bool { return a.replay.i == 1 }},
		{"space", []tea.KeyPressMsg{key("p"), namedKey(tea.KeySpace)}, func(_, a deskModel) bool { return a.replay.playing }},
		{"a", []tea.KeyPressMsg{key("a")}, func(_, a deskModel) bool {
			return strings.Contains(ansi.Strip(a.footer), "approve is in the queue (6)")
		}},
		{"a in the queue", []tea.KeyPressMsg{key("6"), key("a")}, func(_, a deskModel) bool {
			return strings.Contains(ansi.Strip(a.footer), "cannot approve") // the fixture's first item has no full hash
		}},
		{"s", []tea.KeyPressMsg{key("6"), key("s")}, func(_, a deskModel) bool {
			return strings.Contains(ansi.Strip(a.footer), "no desk to act on") // the fixture has no desk behind it
		}},
		{"u", []tea.KeyPressMsg{key("6"), key("u")}, func(_, a deskModel) bool {
			return strings.Contains(ansi.Strip(a.footer), "only a skipped item can be unskipped")
		}},
		{"/", []tea.KeyPressMsg{key("/")}, func(_, a deskModel) bool { return a.filtering }},
		{"r", []tea.KeyPressMsg{key("r")}, func(_, a deskModel) bool { return strings.Contains(ansi.Strip(a.footer), "refreshing") }},
		{"R", []tea.KeyPressMsg{key("R")}, func(_, a deskModel) bool {
			return strings.Contains(ansi.Strip(a.footer), "refreshing everything")
		}},
		{"m", []tea.KeyPressMsg{key("m")}, func(_, a deskModel) bool { return a.mouseOff }},
		{"?", []tea.KeyPressMsg{key("?")}, func(_, a deskModel) bool { return a.showHelp }},
		{"esc", []tea.KeyPressMsg{namedKey(tea.KeyEnter), namedKey(tea.KeyEscape)}, func(_, a deskModel) bool { return len(a.modals) == 0 }},
		{"y outside a dialog", []tea.KeyPressMsg{key("6"), key("y")}, func(_, a deskModel) bool { return a.action == nil && len(a.modals) == 0 }},
		{"n outside a dialog", []tea.KeyPressMsg{key("n")}, func(b, a deskModel) bool { return a.selected == b.selected && len(a.modals) == 0 }},
	}
	for _, c := range checks {
		after := m
		for _, k := range c.in {
			// Read the footer the last key left, not one a later key cleared.
			after = pressDesk(t, after, k)
		}
		if !c.ok(m, after) {
			t.Errorf("key %s did not do what the table says (footer %q)", c.keys, ansi.Strip(after.footer))
		}
	}
	for _, k := range []tea.KeyPressMsg{key("q"), {Code: 'c', Mod: tea.ModCtrl}} {
		if _, cmd := m.Update(k); cmd == nil || cmd() != tea.Quit() {
			t.Errorf("%s does not quit", k.String())
		}
	}
	// ctrl+c quits through a dialog; q there only closes it.
	d := pressDesk(t, m, namedKey(tea.KeyEnter))
	if _, cmd := d.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil || cmd() != tea.Quit() {
		t.Error("ctrl+c does not quit through a dialog")
	}
	if _, cmd := d.Update(key("q")); cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("q in a dialog quit the dashboard")
		}
	}
}
