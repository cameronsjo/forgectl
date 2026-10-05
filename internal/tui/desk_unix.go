// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	binding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The desk dashboard: a read-only view of runs from any number of sources
// (spec-described logs, desk batches) and of the desk's approval queue. It
// loads in tea.Cmds and applies in Update. Everything it draws that came from
// a file goes through termsafe at draw time, whatever the source already did.

const (
	deskTickEvery  = time.Second
	deskListEvery  = 5 * time.Second
	deskRefreshGap = 15 * time.Second
	// deskPrefetch bounds how many runs other than the selected one a single
	// list reload starts loading, so a source with hundreds of runs does not
	// start hundreds of reads at once.
	deskPrefetch = 12
	// deskNoteFor is how long a "log restarted" note stays on screen.
	deskNoteFor = 30 * time.Second

	deskMinW = 40
	deskMinH = 12
)

// The six views, numbered as the keys pick them.
const (
	viewFlow = iota + 1
	viewEvents
	viewTimeline
	viewMetrics
	viewSummary
	viewQueue
	viewCount = viewQueue
)

var deskViewNames = [...]string{"", "flow", "events", "timeline", "metrics", "summary", "queue"}

// The panes focus can be on. In the single-pane layout the one pane shown has
// the focus whatever this says.
const (
	paneList = iota
	paneFlow
	paneSide
)

// deskRunKey is a run's identity. Selection follows it, never a row index.
type deskRunKey struct{ source, name string }

// deskRun is one run's loaded state. The model holds it by pointer, so a copy
// of the model shares it with the commands loading into it.
type deskRun struct {
	key  deskRunKey
	src  runview.Source
	idx  int // src's place in the sources, for telling which source listed it
	ref  runview.RunRef
	spec *runview.Spec
	// cur is the load position. Only the load command dereferences it, and
	// the busy flag keeps two loads of one run from overlapping.
	cur *runview.Cursor

	folder  *runview.Folder
	defs    []runview.StepDef
	events  []runview.Event
	metrics []runview.Metric
	summary []runview.KV
	timing  []runview.StepTiming
	state   runview.RunState
	live    runview.LiveState // the desk's word for a desk run, else empty

	err     error // the load's Delta.Err
	loadErr error // an error from Load itself
	dropped int
	fields  int
	partial bool

	note        string
	noteUntil   time.Time
	everEvents  bool
	gone        bool
	loaded      bool
	busy        bool
	loadedAt    time.Time // ref.Updated when the last load started
	refreshedAt time.Time
}

// deskHooks are the seams for the dialogs and queue verbs. A nil hook leaves
// its key a no-op.
type deskHooks struct {
	open     func(m deskModel) (deskModel, tea.Cmd)
	verb     func(m deskModel, verb string) (deskModel, tea.Cmd)
	switcher func(m deskModel) (deskModel, tea.Cmd)
	replay   func(m deskModel) (deskModel, tea.Cmd)
}

// deskModel is the dashboard.
type deskModel struct {
	ctx    context.Context
	theme  theme.Theme
	styles theme.Styles
	glyph  glyphSet
	keys   keymap.Desk
	help   help.Model

	sources []runview.Source
	specs   map[string]*runview.Spec
	failed  []DeskSourceFailure
	now     func() time.Time
	// schedule arms the next tick; tests replace it so a model driven by hand
	// never sleeps.
	schedule func() tea.Cmd

	width, height int

	runs      map[deskRunKey]*deskRun
	order     []deskRunKey
	listBusy  bool
	listed    bool
	lastList  time.Time
	listErrs  map[string]error // by source name, from the latest list
	items     []runview.QueueItem
	queueErrs []error
	restore   *DeskViewState
	selected  deskRunKey
	hasSel    bool
	view      int
	focus     int
	frame     int
	top, cur  [viewCount + 1]int
	follow    [viewCount + 1]bool
	pausedAt  int // events held when following stopped
	filter    []rune
	filtering bool
	showHelp  bool
	helpTop   int
	mouseOff  bool
	footer    string
	lastAll   time.Time
	hooks     deskHooks
	action    DeskAction
	listTop   int
}

