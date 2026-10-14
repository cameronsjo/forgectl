// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The flowchart layout (desk show --live --layout flow) draws a run's steps
// top to bottom as boxes between a start and an end terminal: each box
// toned by its state, with its duration and its note beside it, the running
// one marked now, and the end showing how the run finished. The event
// timeline sits beside the chart when the window is wide enough, else below.

const (
	// flowBoxMin and flowBoxMax bound a step box's inner width; a longer id
	// is cut.
	flowBoxMin = 12
	flowBoxMax = 28
	// flowSideBySide is the narrowest window that puts the timeline beside
	// the chart rather than under it.
	flowSideBySide = 100
	// flowChartMax is the widest the chart column gets beside the timeline.
	flowChartMax = 64
)

// flowchartBody is the run view's body in the flowchart layout: rows lines
// at width.
func (v *deskRunView) flowchartBody(st theme.Styles, g runview.Glyphs, s runview.RunState, width, rows int) []string {
	var now time.Time
	if v.follow {
		now = time.Now()
		if v.clock != nil {
			now = v.clock()
		}
	}
	finished := runEnded(v.sourceLive())
	live, exit := v.live(s), s.Exit
	if finished && v.folder != nil {
		live, exit = v.sourceLive(), v.folder.At(v.folder.Len()).Exit
	}
	chart, focus := flowchart(st, g, s, v.timing(), runview.RunMark(g, live, exit), now)

	rule := "events"
	if c := v.counts(s); c != "" {
		rule += " · " + c
	}
	if v.err != nil {
		rule += " · " + safeMessage(v.err.Error())
	}
	ruleLine := st.Muted.Render(" " + rule)

	if width >= flowSideBySide {
		chartW := min(flowChartMax, width*3/5)
		eventsW := width - chartW - 3
		left := flowWindow(chart, focus, rows)
		right := append([]string{cut(ruleLine, eventsW)}, v.flowEvents(st, eventsW, rows-1)...)
		out := make([]string, rows)
		for i := range out {
			l, r := "", ""
			if i < len(left) {
				l = cut(left[i], chartW)
			}
			if i < len(right) {
				r = right[i]
			}
			out[i] = padTo(l, chartW) + st.Dim.Render(" │ ") + r
		}
		return out
	}
	chartRows := max(rows-max(rows/3, 2)-1, 1)
	out := flowWindow(chart, focus, chartRows)
	for i := range out {
		out[i] = cut(out[i], width)
	}
	out = append(out, cut(ruleLine, width))
	return append(out, v.flowEvents(st, width, max(rows-len(out), 1))...)
}

// flowEvents is the timeline at width in rows lines.
func (v *deskRunView) flowEvents(st theme.Styles, width, rows int) []string {
	if v.folder == nil || v.folder.Len() == 0 {
		return []string{st.Muted.Render(" " + noEventsText(v.delta.Live))}
	}
	return runEvents(st, v.folder, v.at, width, rows, v.raw)
}

// flowWindow is rows lines of chart that keep line focus in view, a third
// of the way down when the chart is taller than rows.
func flowWindow(chart []string, focus, rows int) []string {
	if len(chart) <= rows {
		return slices.Clone(chart)
	}
	start := min(max(focus-rows/3, 0), len(chart)-rows)
	return slices.Clone(chart[start : start+rows])
}

// flowchart draws s top to bottom: a start terminal, a box per step in
// depth order, and an end terminal marked end. focus is the line of the
// step to keep in view: the running one, else the last that did anything.
// now, when set, counts a running step's time so far.
func flowchart(st theme.Styles, g runview.Glyphs, s runview.RunState, timing []runview.StepTiming, end runview.Mark, now time.Time) (lines []string, focus int) {
	inner := flowBoxMin
	for _, x := range s.Steps {
		inner = max(inner, ansi.StringWidth(x.ID)+2)
	}
	inner = min(inner, flowBoxMax)
	dim := st.Dim
	arrow := func() {
		lines = append(lines, "  "+dim.Render(center("↓", inner+2)))
	}
	terminal := func(label, side string, below bool) {
		bottom := "╰" + strings.Repeat("─", inner) + "╯"
		if below {
			bottom = "╰" + center("┬", inner, '─') + "╯"
		}
		lines = append(lines,
			"  "+dim.Render("╭"+strings.Repeat("─", inner)+"╮"),
			"  "+dim.Render("│")+st.Fg.Render(center(label, inner))+dim.Render("│")+side,
			"  "+dim.Render(bottom))
	}

	terminal("Start", "", true)
	arrow()
	if len(s.Steps) == 0 {
		lines = append(lines, "  "+st.Muted.Render(center("no step model", inner+2)))
		arrow()
	}

	after := map[string][]string{}
	for _, e := range s.Edges {
		after[e[1]] = append(after[e[1]], e[0])
	}
	depth := stepDepths(s)
	order := make([]int, len(s.Steps))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return depth[a] - depth[b] })

	focus, lastActive := -1, 0
	for k, i := range order {
		x := s.Steps[i]
		mk := runview.StepMark(g, x.Status, runview.RunnerState(timing, x.ID))
		tone := runToneStyle(st, mk.Tone)
		state := mk.Glyph + " " + mk.Word
		d := runview.StepDur(timing, x)
		if d == 0 && x.Status == runview.StepRunning && !x.Start.IsZero() && !now.IsZero() {
			d = now.Sub(x.Start)
		}
		if d > 0 {
			state += " " + d.Round(time.Second).String()
		}
		side := "  " + tone.Render(state)
		if x.Status == runview.StepRunning {
			side += st.Accent.Render(" ← now")
			if focus < 0 {
				focus = len(lines) + 1
			}
		}
		if x.Status != runview.StepPending {
			lastActive = len(lines) + 1
		}
		var note []string
		if x.Note != "" {
			note = append(note, x.Note)
		}
		if p := after[x.ID]; len(p) > 0 && (k == 0 || len(p) > 1 || p[0] != s.Steps[order[k-1]].ID) {
			note = append(note, "after "+strings.Join(p, ", "))
		}
		bottomSide := ""
		if len(note) > 0 {
			bottomSide = "  " + st.Muted.Render(strings.Join(note, " · "))
		}
		lines = append(lines,
			"  "+tone.Render("┌"+strings.Repeat("─", inner)+"┐"),
			"  "+tone.Render("│")+tone.Render(center(cutPlain(x.ID, inner-2), inner))+tone.Render("│")+side,
			"  "+tone.Render("└"+center("┬", inner, '─')+"┘")+bottomSide)
		arrow()
	}
	if focus < 0 {
		focus = lastActive
	}

	endSide := "  " + runToneStyle(st, end.Tone).Render(end.Glyph+" "+end.Word)
	terminal("End", endSide, false)
	return lines, focus
}

// center is s centered in w cells, padded with fill (a space when none is
// given). s must be plain text no wider than w.
func center(s string, w int, fill ...rune) string {
	f := " "
	if len(fill) > 0 {
		f = string(fill[0])
	}
	gap := max(w-ansi.StringWidth(s), 0)
	return strings.Repeat(f, gap/2) + s + strings.Repeat(f, gap-gap/2)
}

// cutPlain cuts plain text to w cells with an ellipsis.
func cutPlain(s string, w int) string { return ansi.Truncate(s, w, "…") }
