package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/build"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// cockpitEpoch is the fixed clock every cockpit test starts from.
var cockpitEpoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fakeClock is a settable clock for the refresh-gap rules.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeCockpitSource counts its loads and, when gate is set, blocks each load
// until gate is closed. done, when set, is the channel each load returns, so
// a test controls when the "source goroutine" returns.
type fakeCockpitSource struct {
	name    string
	auto    bool
	sec     CockpitSection
	loads   atomic.Int32
	started chan struct{}
	gate    chan struct{}
	done    chan struct{}
}

func (f *fakeCockpitSource) source() CockpitSource {
	return CockpitSource{Name: f.name, Auto: f.auto, Load: func(context.Context) (CockpitSection, <-chan struct{}) {
		f.loads.Add(1)
		if f.started != nil {
			f.started <- struct{}{}
		}
		if f.gate != nil {
			<-f.gate
		}
		if f.done == nil {
			return f.sec, nil
		}
		return f.sec, f.done
	}}
}

// cockpitFixture is the four sections the goldens and most tests draw.
func cockpitFixture() []*fakeCockpitSource {
	return []*fakeCockpitSource{
		{name: "git", auto: true, sec: CockpitSection{
			Name: "git", State: CockpitOK,
			Headline: `3 project(s) under "/p": 1 clean, 1 dirty, 1 ahead, 0 unknown`,
			Rows: []CockpitRow{
				{Kind: CockpitRowProject, Label: "alpha", Detail: "[clean]", Path: "/p/alpha"},
				{Kind: CockpitRowProject, Label: "beta", Detail: "[2 modified]", Path: "/p/beta"},
				{Kind: CockpitRowProject, Label: "gamma", Detail: "[1 ahead]", Path: "/p/gamma"},
			},
		}},
		{name: "prs", sec: CockpitSection{
			Name: "prs", State: CockpitDegraded,
			Headline: "0 active review(s), 2 awaiting you, 1 open by you",
			Notes:    []string{"your-open: query failed"},
			Rows: []CockpitRow{
				{Kind: CockpitRowPR, Label: "o/r#1", Detail: "awaiting you · fix the thing", Ref: "o/r#1"},
				{Kind: CockpitRowPR, Label: "o/r#2", Detail: "awaiting you · add the other", Ref: "o/r#2"},
				{Kind: CockpitRowPR, Label: "o/r#3", Detail: "yours · mine", Ref: "o/r#3"},
			},
		}},
		{name: "clean", sec: CockpitSection{Name: "clean", State: CockpitFailed, Error: "timed out after 20s"}},
		{name: "bench", sec: CockpitSection{
			Name: "bench", State: CockpitOK, Headline: "hearth ok, chronicle unavailable",
			Rows: []CockpitRow{
				{Kind: CockpitRowInfo, Label: "hearth", Detail: "ok — up"},
				{Kind: CockpitRowInfo, Label: "chronicle", Detail: "unavailable — down"},
			},
		}},
	}
}

// newTestCockpit builds a 100×30 cockpit over fakes with a fixed clock and no
// timer, and with build as its argv builder (nil means PickerArgv).
func newTestCockpit(fakes []*fakeCockpitSource, build ArgvBuilder) (cockpitModel, *fakeClock) {
	srcs := make([]CockpitSource, 0, len(fakes))
	for _, f := range fakes {
		srcs = append(srcs, f.source())
	}
	m := newCockpitModel(context.Background(), CockpitOptions{Sources: srcs, BuildArgv: build, NoIcons: true, Theme: theme.Default()})
	clock := &fakeClock{t: cockpitEpoch}
	m.now = clock.now
	m.schedule = func() tea.Cmd { return nil }
	out, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return out.(cockpitModel), clock
}

// runCmd executes cmd and every command a batch carries, feeding each
// resulting message back through Update, until nothing is left. Commands
// must not block.
func runCmd(t *testing.T, m cockpitModel, cmd tea.Cmd) cockpitModel {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 100 {
			t.Fatal("runCmd: command loop did not settle")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			out, next := m.Update(msg)
			m = out.(cockpitModel)
			queue = append(queue, next)
		}
	}
	return m
}

