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

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
)

// The bodies of the run list and of views 2 to 6. Every string that came from
// a file is escaped and capped here, at draw time, whatever the source did.

// deskBody is a pane's content before it is cut to the pane's height: n lines,
// some of them rows the cursor can sit on.
type deskBody struct {
	n          int
	line       func(i int) string
	rowOfLine  func(i int) (int, bool) // nil when no line is selectable
	rows       int
	cursorLine int // first line of the cursor row, or -1
	cursorH    int // lines in the cursor row
}

func textBodyFrom(lines []string) deskBody {
	return deskBody{n: len(lines), line: func(i int) string { return lines[i] }, cursorLine: -1}
}

// bodyFor builds the body of view v at the pane's current size.
func (m deskModel) bodyFor(v int) deskBody {
	iw, ih := m.paneInner(v)
	switch v {
	case viewFlow:
		return m.flowBody(iw, ih)
	case viewQueue:
		return m.queueBody(iw)
	}
	e := m.sel()
	if e == nil {
		return m.placeholderBody(iw)
	}
	switch v {
	case viewEvents:
		return m.eventsBody(e, iw)
	case viewTimeline:
		return m.timelineBody(e, iw)
	case viewMetrics:
		return m.metricsBody(e, iw)
	case viewSummary:
		return m.summaryBody(e)
	}
	return textBodyFrom(nil)
}

// placeholderBody is what a run view shows when no run is selected: loading,
// or the cause of there being none.
func (m deskModel) placeholderBody(_ int) deskBody {
	if !m.listed {
		return textBodyFrom([]string{m.styles.Muted.Render("loading runs…")})
	}
	if len(m.order) == 0 {
		return textBodyFrom(m.noRunsLines())
	}
	return textBodyFrom([]string{m.styles.Muted.Render("no run selected")})
}

// noRunsLines says why there are no runs, with the cause that applies.
func (m deskModel) noRunsLines() []string {
	st := m.styles
	if len(m.sources) == 0 && len(m.failed) == 0 {
		return []string{st.Muted.Render("no sources: pass --spec")}
	}
	var lines []string
	names := make([]string, 0, len(m.listErrs))
	for n := range m.listErrs {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		err := m.listErrs[n]
		if errors.Is(err, runview.ErrRoot) {
			lines = append(lines, st.Warn.Render(capSafe(strings.TrimPrefix(err.Error(), runview.ErrRoot.Error()+": "), 160)))
			continue
		}
		lines = append(lines, st.Danger.Render("cannot list "+capSafe(n, 48)+": "+capSafe(err.Error(), 240)))
	}
	if len(lines) > 0 {
		return lines
	}
	var pats []string
	for _, src := range m.sources {
		if sp := m.specOf(src); sp != nil && sp.Runs != "" {
			pats = append(pats, capSafe(sp.Runs, 96))
		}
	}
	if len(pats) > 0 {
		return []string{st.Muted.Render("no runs match " + strings.Join(pats, ", "))}
	}
	if len(m.failed) > 0 {
		return []string{st.Muted.Render("no runs: every source failed (? for detail)")}
	}
	return []string{st.Muted.Render("no runs yet")}
}

// specOf is a log source's spec, or nil.
func (m deskModel) specOf(src runview.Source) *runview.Spec {
	if s, ok := src.(runview.SpecProvider); ok {
		return s.Spec()
	}
	return nil
}

// --- run list ---

// runLook is a run's mark, word and style.
func (m deskModel) runLook(e *deskRun, now time.Time) (string, string, lipgloss.Style) {
	st := m.styles
	g := m.glyph
	switch {
	case e.gone:
		return g.Pending, "gone", st.Dim
	case e.loadErr != nil:
		return g.Failed, "error", st.Danger
	case !e.loaded:
		return g.Pending, "loading", st.Dim
	}
	live := m.liveState(e, now)
	switch live {
	case runview.LiveLive:
		return m.spin(), "live", st.Active
	case runview.LiveRunning:
		return m.spin(), "running", st.Active
	case runview.LiveEnded:
		switch {
		case e.state.Exit == nil:
			return g.Pending, "ended", st.Muted
		case *e.state.Exit == 0:
			return g.Done, "ok", st.OK
		}
		return g.Failed, strconv.Itoa(*e.state.Exit), st.Danger
	case runview.LiveStale:
		if errNoLog(e.err) && !e.everEvents {
			return g.Pending, "new", st.Dim
		}
		return g.Stale, "stale", st.Warn
	case runview.LiveLost:
		return g.Lost, "lost", st.Danger
	case runview.LiveWaiting:
		return g.Pending, "waiting", st.Dim
	case runview.LiveSkipped:
		return g.Skipped, "skipped", st.Dim
	}
	return g.Pending, "pending", st.Dim
}