// Messages.
type (
	deskTickMsg struct{}
	deskListMsg struct {
		lists []deskSourceList
	}
	deskLoadMsg struct {
		key deskRunKey
		d   runview.Delta
		err error
	}
)

// deskSourceList is one source's List (and Queue) result.
type deskSourceList struct {
	name     string
	idx      int // the source's place in DeskOptions.Sources
	src      runview.Source
	refs     []runview.RunRef
	err      error
	items    []runview.QueueItem
	hasQueue bool
	queueErr error
}

// RunDesk drives the dashboard until the operator quits and returns the
// deferred action, if any, and the view state to restore on the next call.
func RunDesk(ctx context.Context, opts DeskOptions) (DeskResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := tea.NewProgram(newDeskModel(ctx, opts), tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return DeskResult{}, err
	}
	fm, ok := final.(deskModel)
	if !ok {
		return DeskResult{}, nil
	}
	return DeskResult{Action: fm.action, State: fm.viewState()}, nil
}

func newDeskModel(ctx context.Context, opts DeskOptions) deskModel {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	m := deskModel{
		ctx:      ctx,
		theme:    opts.Theme,
		styles:   opts.Theme.Styles(),
		glyph:    pickGlyphs(opts.NoIcons),
		keys:     keymap.NewDesk(),
		help:     help.New(),
		sources:  opts.Sources,
		specs:    opts.Specs,
		failed:   opts.Failed,
		now:      now,
		runs:     map[deskRunKey]*deskRun{},
		listErrs: map[string]error{},
		restore:  opts.Restore,
		view:     viewFlow,
		focus:    paneFlow,
		// Init starts the first list.
		listBusy: true,
		schedule: func() tea.Cmd {
			return tea.Tick(deskTickEvery, func(time.Time) tea.Msg { return deskTickMsg{} })
		},
	}
	m.follow[viewFlow], m.follow[viewEvents] = true, true
	m.applyHelpStyles()
	if r := opts.Restore; r != nil {
		m.mouseOff = r.MouseOff
		m.filter = []rune(r.Filter)
		if r.View >= viewFlow && r.View <= viewCount {
			m.view = r.View
		}
		if r.Focus >= paneList && r.Focus <= paneSide {
			m.focus = r.Focus
		}
	}
	return m
}

// applyHelpStyles draws the footer hints in the theme's styles.
func (m *deskModel) applyHelpStyles() {
	s := m.styles
	m.help.Styles = help.Styles{
		Ellipsis:       s.Dim,
		ShortKey:       s.Accent,
		ShortDesc:      s.Muted,
		ShortSeparator: s.Dim,
		FullKey:        s.Accent,
		FullDesc:       s.Muted,
		FullSeparator:  s.Dim,
	}
}

func (m deskModel) viewState() DeskViewState {
	st := DeskViewState{View: m.view, Focus: m.focus, Filter: string(m.filter), MouseOff: m.mouseOff}
	if m.hasSel {
		st.Source, st.Run = m.selected.source, m.selected.name
	}
	return st
}