// openCockpit runs the opening collection to completion.
func openCockpit(t *testing.T, fakes []*fakeCockpitSource, build ArgvBuilder) (cockpitModel, *fakeClock) {
	t.Helper()
	m, clock := newTestCockpit(fakes, build)
	m = runCmd(t, m, m.Init())
	return m, clock
}

func pressCockpit(m cockpitModel, k tea.KeyPressMsg) (cockpitModel, tea.Cmd) {
	out, cmd := m.Update(k)
	return out.(cockpitModel), cmd
}

func keyCode(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

var shiftTab = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// --- opening and refresh discipline ---

func TestCockpit_OpenCollectsEverySectionOnce(t *testing.T) {
	fakes := cockpitFixture()
	m, _ := openCockpit(t, fakes, nil)
	for i, f := range fakes {
		if got := f.loads.Load(); got != 1 {
			t.Errorf("%s loaded %d times on open, want 1", f.name, got)
		}
		if m.secs[i].busy || m.secs[i].snap.State != f.sec.State {
			t.Errorf("%s after open: busy=%v state=%v, want idle with its result", f.name, m.secs[i].busy, m.secs[i].snap.State)
		}
	}
}

// TestCockpit_DoubleRefreshStartsOneLoad: r while the section's refresh is
// still running does not start a second one.
func TestCockpit_DoubleRefreshStartsOneLoad(t *testing.T) {
	fakes := cockpitFixture()
	m, clock := openCockpit(t, fakes, nil)
	git := fakes[0]
	git.started = make(chan struct{}, 1)
	git.gate = make(chan struct{})
	clock.advance(time.Minute)

	m, cmd := pressCockpit(m, key("r"))
	if cmd == nil {
		t.Fatal("first r started nothing")
	}
	results := make(chan tea.Msg, 1)
	go func() { results <- cmd() }()
	<-git.started

	clock.advance(time.Minute) // past the minimum gap: only the busy rule can refuse now
	m, cmd = pressCockpit(m, key("r"))
	if cmd != nil {
		close(git.gate)
		t.Fatal("second r while the first is running returned a command")
	}
	if !strings.Contains(m.footer, "already running") {
		t.Errorf("footer = %q, want the already-running refusal", m.footer)
	}
	close(git.gate)
	out, _ := m.Update(<-results)
	m = out.(cockpitModel)
	if got := git.loads.Load(); got != 2 {
		t.Errorf("git loaded %d times, want 2 (open + one refresh)", got)
	}
	if m.secs[0].busy {
		t.Error("git still busy after its source returned")
	}
}

// TestCockpit_BusyUntilTheSourceReturns: a result that arrives at the
// deadline while the source goroutine runs on leaves the section busy, and
// refusing refreshes, until that goroutine returns.
func TestCockpit_BusyUntilTheSourceReturns(t *testing.T) {
	fakes := cockpitFixture()
	git := fakes[0]
	git.done = make(chan struct{})
	git.sec = CockpitSection{Name: "git", State: CockpitFailed, Error: "timed out after 20s"}
	m, clock := newTestCockpit(fakes, nil)
	cmd, _ := m.refresh(0)
	out, wait := m.Update(cmd())
	m = out.(cockpitModel)
	if m.secs[0].snap.State != CockpitFailed {
		t.Fatalf("the deadline result was not shown: %+v", m.secs[0].snap)
	}
	if !m.secs[0].busy {
		t.Fatal("section went idle while its source goroutine was still running")
	}
	clock.advance(time.Minute)
	if _, c := pressCockpit(m, key("r")); c != nil {
		t.Error("r on a section whose source is still running started another load")
	}
	if !strings.Contains(m.headerView(), "git refreshing…") {
		t.Errorf("header = %q, want git refreshing…", m.headerView())
	}
	close(git.done)
	out, _ = m.Update(wait())
	m = out.(cockpitModel)
	if m.secs[0].busy {
		t.Error("section still busy after its source goroutine returned")
	}
	if _, c := pressCockpit(m, key("r")); c == nil {
		t.Error("r after the source returned started nothing")
	}
}

func TestCockpit_MinimumGapBetweenRefreshes(t *testing.T) {
	fakes := cockpitFixture()
	m, clock := openCockpit(t, fakes, nil)
	clock.advance(10 * time.Second)
	m, cmd := pressCockpit(m, key("r"))
	if cmd != nil {
		t.Fatal("r 10s after the last refresh started one; the gap is 15s")
	}
	if !strings.Contains(m.footer, "try again in 5s") {
		t.Errorf("footer = %q, want the wait named", m.footer)
	}
	clock.advance(6 * time.Second)
	m, cmd = pressCockpit(m, key("r"))
	if cmd == nil {
		t.Fatal("r 16s after the last refresh started nothing")
	}
	runCmd(t, m, cmd)
	if got := fakes[0].loads.Load(); got != 2 {
		t.Errorf("git loaded %d times, want 2", got)
	}
}

// TestCockpit_TimerRefreshesOnlyTheAutoSection: the timer never reaches the
// network (prs) or the long walks.
func TestCockpit_TimerRefreshesOnlyTheAutoSection(t *testing.T) {
	fakes := cockpitFixture()
	m, clock := openCockpit(t, fakes, nil)
	for range 3 {
		clock.advance(cockpitAutoEvery)
		out, cmd := m.Update(cockpitTickMsg{})
		m = runCmd(t, out.(cockpitModel), cmd)
	}
	if got := fakes[0].loads.Load(); got != 4 {
		t.Errorf("git loaded %d times, want 4 (open + three ticks)", got)
	}
	for _, f := range fakes[1:] {
		if got := f.loads.Load(); got != 1 {
			t.Errorf("%s loaded %d times; the timer must never refresh it", f.name, got)
		}
	}
}

func TestCockpit_TimerSkipsABusyOrRecentSection(t *testing.T) {
	fakes := cockpitFixture()
	m, clock := openCockpit(t, fakes, nil)
	clock.advance(5 * time.Second)
	out, cmd := m.Update(cockpitTickMsg{})
	m = runCmd(t, out.(cockpitModel), cmd)
	if got := fakes[0].loads.Load(); got != 1 {
		t.Errorf("a tick 5s after a refresh loaded git again (%d loads)", got)
	}
	clock.advance(time.Minute)
	m.secs[0].busy = true
	out, cmd = m.Update(cockpitTickMsg{})
	runCmd(t, out.(cockpitModel), cmd)
	if got := fakes[0].loads.Load(); got != 1 {
		t.Errorf("a tick on a busy section loaded it again (%d loads)", got)
	}
}

func TestCockpit_TickRearmsTheTimer(t *testing.T) {
	m, _ := newTestCockpit(cockpitFixture(), nil)
	armed := 0
	m.schedule = func() tea.Cmd { armed++; return nil }
	m.Update(cockpitTickMsg{})
	if armed != 1 {
		t.Errorf("a tick armed the timer %d times, want 1", armed)
	}
}

// --- keys ---

func TestCockpit_EnterOnAPRRowRunsPrThroughTheBuilder(t *testing.T) {
	var gotPrefix []string
	var gotArg string
	build := func(prefix []string, arg string, optional bool) ([]string, error) {
		gotPrefix, gotArg = prefix, arg
		if optional {
			t.Error("a PR ref is not optional")
		}
		return append(append([]string(nil), prefix...), "--", arg), nil
	}
	m, _ := openCockpit(t, cockpitFixture(), build)
	m, _ = pressCockpit(m, keyCode(tea.KeyTab)) // prs
	m, _ = pressCockpit(m, keyCode(tea.KeyDown))
	m, cmd := pressCockpit(m, keyCode(tea.KeyEnter))
	if !isQuit(cmd) {
		t.Fatal("enter on a PR row did not quit to run it")
	}
	if strings.Join(gotPrefix, " ") != "pr" || gotArg != "o/r#2" {
		t.Errorf("builder got prefix=%q arg=%q, want [pr] o/r#2", gotPrefix, gotArg)
	}
	if m.action.Kind != ActionRunVerb || strings.Join(m.action.Argv, " ") != "pr -- o/r#2" {
		t.Errorf("action = %+v, want RunVerb with the builder's argv", m.action)
	}
}

func TestCockpit_ArefusedRefStaysInTheCockpit(t *testing.T) {
	build := func([]string, string, bool) ([]string, error) { return nil, errors.New("not a ref") }
	m, _ := openCockpit(t, cockpitFixture(), build)
	m, _ = pressCockpit(m, keyCode(tea.KeyTab))
	m, cmd := pressCockpit(m, keyCode(tea.KeyEnter))
	if cmd != nil || m.action.Kind != ActionNone {
		t.Fatalf("a refused ref ran something: action=%+v", m.action)
	}
	if !strings.Contains(m.footer, "not a ref") {
		t.Errorf("footer = %q, want the refusal", m.footer)
	}
}

func TestCockpit_EnterOnAProjectRowOnlySaysFocusIsNotAvailable(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	m, cmd := pressCockpit(m, keyCode(tea.KeyEnter))
	if cmd != nil || m.action.Kind != ActionNone {
		t.Fatalf("enter on a project row did something: action=%+v", m.action)
	}
	if !strings.Contains(m.footer, "focus not available yet") {
		t.Errorf("footer = %q", m.footer)
	}
}

// TestCockpit_DigitsOpenNothing: rows carry no visible numbers, so a digit
// must not open (and on a PR row, start a review on) an unnumbered row.
func TestCockpit_DigitsOpenNothing(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	m, _ = pressCockpit(m, keyCode(tea.KeyTab)) // prs
	for _, d := range "123456789" {
		var cmd tea.Cmd
		m, cmd = pressCockpit(m, key(string(d)))
		if cmd != nil || m.action.Kind != ActionNone {
			t.Errorf("%c on prs: cmd=%v action=%+v, want nothing", d, cmd != nil, m.action)
		}
	}
}

// TestCockpit_ScrollFollowsTheCursorInUpdate: the offset moves in Update, so
// a model that has never been drawn still keeps its cursor in view, and View
// leaves the model alone.
func TestCockpit_ScrollFollowsTheCursorInUpdate(t *testing.T) {
	fakes := cockpitFixture()
	for i := range 40 {
		fakes[0].sec.Rows = append(fakes[0].sec.Rows, CockpitRow{Kind: CockpitRowProject, Label: fmt.Sprintf("p%02d", i)})
	}
	m, _ := openCockpit(t, fakes, nil)
	for range 35 {
		m, _ = pressCockpit(m, key("j"))
	}
	s := m.secs[0]
	room := m.focusRoom()
	if s.offset == 0 || s.cursor < s.offset || s.cursor >= s.offset+room {
		t.Fatalf("cursor %d, offset %d, room %d: Update did not scroll the cursor into view", s.cursor, s.offset, room)
	}
	before := s.offset
	view := m.View().Content
	if m.secs[0].offset != before {
		t.Error("View moved the scroll offset")
	}
	var plain bytes.Buffer
	if _, err := theme.Default().Writer(&plain, []string{"NO_COLOR=1"}).Write([]byte(view)); err != nil {
		t.Fatal(err)
	}
	if want := "> " + m.visibleRows(0)[s.cursor].Label; !strings.Contains(plain.String(), want) {
		t.Errorf("the selected row %q is not drawn:\n%s", want, plain.String())
	}
	for range 40 {
		m, _ = pressCockpit(m, key("k"))
	}
	if m.secs[0].offset != 0 {
		t.Errorf("offset = %d after moving back to the top, want 0", m.secs[0].offset)
	}
}

func TestCockpit_TabCyclesSectionsBothWays(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	for _, want := range []int{1, 2, 3, 0} {
		m, _ = pressCockpit(m, keyCode(tea.KeyTab))
		if m.focus != want {
			t.Fatalf("tab: focus = %d, want %d", m.focus, want)
		}
	}
	m, _ = pressCockpit(m, shiftTab)
	if m.focus != 3 {
		t.Errorf("shift+tab from git: focus = %d, want bench (3)", m.focus)
	}
}

func TestCockpit_JKMoveTheCursorWithinTheSection(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	m, _ = pressCockpit(m, key("j"))
	m, _ = pressCockpit(m, key("j"))
	m, _ = pressCockpit(m, key("j")) // clamps at the last row
	if m.secs[0].cursor != 2 {
		t.Errorf("cursor = %d, want 2", m.secs[0].cursor)
	}
	m, _ = pressCockpit(m, key("k"))
	if m.secs[0].cursor != 1 {
		t.Errorf("cursor after k = %d, want 1", m.secs[0].cursor)
	}
}

func TestCockpit_FilterNarrowsRowsAndEscClearsIt(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	m, _ = pressCockpit(m, key("/"))
	for _, r := range "bet" {
		m, _ = pressCockpit(m, key(string(r)))
	}
	if rows := m.visibleRows(0); len(rows) != 1 || rows[0].Label != "beta" {
		t.Fatalf("filtered git rows = %+v, want only beta", rows)
	}
	m.filter = []rune("/p/")
	if rows := m.visibleRows(0); len(rows) != 0 {
		t.Fatalf("a filter matching only the hidden path kept rows %+v; it must match drawn fields only", rows)
	}
	m.filter = []rune("bet")
	m, cmd := pressCockpit(m, key("q")) // typed into the filter, not a quit
	if isQuit(cmd) {
		t.Fatal("q while filtering quit")
	}
	m, _ = pressCockpit(m, keyCode(tea.KeyBackspace))
	m, _ = pressCockpit(m, keyCode(tea.KeyEnter))
	if m.filtering || string(m.filter) != "bet" {
		t.Fatalf("enter should keep the filter: filtering=%v filter=%q", m.filtering, string(m.filter))
	}
	m, cmd = pressCockpit(m, keyCode(tea.KeyEscape))
	if isQuit(cmd) || len(m.filter) != 0 {
		t.Fatalf("esc with a filter should clear it, not quit (filter=%q)", string(m.filter))
	}
	if _, cmd = pressCockpit(m, keyCode(tea.KeyEscape)); !isQuit(cmd) {
		t.Error("esc with no filter did not quit")
	}
}

func TestCockpit_HelpOpensAndCloses(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	m, _ = pressCockpit(m, key("?"))
	if !m.help || !strings.Contains(m.View().Content, "refresh every section") {
		t.Fatalf("? did not show help:\n%s", m.View().Content)
	}
	m, cmd := pressCockpit(m, keyCode(tea.KeyEscape))
	if m.help || isQuit(cmd) {
		t.Error("esc in help should close it, not quit")
	}
}

func TestCockpit_QuitKeys(t *testing.T) {
	for name, k := range map[string]tea.KeyPressMsg{
		"q":      key("q"),
		"esc":    keyCode(tea.KeyEscape),
		"ctrl+c": {Code: 'c', Mod: tea.ModCtrl},
	} {
		m, _ := openCockpit(t, cockpitFixture(), nil)
		m, cmd := pressCockpit(m, k)
		if !isQuit(cmd) || m.action.Kind != ActionNone {
			t.Errorf("%s: quit=%v action=%+v, want a plain quit", name, isQuit(cmd), m.action)
		}
	}
}

// TestCockpit_NoKeyMutatesAnything sweeps every printable key outside the
// cockpit's own: none may run an action, quit, or start a load. The cockpit
// is read-only; a stray d, x or K does nothing.
func TestCockpit_NoKeyMutatesAnything(t *testing.T) {
	own := "jkrR?/q"
	for c := rune('!'); c <= '~'; c++ {
		if strings.ContainsRune(own, c) {
			continue
		}
		fakes := cockpitFixture()
		m, clock := openCockpit(t, fakes, nil)
		clock.advance(time.Hour)
		for _, focus := range []int{0, 1, 2, 3} {
			m.focus = focus
			out, cmd := m.Update(key(string(c)))
			m = out.(cockpitModel)
			if cmd != nil || m.action.Kind != ActionNone {
				t.Errorf("key %q on section %d: cmd=%v action=%+v, want nothing", c, focus, cmd != nil, m.action)
			}
		}
		for _, f := range fakes {
			if f.loads.Load() != 1 {
				t.Errorf("key %q reloaded %s", c, f.name)
			}
		}
	}
	for _, k := range []tea.KeyPressMsg{keyCode(tea.KeyDelete), keyCode(tea.KeyBackspace), {Code: 'd', Mod: tea.ModCtrl}, {Code: 'k', Mod: tea.ModCtrl}} {
		m, _ := openCockpit(t, cockpitFixture(), nil)
		if _, cmd := pressCockpit(m, k); cmd != nil {
			t.Errorf("%s returned a command", k.String())
		}
	}
}

// --- view ---

// goldenCockpit is the fixture opened, with the clock moved on two minutes
// and the cursor on beta, so the header ages and the selection both show.
func goldenCockpit(t *testing.T) cockpitModel {
	t.Helper()
	m, clock := openCockpit(t, cockpitFixture(), nil)
	clock.advance(2 * time.Minute)
	m, _ = pressCockpit(m, key("j"))
	return m
}

func TestGoldenCockpit(t *testing.T) {
	forceTrueColor(t)
	assertGolden(t, "cockpit", goldenCockpit(t).View().Content)
}

// TestGoldenCockpitNoColor is the same screen through the NO_COLOR writer the
// program's output takes, so the plain layout is pinned too.
func TestGoldenCockpitNoColor(t *testing.T) {
	var buf bytes.Buffer
	w := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
	if _, err := w.Write([]byte(goldenCockpit(t).View().Content)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatal("the no-color render still carries escapes")
	}
	assertGolden(t, "cockpit_nocolor", buf.String())
}

func TestCockpit_ViewFitsTheWindow(t *testing.T) {
	m := goldenCockpit(t)
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) != 30 {
		t.Errorf("view is %d lines, want exactly the 30-line window", len(lines))
	}
}