func (m deskModel) runWord(e *deskRun, now time.Time) string {
	_, w, _ := m.runLook(e, now)
	return w
}

// runAge is the time since the run last did anything.
func (m deskModel) runAge(e *deskRun, now time.Time) string {
	a := e.activity()
	if a.IsZero() {
		return ""
	}
	return ageShort(now.Sub(a))
}

// listLines draws the run list from the scroll offset.
func (m deskModel) listLines(iw, ih int) []string {
	st := m.styles
	if !m.listed {
		return []string{st.Muted.Render("loading runs…")}
	}
	if len(m.order) == 0 {
		return m.noRunsLines()
	}
	order := m.visibleOrder()
	if len(order) == 0 {
		return []string{st.Muted.Render("no runs match the filter")}
	}
	dup := map[string]int{}
	for _, k := range m.order {
		dup[k.name]++
	}
	now := m.now()
	nameW := max(iw-15, 4)
	var lines []string
	for i := m.listTop; i < len(order) && i < m.listTop+ih; i++ {
		k := order[i]
		e := m.runs[k]
		g, word, sty := m.runLook(e, now)
		name := capSafe(k.name, 96)
		if dup[k.name] > 1 {
			name += "@" + capSafe(k.source, 24)
		}
		marker := " "
		nameSty := st.Fg
		if k == m.selected {
			marker, nameSty = ">", st.Selected
		}
		lines = append(lines, marker+sty.Render(g)+" "+sty.Render(padTo(word, 7))+" "+
			nameSty.Render(padTo(midCut(name, nameW), nameW))+" "+st.Muted.Render(padTo(m.runAge(e, now), 3)))
	}
	return lines
}

// --- events ---

// eventTag is the short word an event row carries.
func eventTag(spec *runview.Spec, ev runview.Event, dot string) string {
	for _, c := range spec.Checks {
		if c.Event == ev.Name {
			return "chk"
		}
	}
	switch spec.On[ev.Name] {
	case runview.ActionStart:
		return "start"
	case runview.ActionClose:
		return "ok"
	case runview.ActionFail:
		return "FAIL"
	case runview.ActionReject:
		return "rej"
	case runview.ActionRestart:
		return "rst"
	case runview.ActionEnd:
		return "end"
	}
	return dot
}

func (m deskModel) tagStyle(tag string) lipgloss.Style {
	st := m.styles
	switch tag {
	case "start":
		return st.Active
	case "ok":
		return st.OK
	case "FAIL":
		return st.Danger
	case "rej", "rst":
		return st.Warn
	case "end":
		return st.Fg
	case "chk":
		return st.Accent
	}
	return st.Dim
}

// eventFields is what an event row shows after its tag: the fields the spec
// asks for, in its order, or every field when it asks for none.
func eventFields(spec *runview.Spec, ev runview.Event) string {
	var parts []string
	// The step has its own column, so its field is not repeated.
	skip := func(key string) bool { return key == "" || key == spec.Log.Step }
	if len(spec.Log.Show) == 0 {
		for _, f := range ev.Fields {
			if !skip(f.Key) {
				parts = append(parts, capSafe(f.Key, 48)+"="+capSafe(f.Value, 96))
			}
		}
		return strings.Join(parts, " ")
	}
	for _, key := range spec.Log.Show {
		for _, f := range ev.Fields {
			if f.Key == key && !skip(key) {
				parts = append(parts, capSafe(f.Key, 48)+"="+capSafe(f.Value, 96))
				break
			}
		}
	}
	return strings.Join(parts, " ")
}