// Init lists the sources, arms the tick, and probes the background colour
// where that is safe.
func (m deskModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.listCmd(), m.schedule()}
	env := theme.Env{
		StdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		Term:      os.Getenv("TERM"),
		NoColor:   os.Getenv("NO_COLOR") != "",
	}
	if theme.ShouldProbe(m.theme.Mode(), env) {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

// listCmd lists every source (and the queue of those that have one).
func (m deskModel) listCmd() tea.Cmd {
	sources := slices.Clone(m.sources)
	return func() tea.Msg {
		out := make([]deskSourceList, 0, len(sources))
		for i, src := range sources {
			l := deskSourceList{name: src.Name(), idx: i, src: src}
			l.refs, l.err = src.List()
			if qs, ok := src.(runview.QueueSource); ok {
				l.hasQueue = true
				l.items, l.queueErr = qs.Queue()
			}
			out = append(out, l)
		}
		return deskListMsg{lists: out}
	}
}

// loadCmd starts a load of e, or does nothing while one is running.
func (m deskModel) loadCmd(e *deskRun) tea.Cmd {
	if e == nil || e.busy || e.gone {
		return nil
	}
	e.busy = true
	key, src, ref, cur := e.key, e.src, e.ref, e.cur
	e.loadedAt = ref.Updated
	return func() tea.Msg {
		d, err := src.Load(ref, cur)
		return deskLoadMsg{key: key, d: d, err: err}
	}
}

// Update handles msg, then settles the scroll offsets so View only reads.
func (m deskModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	out, cmd := m.update(msg)
	next := out.(deskModel)
	next.syncScroll()
	return next, cmd
}

func (m deskModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		m.help.SetWidth(t.Width)
		return m, nil
	case tea.BackgroundColorMsg:
		if m.theme.Mode() == theme.ModeAuto {
			m.theme = m.theme.WithDark(t.IsDark())
			m.styles = m.theme.Styles()
			m.applyHelpStyles()
		}
		return m, nil
	case deskTickMsg:
		return m.onTick()
	case deskListMsg:
		return m.onList(t)
	case deskLoadMsg:
		return m.onLoad(t)
	case tea.PasteMsg:
		if m.filtering {
			m.typeFilter(t.Content)
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.onKey(t)
	case tea.MouseClickMsg:
		return m.onClick(t.Mouse())
	case tea.MouseWheelMsg:
		return m.onWheel(t.Mouse()), nil
	}
	return m, nil
}

func (m deskModel) onTick() (tea.Model, tea.Cmd) {
	m.frame++
	cmds := []tea.Cmd{m.schedule()}
	if e := m.sel(); e != nil && e.loaded {
		if c := m.loadCmd(e); c != nil {
			cmds = append(cmds, c)
		}
	}
	if !m.listBusy && m.now().Sub(m.lastList) >= deskListEvery {
		m.listBusy = true
		cmds = append(cmds, m.listCmd())
	}
	return m, tea.Batch(cmds...)
}

// onList merges a list result into the runs: new runs appear, runs a
// successful list no longer shows are marked gone (their last state stays),
// and runs that are new or changed start loading.
func (m deskModel) onList(msg deskListMsg) (tea.Model, tea.Cmd) {
	m.listBusy = false
	m.lastList = m.now()
	m.listed = true
	m.listErrs = map[string]error{}
	m.queueErrs = nil
	m.items = nil
	for _, l := range msg.lists {
		if l.err != nil {
			m.listErrs[l.name] = l.err
		}
		if l.hasQueue {
			m.items = append(m.items, l.items...)
			if l.queueErr != nil {
				m.queueErrs = append(m.queueErrs, l.queueErr)
			}
		}
		if l.err != nil {
			continue
		}
		seen := map[deskRunKey]bool{}
		for _, ref := range l.refs {
			k := deskRunKey{ref.Source, ref.Name}
			seen[k] = true
			e := m.runs[k]
			if e == nil {
				e = m.newRun(k, l.idx, l.src, ref)
				m.runs[k] = e
			}
			e.ref, e.gone = ref, false
		}
		for k, e := range m.runs {
			if e.idx == l.idx && !seen[k] {
				e.gone = true
			}
		}
	}
	m.sortRuns()
	if !m.hasSel {
		m.chooseInitial()
	}

	var cmds []tea.Cmd
	started := 0
	for _, k := range m.order {
		e := m.runs[k]
		if e.gone || e.busy {
			continue
		}
		isSel := m.hasSel && k == m.selected
		if !isSel && started >= deskPrefetch {
			continue
		}
		if !e.loaded || e.ref.Updated.After(e.loadedAt) || e.loadedAt.IsZero() {
			if c := m.loadCmd(e); c != nil {
				cmds = append(cmds, c)
				if !isSel {
					started++
				}
			}
		}
	}
	return m, tea.Batch(cmds...)
}

// newRun builds a run's entry: its spec, a cursor, and an empty folder.
func (m deskModel) newRun(k deskRunKey, idx int, src runview.Source, ref runview.RunRef) *deskRun {
	e := &deskRun{key: k, src: src, idx: idx, ref: ref, cur: &runview.Cursor{}}
	e.spec = m.specFor(src, ref)
	e.folder = runview.NewFolder(e.spec, nil)
	return e
}

// specFor is the spec a run folds with: the desk's for a desk run, else the
// source's own, else an empty spec that keeps events and shows no step state.
func (m deskModel) specFor(src runview.Source, ref runview.RunRef) *runview.Spec {
	if ref.Kind == runview.KindDesk {
		return runview.DeskSpec()
	}
	if s, ok := src.(interface{ Spec() *runview.Spec }); ok {
		if sp := s.Spec(); sp != nil {
			return sp
		}
	}
	if sp := m.specs[src.Name()]; sp != nil {
		return sp
	}
	return &runview.Spec{}
}

// chooseInitial picks the view and run the dashboard opens on: where the
// previous call left off if that run is still listed, else the queue when
// items wait, else the newest live run.
func (m *deskModel) chooseInitial() {
	if r := m.restore; r != nil {
		if k := (deskRunKey{r.Source, r.Run}); r.Run != "" && m.runs[k] != nil {
			m.selectKey(k)
			return
		}
	}
	if len(m.order) > 0 {
		m.selectKey(m.order[0])
	}
	if m.restore == nil && m.waiting() > 0 {
		m.view, m.focus = viewQueue, paneSide
	}
}

// onLoad applies a finished load to its run.
func (m deskModel) onLoad(msg deskLoadMsg) (tea.Model, tea.Cmd) {
	e := m.runs[msg.key]
	if e == nil {
		return m, nil
	}
	e.busy = false
	e.loaded = true
	e.loadErr = msg.err
	if msg.err != nil {
		return m, nil
	}
	m.apply(e, msg.d)
	m.sortRuns()
	return m, nil
}

// apply folds a delta into e. The fold runs only when events arrived, the
// log was replaced, or the step definitions changed.
func (m *deskModel) apply(e *deskRun, d runview.Delta) {
	changed := false
	if d.Reset {
		e.events, e.metrics = nil, nil
		e.folder = runview.NewFolder(e.spec, e.defs)
		e.note, e.noteUntil = "log restarted", m.now().Add(deskNoteFor)
		e.everEvents = false
		if e.key == m.selected {
			m.pausedAt = 0
		}
		changed = true
	}
	if d.Defs != nil && !reflect.DeepEqual(d.Defs, e.defs) {
		e.defs = d.Defs
		e.folder = runview.NewFolder(e.spec, e.defs)
		e.folder.Append(e.events...)
		changed = true
	}
	if len(d.Events) > 0 {
		e.events = append(e.events, d.Events...)
		e.folder.Append(d.Events...)
		e.everEvents = true
		changed = true
	}
	e.metrics = append(e.metrics, d.Metrics...)
	if d.Summary != nil {
		e.summary = d.Summary
	}
	if d.Timing != nil {
		e.timing = d.Timing
	}
	if d.Live != "" {
		e.live = d.Live
	}
	e.err, e.dropped, e.fields, e.partial = d.Err, d.Dropped, d.DroppedFields, d.Partial
	if changed || !e.loaded || e.state.Steps == nil {
		e.state = e.folder.At(e.folder.Len(), m.now())
	}
}

// sortRuns orders the runs: live first, then newest activity first.
func (m *deskModel) sortRuns() {
	m.order = make([]deskRunKey, 0, len(m.runs))
	for k := range m.runs {
		m.order = append(m.order, k)
	}
	now := m.now()
	slices.SortFunc(m.order, func(a, b deskRunKey) int {
		ea, eb := m.runs[a], m.runs[b]
		if la, lb := m.isLive(ea, now), m.isLive(eb, now); la != lb {
			if la {
				return -1
			}
			return 1
		}
		if ta, tb := ea.activity(), eb.activity(); !ta.Equal(tb) {
			if ta.After(tb) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name+"\x00"+a.source, b.name+"\x00"+b.source)
	})
}

// activity is when the run last did anything: its last event, else the time
// its source last saw it change.
func (e *deskRun) activity() time.Time {
	if !e.state.LastEvent.IsZero() {
		return e.state.LastEvent
	}
	return e.ref.Updated
}

func (m deskModel) isLive(e *deskRun, now time.Time) bool {
	switch m.liveState(e, now) {
	case runview.LiveLive, runview.LiveRunning:
		return true
	}
	return false
}

// liveState is the run's state as of now. A spec run's staleness is judged
// here, against the clock, so the fold need not run when only time passed.
func (m deskModel) liveState(e *deskRun, now time.Time) runview.LiveState {
	if e.live != "" {
		return e.live
	}
	if !e.loaded {
		return ""
	}
	s := e.state.Live
	if s == runview.LiveLive && e.spec != nil && e.spec.Live.StaleAfter > 0 {
		last := e.state.LastEvent
		if last.IsZero() || now.Sub(last) > e.spec.Live.StaleAfter {
			return runview.LiveStale
		}
	}
	return s
}

// sel is the selected run, or nil.
func (m deskModel) sel() *deskRun {
	if !m.hasSel {
		return nil
	}
	return m.runs[m.selected]
}

// waiting counts the queue items waiting for approval.
func (m deskModel) waiting() int {
	n := 0
	for _, it := range m.items {
		if it.State == "waiting" {
			n++
		}
	}
	return n
}

func (m deskModel) lostItems() int {
	n := 0
	for _, it := range m.items {
		if it.State == "lost" {
			n++
		}
	}
	return n
}

// selectKey selects a run and resets the per-run view state.
func (m *deskModel) selectKey(k deskRunKey) {
	m.selected, m.hasSel = k, true
	for v := range m.top {
		m.top[v], m.cur[v] = 0, 0
	}
	m.follow[viewFlow], m.follow[viewEvents] = true, true
	m.pausedAt = 0
}

// visibleOrder is the run order the list shows: filtered when the list is the
// filter's target.
func (m deskModel) visibleOrder() []deskRunKey {
	if len(m.filter) == 0 || !m.filterTargetsList() {
		return m.order
	}
	needle := strings.ToLower(string(m.filter))
	now := m.now()
	var out []deskRunKey
	for _, k := range m.order {
		hay := strings.ToLower(k.name + "\x00" + m.runWord(m.runs[k], now))
		if strings.Contains(hay, needle) {
			out = append(out, k)
		}
	}
	return out
}

// filterTargetsList reports whether `/` filters the run list (when it is on
// screen) rather than the events.
func (m deskModel) filterTargetsList() bool {
	return m.layout().kind == layoutWide
}

func (m *deskModel) typeFilter(s string) {
	for _, r := range s {
		if len(m.filter) >= cockpitFilterMax {
			break
		}
		if termsafe.IsUnsafeTerminalRune(r) {
			continue
		}
		m.filter = append(m.filter, r)
	}
	m.cur[viewEvents], m.top[viewEvents] = 0, 0
}

// --- keys ---

func (m deskModel) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if binding.Matches(k, m.keys.ForceQuit) {
		return m, tea.Quit
	}
	m.footer = ""
	switch {
	case m.showHelp:
		return m.onHelpKey(k)
	case m.filtering:
		return m.onFilterKey(k)
	}
	return m.onBrowseKey(k)
}