func TestCockpit_HeaderShowsLoadingBeforeTheFirstResult(t *testing.T) {
	m, _ := newTestCockpit(cockpitFixture(), nil)
	m.secs[1].busy = true
	h := m.headerView()
	if !strings.Contains(h, "prs loading…") {
		t.Errorf("header = %q, want prs loading…", h)
	}
}

// TestCockpit_DrawsNothingUnsafe puts termsafetest.Hostile in every field a
// section carries, and in the filter, and asserts the whole screen inert.
func TestCockpit_DrawsNothingUnsafe(t *testing.T) {
	h := termsafetest.Hostile
	hostileRow := func(kind CockpitRowKind) CockpitRow {
		return CockpitRow{Kind: kind, Label: h("label"), Detail: h("detail"), Path: h("/p/x"), Ref: h("o/r#9")}
	}
	fakes := []*fakeCockpitSource{
		{name: h("git"), sec: CockpitSection{Name: h("git"), State: CockpitOK, Headline: h("headline"), Rows: []CockpitRow{hostileRow(CockpitRowProject), hostileRow(CockpitRowProject), hostileRow(CockpitRowProject)}}},
		{name: h("prs"), sec: CockpitSection{Name: h("prs"), State: CockpitDegraded, Headline: h("headline"), Notes: []string{h("note")}, Rows: []CockpitRow{hostileRow(CockpitRowPR)}}},
		{name: h("clean"), sec: CockpitSection{Name: h("clean"), State: CockpitFailed, Error: h("error")}},
		{name: h("bench"), sec: CockpitSection{Name: h("bench"), State: CockpitOK, Headline: h("headline"), Rows: []CockpitRow{hostileRow(CockpitRowInfo)}}},
	}
	build := func([]string, string, bool) ([]string, error) { return nil, errors.New(h("refused")) }
	m, clock := openCockpit(t, fakes, build)

	views := map[string]string{"open": m.View().Content}
	m, _ = pressCockpit(m, keyCode(tea.KeyTab))
	m, _ = pressCockpit(m, keyCode(tea.KeyEnter)) // the builder's hostile refusal
	views["refused ref"] = m.View().Content
	m, _ = pressCockpit(m, key("r")) // a refusal naming the section
	views["refresh refused"] = m.View().Content
	clock.advance(time.Minute)
	m, cmd := pressCockpit(m, key("R"))
	views["refresh all"] = m.View().Content
	m = runCmd(t, m, cmd)
	m, _ = pressCockpit(m, keyCode(tea.KeyTab))
	m, _ = pressCockpit(m, keyCode(tea.KeyEnter)) // the failed section
	m, _ = pressCockpit(m, key("/"))
	out, _ := m.Update(tea.PasteMsg{Content: h("typed")})
	m = out.(cockpitModel)
	views["filtering"] = m.View().Content
	m, _ = pressCockpit(m, keyCode(tea.KeyEnter))
	views["filtered"] = m.View().Content
	for name, v := range views {
		if !strings.Contains(v, "label") && name != "filtering" && name != "filtered" {
			t.Errorf("%s: the hostile rows did not draw; the check would pass vacuously:\n%s", name, v)
		}
		termsafetest.AssertInert(t, "cockpit "+name, v)
	}
}