// eventIndexes is the events the view shows: all of them, or those matching
// the filter when `/` is filtering events.
func (m deskModel) eventIndexes(e *deskRun) []int {
	idx := make([]int, 0, len(e.events))
	if len(m.filter) == 0 || m.filterTargetsList() {
		for i := range e.events {
			idx = append(idx, i)
		}
		return idx
	}
	needle := strings.ToLower(string(m.filter))
	for i, ev := range e.events {
		hay := strings.ToLower(ev.Name + "\x00" + ev.Step + "\x00" + eventFields(e.spec, ev))
		if strings.Contains(hay, needle) {
			idx = append(idx, i)
		}
	}
	return idx
}

func (m deskModel) eventsBody(e *deskRun, iw int) deskBody {
	st := m.styles
	idx := m.eventIndexes(e)
	if len(idx) == 0 {
		var lines []string
		switch {
		case errNoLog(e.err) && !e.everEvents:
			lines = []string{st.Muted.Render("no log yet")}
		case e.err != nil && !errNoLog(e.err):
			path := capSafe(e.spec.Log.Path, 96)
			if path == "" {
				path = "the log"
			}
			lines = []string{st.Danger.Render("cannot read " + path + ": " + capSafe(e.err.Error(), 240))}
		case !e.loaded:
			lines = []string{st.Muted.Render("loading…")}
		case len(e.events) > 0:
			lines = []string{st.Muted.Render("no events match the filter")}
		default:
			lines = []string{st.Muted.Render("no events yet")}
		}
		return textBodyFrom(lines)
	}
	cur := min(max(m.cur[viewEvents], 0), len(idx)-1)
	stepW := 10
	return deskBody{
		n: len(idx),
		line: func(i int) string {
			ev := e.events[idx[i]]
			when := "#" + strconv.Itoa(ev.Seq)
			if !ev.Time.IsZero() {
				when = ev.Time.UTC().Format("15:04:05")
			}
			tag := eventTag(e.spec, ev, m.glyph.Pending)
			marker := " "
			if i == cur {
				marker = ">"
			}
			return marker + st.Muted.Render(padTo(when, 8)) + " " + m.tagStyle(tag).Render(padTo(tag, 5)) + " " +
				st.Fg.Render(padTo(midCut(capSafe(ev.Step, 64), stepW), stepW)) + " " +
				st.Fg.Render(capSafe(ev.Name, 48)) + " " + st.Muted.Render(eventFields(e.spec, ev))
		},
		rowOfLine:  func(i int) (int, bool) { return i, true },
		rows:       len(idx),
		cursorLine: cur,
		cursorH:    1,
	}
}

// --- timeline ---

func (m deskModel) timelineBody(e *deskRun, iw int) deskBody {
	st := m.styles
	steps := e.state.Steps
	if len(steps) == 0 {
		return textBodyFrom([]string{st.Muted.Render(m.noStepsText(e))})
	}
	now := m.now()
	segs := make([]Seg, len(steps))
	for i, s := range steps {
		switch s.Status {
		case runview.StepClosed:
			segs[i] = SegDone
		case runview.StepRunning:
			segs[i] = SegRunning
		case runview.StepFailed:
			segs[i] = SegFailed
		default: // pending, and interrupted, which never finished
			segs[i] = SegWaiting
		}
	}
	lines := []string{BarSegments(st, iw, segs)}

	// Restart marks sit under the bar at the step that began after each one.
	if len(e.state.Restarts) > 0 {
		marks := []rune(strings.Repeat(" ", iw))
		mark := []rune(m.glyph.Resumed)[0]
		for _, r := range e.state.Restarts {
			at := len(steps)
			for i, s := range steps {
				if !s.Start.IsZero() && s.Start.After(r) {
					at = i
					break
				}
			}
			col := min(at*iw/len(steps), iw-1)
			marks[col] = mark
		}
		lines = append(lines, st.Warn.Render(string(marks)))
		for i, r := range e.state.Restarts {
			if i >= 4 {
				lines = append(lines, st.Muted.Render(fmt.Sprintf("… %d more restarts", len(e.state.Restarts)-i)))
				break
			}
			when := "restart"
			if !r.IsZero() {
				when += " at " + r.UTC().Format("15:04:05")
			}
			lines = append(lines, st.Warn.Render(m.glyph.Resumed+" "+when))
		}
	}
	lines = append(lines, "")

	var longest time.Duration
	durs := make([]time.Duration, len(steps))
	idW := 0
	for i, s := range steps {
		durs[i] = m.stepDur(e, s, now)
		longest = max(longest, durs[i])
		idW = max(idW, ansi.StringWidth(capSafe(s.ID, 64)))
	}
	idW = min(idW, max(iw/3, 6))
	barW := max(iw-idW-22, 6)
	for i, s := range steps {
		gl, word, sty := m.stepLook(e, s)
		frac := 0.0
		if longest > 0 {
			frac = float64(durs[i]) / float64(longest)
		}
		lines = append(lines, sty.Render(gl)+" "+st.Fg.Render(padTo(midCut(capSafe(s.ID, 64), idW), idW))+" "+
			BarSolid(st, barW, frac)+" "+sty.Render(padTo(word, 9))+st.Muted.Render(ageShort(durs[i])))
	}
	return textBodyFrom(lines)
}

