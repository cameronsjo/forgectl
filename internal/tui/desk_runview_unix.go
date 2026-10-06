// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The run view is the dashboard's visualizer (ADR-0013): one run, full
// screen, as the flow of its steps above the timeline of its events, with
// replay. r opens it on the selected item's run. It follows the run live
// until the operator steps back through the events.

// deskRunPlayEvery is how fast space plays a replay forward, one event per
// step.
const deskRunPlayEvery = 300 * time.Millisecond

// deskRunJump is how far [ and ] move the replay point.
const deskRunJump = 10

// deskRunView is the open run view. at is how many events are folded into
// the state shown; follow keeps it at the newest event as the run grows.
type deskRunView struct {
	refs []runview.RunRef // the runs n and p move through, as the source listed them
	idx  int              // the run shown, in refs

	cur    *runview.Cursor
	folder *runview.Folder
	defs   []runview.StepDef
	delta  runview.Delta // the latest load's defs, timing, live state and counts
	gen    int           // the model's runGen when this view opened, so a stale load is dropped
	// playSeq changes whenever play starts or stops, so a tick from an
	// earlier play is dropped and a pause and resume never run two chains.
	playSeq int

	at      int
	follow  bool
	playing bool
	loading bool
	loaded  bool
	// gone is set once the run is no longer in the desk, so it is not polled.
	gone bool
	// want is the run r was pressed on.
	want string
	err  error
}

// deskRunLoadMsg is one load of the run view's run. refs is set when the load
// listed the runs first.
type deskRunLoadMsg struct {
	gen   int
	refs  []runview.RunRef
	ref   runview.RunRef
	cur   *runview.Cursor
	delta runview.Delta
	err   error
}

// deskRunPlayMsg advances a playing replay by one event.
type deskRunPlayMsg struct{ gen, seq int }

func (v *deskRunView) ref() runview.RunRef {
	if v.idx < 0 || v.idx >= len(v.refs) {
		return runview.RunRef{}
	}
	return v.refs[v.idx]
}

// state is the run as shown: the fold at the replay point.
func (v *deskRunView) state() runview.RunState {
	if v.folder == nil {
		return runview.RunState{}
	}
	return v.folder.At(v.at)
}

// live is the run's state word source: the source's own word while
// following, the fold's in a replay. A log has no step model, so it stays
// unknown either way.
func (v *deskRunView) live(s runview.RunState) runview.LiveState {
	if v.delta.Live != "" && (v.follow || v.delta.Live == runview.LiveUnknown) {
		return v.delta.Live
	}
	return s.Live
}

// runEnded reports a run that is over, however it ended; a run the desk no
// longer has is not one of them, since it may have been running.
func runEnded(live runview.LiveState) bool {
	switch live {
	case runview.LiveEnded, runview.LiveSkipped, runview.LiveChanged, runview.LiveLost:
		return true
	}
	return false
}

// polls reports whether the run can still change, so the view reloads it on
// each tick.
func (v *deskRunView) polls() bool {
	if v.gone {
		return false
	}
	return !runEnded(v.delta.Live)
}

// openRunView opens the run view on the selected item's run, or on the
// newest run when nothing is selected. A selected item with no run (waiting
// or refused) says so on the dashboard instead of opening another item's
// run, which would read as its own (#1106).
func (m deskModel) openRunView() (tea.Model, tea.Cmd) {
	if m.runs == nil {
		m.message = m.styles().Warn.Render("the run view is not available here")
		return m, nil
	}
	name := ""
	if r, ok := m.selected(); ok {
		if r.kind == rowWaiting || r.kind == rowRefused {
			m.message = m.styles().Muted.Render(safeMessage(itemLabel(r.item.Name) + " has not run yet · r opens its run once it starts; move to a run to see it"))
			return m, nil
		}
		name = r.item.Name
	}
	m.runGen++
	m.rv = &deskRunView{gen: m.runGen, want: name, follow: true, loading: true}
	return m, loadRunCmd(m.runs, m.rv.gen, name, runview.RunRef{}, nil)
}

