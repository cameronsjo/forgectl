// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/runview"
)

// The flow view: a run's steps as boxes (full form) or two-row entries
// (compact form), one band per wave, following the running step.

const (
	// Rows the full form needs around its step bands, and per band. The form
	// is chosen from these: full when 3 + 6 per band + 7 rows fit.
	flowFullTop    = 3
	flowFullBand   = 6
	flowFullBottom = 7

	flowBoxRows  = 4 // a box: top border, two content rows, bottom border
	flowBoxMax   = 44
	flowBoxMin   = 15
	flowCompactH = 2
)

// followStep is the step the flow follows: the running one, else the last
// one that finished while the run is still going. -1 means none.
func (m deskModel) followStep() int {
	e := m.sel()
	if e == nil {
		return -1
	}
	if m.liveState(e, m.now()) == runview.LiveEnded {
		return -1
	}
	last := -1
	for i, s := range e.state.Steps {
		switch s.Status {
		case runview.StepRunning:
			return i
		case runview.StepClosed, runview.StepFailed:
			last = i
		}
	}
	return last
}

// bands groups the steps into waves: a step sits one band below the deepest
// step it waits on. A wave wider than the pane is split into rows.
func (m deskModel) bands(e *deskRun, iw int) [][]int {
	steps := e.state.Steps
	idx := make(map[string]int, len(steps))
	for i, s := range steps {
		idx[s.ID] = i
	}
	level := make([]int, len(steps))
	for range steps {
		changed := false
		for _, ed := range e.state.Edges {
			a, okA := idx[ed[0]]
			b, okB := idx[ed[1]]
			if okA && okB && level[b] < level[a]+1 {
				level[b] = level[a] + 1
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	deepest := 0
	for _, l := range level {
		deepest = max(deepest, l)
	}
	perRow := max(1, (iw+1)/(flowBoxMin+1))
	var out [][]int
	for l := 0; l <= deepest; l++ {
		var wave []int
		for i := range steps {
			if level[i] == l {
				wave = append(wave, i)
			}
		}
		for len(wave) > 0 {
			n := min(perRow, len(wave))
			out = append(out, wave[:n])
			wave = wave[n:]
		}
	}
	return out
}

// stepDur is how long a step ran: the desk's timing when it has one, else
// the step's own start and end, else the time since a running step began.
func (m deskModel) stepDur(e *deskRun, s runview.StepState, now time.Time) time.Duration {
	for _, t := range e.timing {
		if t.Step != s.ID {
			continue
		}
		if t.Dur > 0 {
			return t.Dur
		}
		if s.Status == runview.StepRunning && !t.Start.IsZero() && t.End.IsZero() {
			return now.Sub(t.Start)
		}
	}
	switch {
	case s.Start.IsZero():
		return 0
	case s.End.IsZero():
		if s.Status == runview.StepRunning {
			return now.Sub(s.Start)
		}
		return 0
	}
	return s.End.Sub(s.Start)
}

// timingState is the desk's own word for a step ("skipped", "cancelled"), or "".
func timingState(e *deskRun, id string) string {
	for _, t := range e.timing {
		if t.Step == id {
			return t.State
		}
	}
	return ""
}

// stepLook is a step's mark, word and style.
func (m deskModel) stepLook(e *deskRun, s runview.StepState) (string, string, lipgloss.Style) {
	st := m.styles
	switch ts := timingState(e, s.ID); {
	case ts == "skipped" && s.Status == runview.StepPending:
		return m.glyph.Skipped, "skipped", st.Dim
	case ts == "cancelled" && s.Status != runview.StepClosed:
		return m.glyph.Skipped, "cancelled", st.Dim
	}
	switch s.Status {
	case runview.StepRunning:
		return m.spin(), "running", st.Active
	case runview.StepClosed:
		return m.glyph.Done, "done", st.OK
	case runview.StepFailed:
		return m.glyph.Failed, "failed", st.Danger
	case runview.StepInterrupted:
		return m.glyph.Interrupted, "interrupted", st.Warn
	}
	return m.glyph.Pending, "pending", st.Dim
}

// spin is the running mark for the current frame.
func (m deskModel) spin() string {
	frames := []rune(m.glyph.Spin)
	if len(frames) == 0 {
		return m.glyph.Running
	}
	return string(frames[m.frame%len(frames)])
}

// stepFlags are a step's extra marks: the attempt, the gate's rejects, and a
// resumed step.
func (m deskModel) stepFlags(e *deskRun, s runview.StepState) []string {
	var out []string
	if s.Attempt > 1 {
		out = append(out, fmt.Sprintf("try %d", s.Attempt))
	}
	if s.Rejects > 0 {
		if limit := e.spec.Gate.MaxRejects; limit > 0 {
			out = append(out, fmt.Sprintf("%s rej %d/%d", m.glyph.Rejected, s.Rejects, limit))
		} else {
			out = append(out, fmt.Sprintf("%s rej %d", m.glyph.Rejected, s.Rejects))
		}
	}
	if s.Resumed {
		out = append(out, m.glyph.Resumed+" resumed")
	}
	return out
}

// flowBody builds the flow view for the selected run.
func (m deskModel) flowBody(iw, ih int) deskBody {
	e := m.sel()
	if e == nil {
		return m.placeholderBody(iw)
	}
	now := m.now()
	steps := e.state.Steps
	st := m.styles

	var lines []string
	var lineStep []int
	push := func(step int, ls ...string) {
		for _, l := range ls {
			lines = append(lines, l)
			lineStep = append(lineStep, step)
		}
	}
	stepLine := make([]int, len(steps))
	for i := range stepLine {
		stepLine[i] = -1
	}

	bands := m.bands(e, iw)
	full := flowFullTop+flowFullBand*len(bands)+flowFullBottom <= ih
	g, word, ls := m.runLook(e, now)
	name := st.Header.Render(midCut(capSafe(e.key.name, 96), max(iw-24, 8)))
	state := ls.Render(g + " " + word)
	if full {
		push(-1, name+st.Muted.Render("  "+capSafe(e.key.source, 32)), state+st.Muted.Render(m.stateDetail(e, now)), "")
	} else {
		push(-1, name+"  "+state+st.Muted.Render(m.stateDetail(e, now)))
	}
	for _, n := range m.notices(e, now) {
		push(-1, n)
	}
	if len(steps) == 0 {
		push(-1, st.Muted.Render(m.noStepsText(e)))
		return textBodyFrom(lines)
	}

	cur := min(max(m.cur[viewFlow], 0), len(steps)-1)
	idW := 0
	for _, s := range steps {
		idW = max(idW, ansi.StringWidth(capSafe(s.ID, 64)))
	}
	idW = min(idW, max(iw/3, 6))

	for bi, band := range bands {
		if full {
			m.fullBand(e, band, cur, iw, now, &lines, &lineStep, stepLine)
			// The connector to what comes next.
			center := m.bandCenter(len(band), iw)
			push(-1, strings.Repeat(" ", center)+st.Dim.Render("│"), strings.Repeat(" ", center)+st.Dim.Render("▼"))
			continue
		}
		for pos, si := range band {
			s := steps[si]
			gl, w, sty := m.stepLook(e, s)
			gutter := "│"
			switch {
			case len(band) == 1:
			case pos == 0:
				gutter = "┌"
			case pos == len(band)-1:
				gutter = "└"
			default:
				gutter = "├"
			}
			marker := " "
			if si == cur {
				marker = ">"
			}
			dur := ageShort(m.stepDur(e, s, now))
			flags := strings.Join(m.stepFlags(e, s), " · ")
			row := marker + st.Dim.Render(gutter) + " " + sty.Render(gl) + " " +
				st.Fg.Render(padTo(midCut(capSafe(s.ID, 64), idW), idW)) + " " + sty.Render(padTo(w, 9)) +
				st.Muted.Render(padTo(dur, 5)) + st.Warn.Render(flags)
			stepLine[si] = len(lines)
			push(si, row)
			tail := "│"
			if bi == len(bands)-1 && pos == len(band)-1 {
				tail = " "
			}
			push(si, " "+st.Dim.Render(tail)+"   "+st.Muted.Render(capSafe(s.Note, 120)))
		}
	}
	push(-1, m.flowBottom(e, now, full)...)

	blockH := flowCompactH
	if full {
		blockH = flowBoxRows
	}
	return deskBody{
		n:    len(lines),
		line: func(i int) string { return lines[i] },
		rowOfLine: func(i int) (int, bool) {
			r := lineStep[i]
			return r, r >= 0
		},
		rows:       len(steps),
		cursorLine: stepLine[cur],
		cursorH:    blockH,
	}
}

// bandCenter is the column a connector below a band of k boxes sits at.
func (m deskModel) bandCenter(k, iw int) int {
	total, _ := m.boxWidths(k, iw)
	pad := max((iw-total)/2, 0)
	return pad + total/2
}

// boxWidths is the width of each box in a band of k and the width of the
// whole row.
func (m deskModel) boxWidths(k, iw int) (total, each int) {
	if k <= 1 {
		each = min(iw, flowBoxMax)
		return each, each
	}
	each = min((iw-(k-1))/k, flowBoxMax)
	return each*k + k - 1, each
}

// fullBand draws one band of boxes, centered, appending its lines.
func (m deskModel) fullBand(e *deskRun, band []int, cur, iw int, now time.Time, lines *[]string, lineStep *[]int, stepLine []int) {
	st := m.styles
	total, each := m.boxWidths(len(band), iw)
	pad := strings.Repeat(" ", max((iw-total)/2, 0))
	boxes := make([][]string, len(band))
	for i, si := range band {
		s := e.state.Steps[si]
		gl, w, sty := m.stepLook(e, s)
		border := st.Dim
		switch s.Status {
		case runview.StepRunning:
			border = st.Active
		case runview.StepClosed:
			border = st.OK
		case runview.StepFailed:
			border = st.Danger
		case runview.StepInterrupted:
			border = st.Warn
		}
		marker := ""
		if si == cur {
			border = st.Selected
			marker = "> "
		}
		title := marker + sty.Render(gl) + " " + st.Fg.Render(capSafe(s.ID, 48))
		status := w
		if d := ageShort(m.stepDur(e, s, now)); d != "" {
			status += " " + d
		}
		if flags := m.stepFlags(e, s); len(flags) > 0 {
			status += " · " + strings.Join(flags, " · ")
		}
		boxes[i] = deskBox(border, each, title, []string{
			st.Muted.Render(capSafe(s.Note, 120)),
			sty.Render(status),
		})
		stepLine[si] = len(*lines)
	}
	for r := range flowBoxRows {
		var row strings.Builder
		row.WriteString(pad)
		for i := range band {
			if i > 0 {
				row.WriteString(" ")
			}
			row.WriteString(boxes[i][r])
		}
		*lines = append(*lines, row.String())
		*lineStep = append(*lineStep, band[0])
	}
}

// deskBox draws a rounded box w cells wide with a title in its top border and
// one row per content string, each cut to fit.
func deskBox(border lipgloss.Style, w int, title string, rows []string) []string {
	inner := max(w-2, 1)
	t := cut(" "+title+" ", inner)
	top := border.Render("╭") + t + border.Render(strings.Repeat("─", max(inner-ansi.StringWidth(t), 0))+"╮")
	out := []string{top}
	for _, r := range rows {
		out = append(out, border.Render("│")+" "+padTo(cut(r, inner-2), inner-2)+" "+border.Render("│"))
	}
	return append(out, border.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// stateDetail is the text after the run's state mark: when it last moved, or
// how it ended.
func (m deskModel) stateDetail(e *deskRun, now time.Time) string {
	live := m.liveState(e, now)
	switch {
	case live == runview.LiveEnded && e.state.Exit != nil:
		d := fmt.Sprintf(" · exit %d", *e.state.Exit)
		if mean := capSafe(e.state.ExitMeaning, 96); mean != "" {
			d += " — " + mean
		}
		return d
	case !e.activity().IsZero():
		return " · " + m.runAge(e, now) + " ago"
	}
	return ""
}

// noStepsText says why the flow has nothing to draw.
func (m deskModel) noStepsText(e *deskRun) string {
	if !e.loaded {
		return "loading…"
	}
	return "no steps defined for this run"
}

// flowBottom is what follows the steps: the gate, the checks, the exit and
// the restarts.
func (m deskModel) flowBottom(e *deskRun, now time.Time, full bool) []string {
	st := m.styles
	var out []string
	if full {
		out = append(out, "")
	}
	if g := e.spec.Gate; g.Label != "" || g.MaxRejects > 0 {
		text := "gate: " + capSafe(g.Label, 48)
		if g.MaxRejects > 0 {
			text += fmt.Sprintf(" · up to %d rejects", g.MaxRejects)
		}
		out = append(out, st.Muted.Render(text))
	}
	for _, c := range e.state.Checks {
		label := capSafe(c.Label, 64)
		if label == "" {
			label = capSafe(c.ID, 64)
		}
		switch {
		case !c.Seen:
			out = append(out, st.Dim.Render(m.glyph.Pending+" pending ")+st.Muted.Render(label))
		case c.Passed:
			out = append(out, st.OK.Render(m.glyph.Done+" passed  ")+st.Fg.Render(label))
		default:
			out = append(out, st.Danger.Render(m.glyph.Failed+" failed  ")+st.Fg.Render(label))
		}
	}
	switch live := m.liveState(e, now); {
	case e.state.Exit != nil && *e.state.Exit == 0:
		out = append(out, st.OK.Render(m.glyph.Done+" exit 0")+st.Muted.Render(meaning(e)))
	case e.state.Exit != nil:
		out = append(out, st.Danger.Render(m.glyph.Failed+" exit "+strconv.Itoa(*e.state.Exit))+st.Muted.Render(meaning(e)))
	case live == runview.LiveEnded:
		out = append(out, st.Muted.Render(m.glyph.Pending+" ended, no exit recorded"))
	default:
		out = append(out, st.Dim.Render(m.glyph.Pending+" not finished"))
	}
	if n := len(e.state.Restarts); n > 0 {
		text := fmt.Sprintf("%s %d restart", m.glyph.Resumed, n)
		if n > 1 {
			text += "s"
		}
		if last := e.state.Restarts[n-1]; !last.IsZero() {
			text += ", last " + last.UTC().Format("15:04:05")
		}
		out = append(out, st.Warn.Render(text))
	}
	return out
}

func meaning(e *deskRun) string {
	if m := capSafe(e.state.ExitMeaning, 96); m != "" {
		return " — " + m
	}
	return ""
}

// notices are the lines that explain why a run looks the way it does:
// vanished, unreadable, lost, stale, or restarted.
func (m deskModel) notices(e *deskRun, now time.Time) []string {
	st := m.styles
	var out []string
	if e.gone {
		out = append(out, st.Warn.Render("gone: no longer listed; showing the last state"))
	}
	if e.loadErr != nil {
		out = append(out, st.Danger.Render("cannot load: "+capSafe(errText(e.loadErr), 240)))
	}
	switch {
	case errNoLog(e.err) && !e.everEvents:
		out = append(out, st.Muted.Render("no log yet"))
	case e.err != nil && !errNoLog(e.err):
		path := capSafe(e.spec.Log.Path, 96)
		if path == "" {
			path = "the log"
		}
		out = append(out, st.Danger.Render("cannot read "+path+": "+capSafe(e.err.Error(), 240)))
	}
	switch m.liveState(e, now) {
	case runview.LiveLost:
		out = append(out, st.Danger.Render("lost: supervisor died — s to skip"))
	case runview.LiveStale:
		since := "never"
		if !e.state.LastEvent.IsZero() {
			since = ageShort(now.Sub(e.state.LastEvent))
		}
		out = append(out, st.Warn.Render(fmt.Sprintf("stale: no events for %s (limit %s)", since, ageShort(e.spec.Live.StaleAfter))))
	}
	if e.note != "" && now.Before(e.noteUntil) {
		out = append(out, st.Muted.Render(capSafe(e.note, 96)))
	}
	return out
}

// ageShort is a duration in one unit: 12s, 4m, 3h, 2d. Zero and negative
// durations are empty.
func ageShort(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// midCut shortens s to w runes by cutting out its middle, so a suffix such as
// "-retry" survives. The tail is never shorter than the head.
func midCut(s string, w int) string {
	r := []rune(s)
	switch {
	case len(r) <= w:
		return s
	case w < 3:
		return string(r[:max(w, 0)])
	}
	head := (w - 1) / 2
	tail := w - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}