func (m deskModel) onHelpKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case binding.Matches(k, m.keys.Help, m.keys.Back):
		m.showHelp, m.helpTop = false, 0
	case binding.Matches(k, m.keys.Quit):
		return m, tea.Quit
	case binding.Matches(k, m.keys.Down):
		m.helpTop++
	case binding.Matches(k, m.keys.Up):
		m.helpTop = max(m.helpTop-1, 0)
	case binding.Matches(k, m.keys.Top):
		m.helpTop = 0
	}
	return m, nil
}

func (m deskModel) onFilterKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "enter":
		m.filtering = false
	case "esc":
		m.filtering = false
		m.filter = nil
	case "backspace":
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
		}
	default:
		if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			m.typeFilter(k.Text)
		}
	}
	return m, nil
}

func (m deskModel) onBrowseKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if s := k.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '6' {
		m.setView(int(s[0] - '0'))
		return m, nil
	}
	switch {
	case binding.Matches(k, m.keys.Quit):
		return m, tea.Quit
	case binding.Matches(k, m.keys.Back):
		if len(m.filter) > 0 {
			m.filter = nil
		}
		return m, nil
	case binding.Matches(k, m.keys.Help):
		m.showHelp = true
		return m, nil
	case binding.Matches(k, m.keys.Filter):
		m.filtering = true
		return m, nil
	case binding.Matches(k, m.keys.NextPane):
		m.nextPane()
		return m, nil
	case binding.Matches(k, m.keys.PrevRun):
		return m.stepRun(-1)
	case binding.Matches(k, m.keys.NextRun):
		return m.stepRun(1)
	case binding.Matches(k, m.keys.Up):
		m.move(-1)
		return m, nil
	case binding.Matches(k, m.keys.Down):
		m.move(1)
		return m, nil
	case binding.Matches(k, m.keys.Top):
		m.jump(false)
		return m, nil
	case binding.Matches(k, m.keys.Bottom):
		m.jump(true)
		return m, nil
	case binding.Matches(k, m.keys.Mouse):
		m.mouseOff = !m.mouseOff
		if m.mouseOff {
			m.footer = m.styles.Muted.Render("mouse capture off: drag to select text (m turns it on)")
		} else {
			m.footer = m.styles.Muted.Render("mouse capture on")
		}
		return m, nil
	case binding.Matches(k, m.keys.Refresh):
		return m.refreshSelected()
	case binding.Matches(k, m.keys.RefreshAll):
		return m.refreshAll()
	case binding.Matches(k, m.keys.Approve):
		return m.verb("approve")
	case binding.Matches(k, m.keys.Skip):
		return m.verb("skip")
	case binding.Matches(k, m.keys.Unskip):
		return m.verb("unskip")
	case binding.Matches(k, m.keys.Open):
		return m.open()
	case binding.Matches(k, m.keys.Switcher):
		if m.hooks.switcher != nil {
			return m.hooks.switcher(m)
		}
	case binding.Matches(k, m.keys.Replay):
		if m.hooks.replay != nil {
			return m.hooks.replay(m)
		}
	}
	return m, nil
}