// loadRunCmd loads a run off the update loop. With no ref it lists the runs
// first and picks name, or the first run when name is "" or gone; a fresh
// cursor rides back on the message, so the view is changed only in Update.
func loadRunCmd(src runview.Source, gen int, name string, ref runview.RunRef, cur *runview.Cursor) tea.Cmd {
	return func() tea.Msg {
		msg := deskRunLoadMsg{gen: gen, ref: ref, cur: cur}
		if ref.Name == "" {
			refs, err := src.List()
			if err != nil {
				msg.err = err
				return msg
			}
			msg.refs = refs
			if len(refs) == 0 {
				msg.refs = []runview.RunRef{} // listed, and empty: not "not listed"
				return msg
			}
			msg.ref = refs[0]
			if i := slices.IndexFunc(refs, func(r runview.RunRef) bool { return r.Name == name }); i >= 0 {
				msg.ref = refs[i]
			}
		}
		if msg.cur == nil {
			msg.cur = &runview.Cursor{}
		}
		msg.delta, msg.err = src.Load(msg.ref, msg.cur)
		return msg
	}
}

// applyRunLoad takes a load into the open view. A load for a view that has
// since switched runs or closed is dropped.
func (m deskModel) applyRunLoad(t deskRunLoadMsg) (tea.Model, tea.Cmd) {
	v := m.rv
	if v == nil || t.gen != v.gen {
		return m, nil
	}
	if t.refs == nil && t.ref.Name != v.ref().Name {
		return m, nil // a load for another run than the one shown
	}
	v.loading = false
	if t.refs != nil {
		v.refs = t.refs
		v.idx = slices.IndexFunc(t.refs, func(r runview.RunRef) bool { return r.Name == t.ref.Name })
		if v.want != "" && t.ref.Name != v.want {
			// The selected item has no run (gone since the dashboard's scan):
			// say so rather than show another item's run as its own (#1106).
			m.rv = nil
			m.message = m.styles().Muted.Render(safeMessage(itemLabel(v.want) + " has no run to show"))
			return m, nil
		}
		if len(t.refs) == 0 {
			v.err = fmt.Errorf("no runs yet: nothing has started")
			return m, nil
		}
	}
	if t.err != nil {
		v.err = t.err
		v.gone = errors.Is(t.err, desk.ErrNotFound)
		return m, nil
	}
	v.err = t.delta.Err
	v.cur = t.cur
	switch {
	case v.folder == nil || t.delta.Reset:
		v.defs = t.delta.Defs
		v.folder = newRunFolder(t.ref, v.defs, nil)
	case !sameDefs(v.defs, t.delta.Defs) && t.delta.Defs != nil:
		// The manifest arrived late, or changed: fold everything again.
		v.defs = t.delta.Defs
		v.folder = newRunFolder(t.ref, v.defs, v.folder.Events())
	}
	v.folder.Append(t.delta.Events...)
	v.delta = t.delta
	v.loaded = true
	if v.follow {
		v.at = v.folder.Len()
	}
	// A reset or a shorter refold can leave a replay point past the events.
	v.at = min(v.at, v.folder.Len())
	v.follow = v.follow || v.at == v.folder.Len() // clamped to the tip: follow again
	return m, nil
}

func newRunFolder(ref runview.RunRef, defs []runview.StepDef, events []runview.Event) *runview.Folder {
	spec := &runview.Spec{}
	if ref.Kind == runview.KindDesk {
		spec = runview.DeskSpec()
	}
	f := runview.NewFolder(spec, defs)
	f.Append(events...)
	return f
}

func sameDefs(a, b []runview.StepDef) bool {
	return slices.EqualFunc(a, b, func(x, y runview.StepDef) bool {
		return x.ID == y.ID && slices.Equal(x.After, y.After)
	})
}

// pollRun reloads the open view's run when it can still change and no load
// is on its way.
func (m deskModel) pollRun() tea.Cmd {
	v := m.rv
	if v == nil || v.loading || !v.polls() {
		return nil
	}
	if !v.loaded {
		if v.err == nil {
			return nil // the first load is still on its way
		}
		v.loading = true
		if ref := v.ref(); ref.Name != "" {
			// The first load of a known run failed: try that run again, from
			// the start, rather than jump to another.
			return loadRunCmd(m.runs, v.gen, "", ref, nil)
		}
		// The listing failed or found no runs: list again, since a run may
		// have started.
		return loadRunCmd(m.runs, v.gen, v.want, runview.RunRef{}, nil)
	}
	v.loading = true
	return loadRunCmd(m.runs, v.gen, "", v.ref(), v.cur)
}