// --- metrics ---

func (m deskModel) metricsBody(e *deskRun, iw int) deskBody {
	st := m.styles
	if len(e.metrics) == 0 {
		text := "no metrics yet"
		if e.spec.Metrics.Path == "" {
			text = "this run has no metrics"
		}
		return textBodyFrom([]string{st.Muted.Render(text)})
	}
	keySet := map[string]bool{}
	errs := 0
	for _, mt := range e.metrics {
		for k := range mt.Values {
			keySet[k] = true
		}
		if mt.Error {
			errs++
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	errLine := st.Muted.Render("0 errors")
	if errs > 0 {
		errLine = st.Danger.Render(fmt.Sprintf("%s %d errors", m.glyph.Failed, errs))
	}
	lines := []string{st.Fg.Render(fmt.Sprintf("%d lines", len(e.metrics))) + st.Muted.Render(" · ") + errLine, ""}
	keyW := 0
	for _, k := range keys {
		keyW = max(keyW, ansi.StringWidth(capSafe(k, 32)))
	}
	for _, k := range keys {
		var total int64
		series := make([]float64, 0, len(e.metrics))
		for _, mt := range e.metrics {
			v := mt.Values[k]
			total += v
			series = append(series, float64(v))
		}
		totalText := strconv.FormatInt(total, 10)
		sparkW := max(iw-keyW-ansi.StringWidth(totalText)-9, 4)
		lines = append(lines, st.Fg.Render(padTo(capSafe(k, 32), keyW))+" "+st.Muted.Render("total ")+st.Fg.Render(totalText)+"  "+
			st.Active.Render(Sparkline(series, sparkW)))
	}
	return textBodyFrom(lines)
}

// --- summary ---

func (m deskModel) summaryBody(e *deskRun) deskBody {
	st := m.styles
	if len(e.summary) == 0 {
		return textBodyFrom([]string{st.Muted.Render("no summary yet")})
	}
	var lines []string
	var prev []string
	for _, kv := range e.summary {
		parts := strings.Split(kv.Key, ".")
		dirs, leaf := parts[:len(parts)-1], parts[len(parts)-1]
		common := 0
		for common < len(dirs) && common < len(prev) && prev[common] == dirs[common] {
			common++
		}
		for i := common; i < len(dirs); i++ {
			lines = append(lines, strings.Repeat("  ", min(i, 8))+st.Accent.Render(capSafe(dirs[i], 64)+":"))
		}
		prev = dirs
		lines = append(lines, strings.Repeat("  ", min(len(dirs), 8))+st.Muted.Render(capSafe(leaf, 64)+": ")+st.Fg.Render(capSafe(kv.Value, 256)))
	}
	return textBodyFrom(lines)
}

// --- queue ---

// queueGroups is the order the queue lists its states in.
var queueGroups = []string{"waiting", "refused", "lost", "running", "skipped"}

func (m deskModel) queueLook(it runview.QueueItem) (string, string, lipgloss.Style) {
	st := m.styles
	g := m.glyph
	switch it.State {
	case "waiting":
		return g.Pending, "waiting", st.Dim
	case "refused":
		return g.Failed, "refused", st.Danger
	case "lost":
		return g.Lost, "lost", st.Danger
	case "running":
		return m.spin(), "running", st.Active
	case "skipped":
		return g.Skipped, "skipped", st.Dim
	case "done":
		return g.Done, "done", st.OK
	}
	return g.Pending, capSafe(it.State, 12), st.Dim
}

// queueOrder is the items grouped by state in queueGroups order, then any
// other state.
func (m deskModel) queueOrder() []runview.QueueItem {
	var out []runview.QueueItem
	for _, grp := range queueGroups {
		for _, it := range m.items {
			if it.State == grp {
				out = append(out, it)
			}
		}
	}
	for _, it := range m.items {
		if !slices.Contains(queueGroups, it.State) {
			out = append(out, it)
		}
	}
	return out
}

func (m deskModel) queueBody(iw int) deskBody {
	st := m.styles
	items := m.queueOrder()
	var lines []string
	for _, err := range m.queueErrs {
		lines = append(lines, st.Danger.Render("cannot read the queue: "+capSafe(errText(err), 240)))
	}
	if len(items) == 0 {
		if !m.listed {
			return textBodyFrom([]string{st.Muted.Render("loading runs…")})
		}
		return textBodyFrom(append(lines, st.Muted.Render("queue empty")))
	}
	cur := min(max(m.cur[viewQueue], 0), len(items)-1)
	lineRow := make([]int, 0, len(items)*2)
	rowStart := make([]int, len(items))
	add := func(row int, s string) {
		lines = append(lines, s)
		lineRow = append(lineRow, row)
	}
	// Lines already added (errors) have no row.
	for range lines {
		lineRow = append(lineRow, -1)
	}
	counts := map[string]int{}
	for _, it := range items {
		counts[it.State]++
	}
	lastState := "\x00"
	nameW := min(max(iw/3, 8), 28)
	for i, it := range items {
		if it.State != lastState {
			lastState = it.State
			add(-1, st.Header.Render(fmt.Sprintf("%s (%d)", capSafe(it.State, 16), counts[it.State])))
		}
		g, word, sty := m.queueLook(it)
		marker := " "
		nameSty := st.Fg
		if i == cur {
			marker, nameSty = ">", st.Selected
		}
		row := marker + " " + sty.Render(g) + " " + sty.Render(padTo(word, 8)) + " " +
			nameSty.Render(padTo(midCut(capSafe(it.Name, 96), nameW), nameW)) + " " +
			st.Muted.Render(capSafe(it.Kind, 16)+" ") + st.Fg.Render(capSafe(it.What, 160))
		if it.Stale {
			row += " " + st.Warn.Render(m.glyph.Stale+" stale")
		}
		if it.TTY {
			row += " " + st.Muted.Render("tty")
		}
		rowStart[i] = len(lines)
		add(i, row)
		if reason := m.queueReason(it); reason != "" {
			add(i, "    "+reason)
		}
	}
	return deskBody{
		n:          len(lines),
		line:       func(i int) string { return lines[i] },
		rowOfLine:  func(i int) (int, bool) { return lineRow[i], lineRow[i] >= 0 },
		rows:       len(items),
		cursorLine: rowStart[cur],
		cursorH:    1 + btoi(m.queueReason(items[cur]) != ""),
	}
}

// queueReason is the line under a queue row: why it cannot be approved, why it
// was skipped, or why it was queued.
func (m deskModel) queueReason(it runview.QueueItem) string {
	st := m.styles
	switch {
	case it.Refusal != "":
		return st.Danger.Render("cannot approve: " + capSafe(it.Refusal, 240))
	case it.SkipReason == desk.SkipOperator:
		return st.Muted.Render("skipped by the operator · u to undo")
	case it.SkipReason == desk.SkipChanged, it.SkipReason == deskLaunchFailed, it.SkipReason == desk.SkipLost:
		return st.Muted.Render("skipped: " + skipHint(it.SkipReason))
	case it.SkipReason != "":
		return st.Muted.Render("skipped: " + capSafe(it.SkipReason, 240))
	case it.Why != "":
		return st.Muted.Render("why: " + capSafe(it.Why, 240))
	}
	return ""
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