// TestCockpit_LongValuesAreCapped: no field may run past its named cap.
func TestCockpit_LongValuesAreCapped(t *testing.T) {
	long := strings.Repeat("x", 4096)
	fakes := []*fakeCockpitSource{{name: long, sec: CockpitSection{
		State: CockpitOK, Headline: long, Notes: []string{long},
		Rows: []CockpitRow{{Kind: CockpitRowProject, Label: long, Detail: long}},
	}}}
	m, _ := openCockpit(t, fakes, nil)
	m.width = 0 // no window truncation: the caps alone must hold
	for _, line := range strings.Split(m.View().Content, "\n") {
		if n := strings.Count(line, "x"); n > cockpitNoteMax+cockpitHeadlineMax {
			t.Errorf("a line carries %d runes of one value; a cap is missing", n)
		}
	}
	if strings.Contains(m.View().Content, strings.Repeat("x", cockpitNoteMax+1)) {
		t.Error("a value ran past every cap")
	}
}

// --- hub row ---

func TestHub_StatusRowOpensTheCockpit(t *testing.T) {
	m := sized(newModel(context.Background(), tmux.New(&exec.FakeRunner{}), RunOptions{
		Hub:   []HubEntry{{Name: "status", Use: "status", Short: "overview"}},
		Theme: theme.Default(),
	}), 100, 20)
	if !strings.Contains(m.View().Content, "$ forgectl status --tui") {
		t.Errorf("the status row's command line is not shown:\n%s", m.View().Content)
	}
	m, cmd := press(m, tea.KeyEnter)
	if cmd == nil || m.action.Kind != ActionRunVerb || strings.Join(m.action.Argv, " ") != "status --tui" {
		t.Errorf("action = %+v, want RunVerb [status --tui]", m.action)
	}
}