// switchRun moves to the run step places away in the listing.
func (m deskModel) switchRun(step int) (tea.Model, tea.Cmd) {
	v := m.rv
	if len(v.refs) < 2 {
		return m, nil
	}
	idx := (v.idx + step + len(v.refs)) % len(v.refs)
	m.runGen++
	m.rv = &deskRunView{refs: v.refs, idx: idx, gen: m.runGen, follow: true, loading: true}
	return m, loadRunCmd(m.runs, m.rv.gen, "", m.rv.ref(), nil)
}

func (m deskModel) runViewKey(key string) (tea.Model, tea.Cmd) {
	v := m.rv
	n := 0
	if v.folder != nil {
		n = v.folder.Len()
	}
	seek := func(at int) {
		v.at = min(max(at, 0), n)
		v.follow = v.at == n
		v.stopPlay()
	}
	switch key {
	case "q", "esc", "r":
		m.rv = nil
	case "left", "h":
		seek(v.at - 1)
	case "right", "l":
		seek(v.at + 1)
	case "[":
		seek(v.at - deskRunJump)
	case "]":
		seek(v.at + deskRunJump)
	case "g", "home":
		seek(0)
	case "G", "end":
		seek(n)
	case "space":
		if v.playing {
			v.stopPlay()
			return m, nil
		}
		if n == 0 {
			return m, nil
		}
		if v.at >= n {
			v.at = 0
		}
		v.playing, v.follow = true, false
		v.playSeq++
		return m, playRunCmd(v.gen, v.playSeq)
	case "n":
		return m.switchRun(1)
	case "p":
		return m.switchRun(-1)
	}
	return m, nil
}

func playRunCmd(gen, seq int) tea.Cmd {
	return tea.Tick(deskRunPlayEvery, func(time.Time) tea.Msg { return deskRunPlayMsg{gen: gen, seq: seq} })
}

// stopPlay stops a playing replay; its pending tick no longer matches.
func (v *deskRunView) stopPlay() {
	if v.playing {
		v.playing = false
		v.playSeq++
	}
}

// playStep advances a playing replay one event; at the newest event it stops
// and follows the run again.
func (m deskModel) playStep(t deskRunPlayMsg) (tea.Model, tea.Cmd) {
	v := m.rv
	if v == nil || t.gen != v.gen || t.seq != v.playSeq || !v.playing || v.folder == nil {
		return m, nil
	}
	v.at++
	if v.at >= v.folder.Len() {
		v.at, v.follow = v.folder.Len(), true
		v.stopPlay()
		return m, nil
	}
	return m, playRunCmd(v.gen, v.playSeq)
}

// runToneStyle maps a mark's tone to the theme's style for it.
func runToneStyle(st theme.Styles, t runview.Tone) lipgloss.Style {
	switch t {
	case runview.ToneActive:
		return st.Active
	case runview.ToneOK:
		return st.OK
	case runview.ToneDanger:
		return st.Danger
	case runview.ToneWarn:
		return st.Warn
	case runview.ToneMuted:
		return st.Muted
	}
	return st.Dim
}