// verb is a queue key. Outside the queue view it says where the verb lives;
// inside, it is the hook's.
func (m deskModel) verb(name string) (tea.Model, tea.Cmd) {
	if m.sideShows() != viewQueue {
		m.footer = m.styles.Muted.Render(name + " is in the queue (6)")
		return m, nil
	}
	if m.hooks.verb != nil {
		return m.hooks.verb(m, name)
	}
	return m, nil
}

func (m deskModel) open() (tea.Model, tea.Cmd) {
	if m.hooks.open != nil {
		return m.hooks.open(m)
	}
	return m, nil
}

// setView picks a view and moves focus to the pane that shows it.
func (m *deskModel) setView(v int) {
	m.view = v
	if m.layout().kind == layoutSingle {
		return
	}
	if v == viewFlow {
		m.focus = paneFlow
	} else {
		m.focus = paneSide
	}
}

// nextPane moves focus to the next pane on screen.
func (m *deskModel) nextPane() {
	switch m.layout().kind {
	case layoutWide:
		m.focus = (m.focus + 1) % 3
	case layoutMid:
		if m.focus == paneFlow {
			m.focus = paneSide
		} else {
			m.focus = paneFlow
		}
	}
}

// focusedView is the view whose body the focus is on: 0 for the run list.
func (m deskModel) focusedView() int {
	switch m.layout().kind {
	case layoutSingle:
		return m.view
	case layoutWide:
		if m.focus == paneList {
			return 0
		}
	}
	if m.focus == paneFlow {
		return viewFlow
	}
	return m.sideShows()
}