// --- import graph ---

// TestCockpitImportGraph pins the seam the cockpit is built on: internal/tui
// draws plain CockpitSnapshot values and never reaches the packages that
// produce them, directly or through anything it imports. internal/cli does
// the converting.
//
// Mutation that turns it red: import internal/bench (or pr, clean, status)
// anywhere in internal/tui's non-test files.
func TestCockpitImportGraph(t *testing.T) {
	const module = "github.com/cameronsjo/forgectl"
	forbidden := []string{module + "/internal/pr", module + "/internal/bench", module + "/internal/clean", module + "/internal/status"}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var walk func(pkg, via string)
	walk = func(pkg, via string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		for _, bad := range forbidden {
			if pkg == bad || strings.HasPrefix(pkg, bad+"/") {
				t.Errorf("internal/tui reaches %s (via %s)", pkg, via)
			}
		}
		for _, goos := range []string{"linux", "darwin", "windows"} {
			ctx := build.Default
			ctx.GOOS = goos
			bp, err := ctx.ImportDir(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, module+"/"))), 0)
			if err != nil {
				var noGo *build.NoGoError
				if errors.As(err, &noGo) {
					continue
				}
				t.Fatalf("list %s for %s: %v", pkg, goos, err)
			}
			for _, imp := range bp.Imports {
				if strings.HasPrefix(imp, module+"/") {
					walk(imp, path.Join(via, path.Base(imp)))
				}
			}
		}
	}
	walk(module+"/internal/tui", "tui")
	if !seen[module+"/internal/termsafe"] {
		t.Fatal("the walk never reached internal/termsafe; it is not following imports")
	}
}

func TestCockpit_FilterDropsControlRunes(t *testing.T) {
	m, _ := openCockpit(t, cockpitFixture(), nil)
	m, _ = pressCockpit(m, key("/"))
	out, _ := m.Update(tea.PasteMsg{Content: termsafetest.Hostile("be")})
	m = out.(cockpitModel)
	if got := string(m.filter); got != "be[31m" {
		t.Errorf("filter = %q, want the pasted text without its escape, bidi and carriage-return runes", got)
	}
}