// render draws the run view at width x height: a header, the flow, the
// events around the replay point, and the key hints.
func (v *deskRunView) render(st theme.Styles, width, height int) string {
	if width <= 0 {
		width = 80
	}
	height = max(height, 3)
	g := runview.IconGlyphs
	ref := v.ref()
	s := v.state()
	n := 0
	if v.folder != nil {
		n = v.folder.Len()
	}

	head := st.Header.Render("run")
	finished := v.loaded && runEnded(v.delta.Live)
	if ref.Name != "" {
		head += " " + st.Fg.Render(itemLabel(ref.Name))
		if ref.Kind == runview.KindLog {
			head = st.Header.Render("log") + " " + st.Fg.Render(ref.Name)
		}
		// A finished run's header shows how it finished, even mid-replay:
		// the fold at the replay point would call it live (#1106).
		live, exit := v.live(s), s.Exit
		if finished {
			live, exit = v.delta.Live, v.folder.At(n).Exit
		}
		mk := runview.RunMark(g, live, exit)
		head += "  " + runToneStyle(st, mk.Tone).Render(mk.Glyph+" "+mk.Word)
	}
	switch {
	case v.playing:
		head += st.Accent.Render(fmt.Sprintf("  ▶ %d/%d", v.at, n))
	case !v.follow && finished:
		head += st.Warn.Render(fmt.Sprintf("  replaying %d/%d · G end", v.at, n))
	case !v.follow:
		head += st.Warn.Render(fmt.Sprintf("  replay %d/%d · G live", v.at, n))
	case v.loaded:
		head += st.Muted.Render(fmt.Sprintf("  %d events", n))
	}
	if len(v.refs) > 1 {
		head += st.Muted.Render(fmt.Sprintf("  run %d of %d", v.idx+1, len(v.refs)))
	}
	lines := []string{cut(head, width)}

	body := height - 3 // header, events rule, hints
	switch {
	case !v.loaded && v.err == nil:
		lines = append(lines, st.Muted.Render(" loading…"))
	case v.err != nil && !v.loaded:
		lines = append(lines, cut(" "+st.Danger.Render(safeMessage(v.err.Error())), width))
	default:
		flowRows := max(min(body/2, 12), 1)
		flow := runFlow(st, g, s, v.timing(), width, flowRows)
		lines = append(lines, flow...)
		rule := "events"
		if c := v.counts(s); c != "" {
			rule += " · " + c
		}
		if v.err != nil {
			rule += " · " + safeMessage(v.err.Error())
		}
		lines = append(lines, cut(st.Muted.Render(" "+rule), width))
		events := runEvents(st, v.folder, v.at, width, max(body-len(flow), 1))
		if n == 0 {
			events = []string{st.Muted.Render(" " + noEventsText(v.delta.Live))}
		}
		lines = append(lines, events...)
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = lines[:height-1]
	return strings.Join(append(lines, cut(st.Muted.Render(runHintsFor(width, finished)), width)), "\n")
}

// noEventsText says why a run shows no events: one that never ran says so,
// in the word the queue uses; one that may still start is waiting for them.
func noEventsText(live runview.LiveState) string {
	switch live {
	case runview.LiveChanged:
		return "never ran: it changed after it was queued"
	case runview.LiveSkipped:
		return "never ran: skipped before it started"
	}
	return "no events yet"
}

// runHintsFor is the widest key-hint line that fits, so "q close" is never
// the part cut off. G goes to a finished run's end, or back to a live run's
// newest event.
func runHintsFor(width int, finished bool) string {
	g := "g/G start/live"
	if finished {
		g = "g/G start/end"
	}
	for _, h := range []string{
		" ←/→ step · [ ] ±10 · " + g + " · space play · n/p run · q close",
		" ←/→ step · " + g + " · space play · n/p run · q close",
		" ←/→ · g/G · space · n/p · q close",
	} {
		if ansi.StringWidth(h) <= width {
			return h
		}
	}
	return " q close"
}

// timing is the runner's step state, which describes the run now: shown only
// while following.
func (v *deskRunView) timing() []runview.StepTiming {
	if !v.follow {
		return nil
	}
	return v.delta.Timing
}

// counts is what the events rule says was left out.
func (v *deskRunView) counts(s runview.RunState) string {
	var parts []string
	for _, c := range []struct {
		n         int
		one, many string
	}{
		{v.delta.Dropped, "line dropped", "lines dropped"},
		{s.UnknownSteps, "unknown step", "unknown steps"},
	} {
		switch {
		case c.n == 1:
			parts = append(parts, "1 "+c.one)
		case c.n > 1:
			parts = append(parts, strconv.Itoa(c.n)+" "+c.many)
		}
	}
	if v.delta.Partial {
		parts = append(parts, "partial: past the 32 MiB cap")
	}
	return strings.Join(parts, " · ")
}

// runFlow draws the steps as a flow: columns by depth, left to right, when
// they fit the width, else one step per line indented by depth. Each step is
// its mark, id and duration. At most rows lines; the rest are counted.
func runFlow(st theme.Styles, g runview.Glyphs, s runview.RunState, timing []runview.StepTiming, width, rows int) []string {
	if len(s.Steps) == 0 {
		return []string{st.Muted.Render(" no step model: this run shows its events only")}
	}
	depth := stepDepths(s)
	cell := func(st0 runview.StepState) (text string, mk runview.Mark) {
		mk = runview.StepMark(g, st0.Status, runview.RunnerState(timing, st0.ID))
		text = mk.Glyph + " " + st0.ID
		if d := runview.StepDur(timing, st0); d > 0 {
			text += " " + d.Round(100*time.Millisecond).String()
		}
		return text, mk
	}

	maxDepth := slices.Max(depth)
	cols := make([][]int, maxDepth+1)
	for i, d := range depth {
		cols[d] = append(cols[d], i)
	}
	colW := make([]int, len(cols))
	tall := 0
	for c, idxs := range cols {
		for _, i := range idxs {
			t, _ := cell(s.Steps[i])
			colW[c] = max(colW[c], ansi.StringWidth(t))
		}
		tall = max(tall, len(idxs))
	}
	const arrow = " → "
	total := 1
	for _, w := range colW {
		total += w + len([]rune(arrow))
	}
	if total <= width && tall <= rows {
		out := make([]string, tall)
		for r := range tall {
			line := " "
			for c, idxs := range cols {
				text, sep := "", "   "
				if r < len(idxs) {
					t, mk := cell(s.Steps[idxs[r]])
					text = runToneStyle(st, mk.Tone).Render(t)
					if r == 0 && c < len(cols)-1 {
						sep = st.Dim.Render(arrow)
					}
				}
				line += padTo(text, colW[c])
				if c < len(cols)-1 {
					line += sep
				}
			}
			out[r] = cut(line, width)
		}
		return out
	}

	after := map[string][]string{}
	for _, e := range s.Edges {
		after[e[1]] = append(after[e[1]], e[0])
	}
	var out []string
	for i, st0 := range s.Steps {
		if rows > 1 && len(out) == rows-1 && len(s.Steps)-i > 1 {
			out = append(out, st.Muted.Render(fmt.Sprintf(" … %d more steps", len(s.Steps)-i)))
			break
		}
		t, mk := cell(st0)
		line := " " + strings.Repeat("  ", min(depth[i], 6)) + runToneStyle(st, mk.Tone).Render(t) + st.Muted.Render(" "+mk.Word)
		if p := after[st0.ID]; len(p) > 1 {
			line += st.Dim.Render(" ← " + strings.Join(p, ", "))
		}
		out = append(out, cut(line, width))
		if len(out) == rows {
			break // one row: the first step, with no room to say how many more
		}
	}
	return out
}

// stepDepths is each step's column: 0 for a step with no predecessor, else
// one more than its deepest predecessor. Repeated passes settle it whatever
// order the steps are listed in; a depth never passes the step count, so a
// cycle (which a manifest refuses) cannot loop.
func stepDepths(s runview.RunState) []int {
	idx := make(map[string]int, len(s.Steps))
	for i, st := range s.Steps {
		idx[st.ID] = i
	}
	depth := make([]int, len(s.Steps))
	for range s.Steps { // at most one pass per step settles any acyclic order
		changed := false
		for _, e := range s.Edges {
			from, ok1 := idx[e[0]]
			to, ok2 := idx[e[1]]
			if ok1 && ok2 && depth[to] < depth[from]+1 && depth[from] < len(s.Steps) {
				depth[to] = depth[from] + 1
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return depth
}

// runEvents draws the events around the replay point: the folded ones in the
// normal color with the newest marked, the ones after it dimmed.
func runEvents(st theme.Styles, f *runview.Folder, at, width, rows int) []string {
	if f == nil || f.Len() == 0 {
		return []string{st.Muted.Render(" no events yet")}
	}
	evs := f.Events()
	end := min(max(at+rows/3, rows), len(evs))
	start := max(end-rows, 0)
	out := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		e := evs[i]
		mark, style := "  ", st.Fg
		switch {
		case i == at-1:
			mark, style = st.Accent.Render("▸ "), st.Fg
		case i >= at:
			style = st.Dim
		}
		out = append(out, cut(" "+mark+style.Render(runEventText(e)), width))
	}
	return out
}

// runEventText is one event as a timeline row. Every value was made inert
// and capped when it was read.
func runEventText(e runview.Event) string {
	parts := []string{fmt.Sprintf("#%-4d", e.Seq)}
	if !e.Time.IsZero() {
		parts = append(parts, e.Time.UTC().Format("15:04:05"))
	}
	parts = append(parts, e.Name)
	if e.Step != "" {
		parts = append(parts, e.Step)
	}
	for _, f := range e.Fields {
		if f.Key == "id" && e.Step != "" {
			continue
		}
		parts = append(parts, f.Key+"="+f.Value)
	}
	return strings.Join(parts, " ")
}