// sideShows is the view in the side pane. Flow has its own pane, so with the
// flow view picked the side pane shows events.
func (m deskModel) sideShows() int {
	if m.layout().kind == layoutSingle {
		return m.view
	}
	if m.view == viewFlow {
		return viewEvents
	}
	return m.view
}

// stepRun selects the previous or next run in the list.
func (m deskModel) stepRun(delta int) (tea.Model, tea.Cmd) {
	order := m.visibleOrder()
	if len(order) == 0 {
		return m, nil
	}
	i := slices.Index(order, m.selected)
	switch {
	case i < 0:
		i = 0
	default:
		i = min(max(i+delta, 0), len(order)-1)
	}
	if order[i] == m.selected && m.hasSel {
		return m, nil
	}
	m.selectKey(order[i])
	return m, m.loadCmd(m.sel())
}

// move is a down or up key in the focused pane.
func (m *deskModel) move(delta int) {
	v := m.focusedView()
	switch v {
	case 0:
		order := m.visibleOrder()
		if len(order) == 0 {
			return
		}
		i := slices.Index(order, m.selected)
		if i < 0 {
			i = 0
		} else {
			i = min(max(i+delta, 0), len(order)-1)
		}
		if order[i] != m.selected || !m.hasSel {
			m.selectKey(order[i])
		}
	case viewFlow, viewEvents, viewQueue:
		n := m.rowCount(v)
		m.cur[v] = min(max(m.cur[v]+delta, 0), max(n-1, 0))
		if delta < 0 {
			m.pause(v)
		}
	default:
		m.top[v] = max(m.top[v]+delta, 0)
	}
}

// pause stops following in a view that follows, remembering how many events
// the run has so the footer can count the new ones.
func (m *deskModel) pause(v int) {
	if v != viewFlow && v != viewEvents {
		return
	}
	if m.follow[v] && v == viewEvents {
		if e := m.sel(); e != nil {
			m.pausedAt = len(e.events)
		}
	}
	m.follow[v] = false
}

// jump goes to the top or bottom of the focused pane; the bottom also
// resumes following.
func (m *deskModel) jump(bottom bool) {
	v := m.focusedView()
	if v == 0 {
		order := m.visibleOrder()
		if len(order) == 0 {
			return
		}
		k := order[0]
		if bottom {
			k = order[len(order)-1]
		}
		if k != m.selected || !m.hasSel {
			m.selectKey(k)
		}
		return
	}
	n := m.rowCount(v)
	switch v {
	case viewFlow, viewEvents, viewQueue:
		if bottom {
			m.cur[v] = max(n-1, 0)
			if v == viewFlow || v == viewEvents {
				m.follow[v] = true
			}
		} else {
			m.cur[v] = 0
			m.pause(v)
		}
	default:
		if bottom {
			m.top[v] = 1 << 30 // clamped by syncScroll
		} else {
			m.top[v] = 0
		}
	}
}

// refreshSelected reloads the selected run, at most once every deskRefreshGap.
func (m deskModel) refreshSelected() (tea.Model, tea.Cmd) {
	e := m.sel()
	if e == nil {
		return m, nil
	}
	now := m.now()
	if !e.refreshedAt.IsZero() {
		if wait := deskRefreshGap - now.Sub(e.refreshedAt); wait > 0 {
			m.footer = m.styles.Warn.Render(fmt.Sprintf("! refreshed moments ago; try again in %ds", int((wait+time.Second-1)/time.Second)))
			return m, nil
		}
	}
	if e.busy {
		m.footer = m.styles.Warn.Render("! a refresh is already running")
		return m, nil
	}
	e.refreshedAt = now
	m.footer = m.styles.Muted.Render("refreshing " + capSafe(e.key.name, 48) + "…")
	return m, m.loadCmd(e)
}

// refreshAll relists every source and reloads the runs, at most once every
// deskRefreshGap.
func (m deskModel) refreshAll() (tea.Model, tea.Cmd) {
	now := m.now()
	if !m.lastAll.IsZero() {
		if wait := deskRefreshGap - now.Sub(m.lastAll); wait > 0 {
			m.footer = m.styles.Warn.Render(fmt.Sprintf("! refreshed moments ago; try again in %ds", int((wait+time.Second-1)/time.Second)))
			return m, nil
		}
	}
	if m.listBusy {
		m.footer = m.styles.Warn.Render("! a refresh is already running")
		return m, nil
	}
	m.lastAll = now
	for _, e := range m.runs {
		e.loadedAt = time.Time{}
	}
	m.listBusy = true
	m.footer = m.styles.Muted.Render("refreshing everything…")
	cmds := []tea.Cmd{m.listCmd()}
	if c := m.loadCmd(m.sel()); c != nil {
		cmds = append(cmds, c)
	}
	return m, tea.Batch(cmds...)
}

// --- mouse ---

func (m deskModel) onWheel(ms tea.Mouse) deskModel {
	pane, _, ok := m.hit(ms.X, ms.Y)
	if !ok {
		return m
	}
	v := m.viewOfPane(pane)
	delta := 0
	switch ms.Button {
	case tea.MouseWheelUp:
		delta = -3
	case tea.MouseWheelDown:
		delta = 3
	}
	if delta == 0 {
		return m
	}
	if v == 0 {
		m.listTop = max(m.listTop+delta, 0)
		return m
	}
	m.top[v] = max(m.top[v]+delta, 0)
	if delta < 0 {
		m.pause(v)
	}
	m.pullCursor(v)
	return m
}

// pullCursor moves view v's cursor onto a visible row after the view was
// scrolled with the wheel, so settling does not scroll it back.
func (m *deskModel) pullCursor(v int) {
	b := m.bodyFor(v)
	if b.rowOfLine == nil || b.n == 0 {
		return
	}
	_, ih := m.paneInner(v)
	m.top[v] = min(max(m.top[v], 0), max(b.n-ih, 0))
	lo, hi := m.top[v], min(m.top[v]+ih, b.n)-1
	switch {
	case b.cursorLine < lo:
		for i := lo; i <= hi; i++ {
			if row, ok := b.rowOfLine(i); ok {
				m.cur[v] = row
				return
			}
		}
	case b.cursorLine > hi:
		for i := hi; i >= lo; i-- {
			if row, ok := b.rowOfLine(i); ok {
				m.cur[v] = row
				return
			}
		}
	}
}

func (m deskModel) onClick(ms tea.Mouse) (tea.Model, tea.Cmd) {
	if ms.Button != tea.MouseLeft || m.showHelp {
		return m, nil
	}
	if v, ok := m.tabAt(ms.X, ms.Y); ok {
		m.setView(v)
		return m, nil
	}
	pane, line, ok := m.hit(ms.X, ms.Y)
	if !ok {
		return m, nil
	}
	m.focus = pane
	v := m.viewOfPane(pane)
	if v == 0 {
		order := m.visibleOrder()
		i := m.listTop + line
		if i >= 0 && i < len(order) && order[i] != m.selected {
			m.selectKey(order[i])
			return m, m.loadCmd(m.sel())
		}
		return m, nil
	}
	row, ok := m.rowAtLine(v, m.top[v]+line)
	if !ok {
		return m, nil
	}
	if m.cur[v] == row {
		return m.open() // a second click on the selected row
	}
	m.cur[v] = row
	m.pause(v)
	return m, nil
}

// viewOfPane is the view a pane shows: 0 for the run list.
func (m deskModel) viewOfPane(pane int) int {
	switch {
	case m.layout().kind == layoutSingle:
		return m.view
	case pane == paneList:
		return 0
	case pane == paneFlow:
		return viewFlow
	}
	return m.sideShows()
}

// errNoLog reports whether err means the run has no log file yet.
func errNoLog(err error) bool { return err != nil && errors.Is(err, fs.ErrNotExist) }
