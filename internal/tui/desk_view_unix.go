// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/meta"
)

// The layout, header, footer, help screen, and the scroll and hit-testing
// that depend on the layout. The bodies of the panes are in desk_flow_unix.go
// and desk_panes_unix.go.

type layoutKind int

const (
	layoutSmall layoutKind = iota
	layoutSingle
	layoutMid
	layoutWide
)

const (
	deskListW = 28
	deskFlowW = 62

	// deskTooSmall is shown below the minimum size.
	deskTooSmall = "terminal too small (needs 40×12) — resize or press q"
)

// deskLayout is how the window is divided: columns choose the layout and rows
// leave the panes their height.
type deskLayout struct {
	kind                layoutKind
	listW, flowW, sideW int
	y0                  int // row of the panes' top border
	panelH              int // a pane's height, borders included
}

func (m deskModel) layout() deskLayout {
	w, h := m.width, m.height
	l := deskLayout{y0: 1}
	switch {
	case w < deskMinW || h < deskMinH:
		l.kind = layoutSmall
	case w >= 150:
		l.kind = layoutWide
		l.listW, l.flowW, l.sideW = deskListW, deskFlowW, w-deskListW-deskFlowW
	case w >= 100:
		l.kind = layoutMid
		l.flowW, l.sideW = deskFlowW, w-deskFlowW
	default:
		l.kind = layoutSingle
		l.flowW, l.sideW = w, w
		l.y0 = 2
	}
	l.panelH = h - l.y0 - 2
	return l
}

// paneInner is the content size of the pane that shows view v (0 is the run
// list).
func (m deskModel) paneInner(v int) (iw, ih int) {
	l := m.layout()
	w := l.sideW
	switch {
	case l.kind == layoutSingle:
		w = l.flowW
	case v == 0:
		w = l.listW
	case v == viewFlow:
		w = l.flowW
	}
	return max(w-4, 1), max(l.panelH-2, 1)
}

// shownViews lists the views on screen: 0 for the run list.
func (m deskModel) shownViews() []int {
	switch l := m.layout(); l.kind {
	case layoutWide:
		return []int{0, viewFlow, m.sideShows()}
	case layoutMid:
		return []int{viewFlow, m.sideShows()}
	case layoutSingle:
		return []int{m.view}
	}
	return nil
}

// --- view ---

// View draws the dashboard in exactly width columns by height rows.
func (m deskModel) View() tea.View {
	var content string
	switch {
	case m.width == 0 || m.height == 0:
		content = ""
	case m.layout().kind == layoutSmall:
		content = m.smallView()
	default:
		content = m.frame0()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	// Event types let a terminal that supports them mark a held key as a
	// repeat, which the approve dialog refuses (it also refuses a y that
	// follows another key too closely, on any terminal).
	v.KeyboardEnhancements.ReportEventTypes = true
	if !m.mouseOff {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m deskModel) smallView() string {
	text := ansi.Wrap(deskTooSmall, max(m.width, 1), "")
	lines := strings.Split(m.styles.Warn.Render(text), "\n")
	return m.fit(lines)
}

// fitLines pads or cuts lines to the window's height, so a dialog can be
// drawn over any row.
func (m deskModel) fitLines(lines []string) []string {
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return lines
}

// fit makes lines exactly the window's size: padded or cut to height, each
// line cut to width.
func (m deskModel) fit(lines []string) string {
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "")
	}
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m deskModel) frame0() string {
	l := m.layout()
	lines := []string{m.headerView()}
	if l.kind == layoutSingle {
		lines = append(lines, m.tabStrip())
	}
	if m.showHelp {
		lines = append(lines, m.helpView(l.panelH)...)
	} else {
		lines = append(lines, strings.Split(m.panesView(l), "\n")...)
	}
	lines = append(lines, strings.Split(m.footerView(), "\n")...)
	return m.fit(m.overlayModal(m.fitLines(lines)))
}

// panesView joins the panes the layout shows.
func (m deskModel) panesView(l deskLayout) string {
	var cols []string
	switch l.kind {
	case layoutWide:
		cols = append(cols, m.pane(0, l.listW, m.focus == paneList))
		fallthrough
	case layoutMid:
		cols = append(cols, m.pane(viewFlow, l.flowW, m.focus == paneFlow))
		cols = append(cols, m.pane(m.sideShows(), l.sideW, m.focus == paneSide))
	default:
		cols = append(cols, m.pane(m.view, l.flowW, false))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cols...)
}

// pane draws the pane that shows view v in width columns.
func (m deskModel) pane(v, width int, focused bool) string {
	l := m.layout()
	iw, ih := m.paneInner(v)
	var lines []string
	title, strip := "runs", ""
	switch v {
	case 0:
		lines = m.listLines(iw, ih)
		strip = fmt.Sprintf("%d", len(m.visibleOrder()))
	default:
		b := m.bodyFor(v)
		top := min(m.top[v], max(b.n-ih, 0))
		for i := top; i < b.n && i < top+ih; i++ {
			lines = append(lines, b.line(i))
		}
		title = deskViewNames[v]
		if l.kind != layoutSingle && v != viewFlow {
			strip = m.sideStrip()
		}
	}
	for len(lines) < ih {
		lines = append(lines, "")
	}
	if focused && l.kind != layoutSingle {
		title = m.glyphFocus() + title
	}
	return Panel(m.styles, width, title, strip, lines)
}

func (m deskModel) glyphFocus() string { return "> " }

// sideStrip is the side pane's view numbers, with the shown one named and
// bracketed.
func (m deskModel) sideStrip() string {
	var parts []string
	for v := viewEvents; v <= viewCount; v++ {
		if v == m.sideShows() {
			parts = append(parts, m.styles.Selected.Render(fmt.Sprintf("[%d %s]", v, deskViewNames[v])))
		} else {
			parts = append(parts, m.styles.Muted.Render(strconv.Itoa(v)))
		}
	}
	return strings.Join(parts, " ")
}

// tabStrip is the single-pane layout's view picker. It falls back to the
// shown view's name with the other numbers when the full strip does not fit.
func (m deskModel) tabStrip() string {
	segs := m.tabSegments()
	var parts []string
	for _, s := range segs {
		if s.view == m.view {
			parts = append(parts, m.styles.Selected.Render("["+s.text+"]"))
		} else {
			parts = append(parts, m.styles.Muted.Render(s.text))
		}
	}
	return strings.Join(parts, " ")
}

type tabSegment struct {
	view int
	text string
}

// tabSegments are the tab labels: every view's number and name when that
// fits the width, else the shown view's name and the other numbers.
func (m deskModel) tabSegments() []tabSegment {
	full := make([]tabSegment, 0, viewCount)
	total := viewCount - 1
	for v := viewFlow; v <= viewCount; v++ {
		t := fmt.Sprintf("%d %s", v, deskViewNames[v])
		full = append(full, tabSegment{v, t})
		total += len(t) + 2 // the brackets of the shown one
	}
	if total-2 <= m.width {
		return full
	}
	out := make([]tabSegment, 0, viewCount)
	for v := viewFlow; v <= viewCount; v++ {
		t := fmt.Sprintf("%d", v)
		if v == m.view {
			t = fmt.Sprintf("%d %s", v, deskViewNames[v])
		}
		out = append(out, tabSegment{v, t})
	}
	return out
}

// tabAt maps a click on the tab strip to a view.
func (m deskModel) tabAt(x, y int) (int, bool) {
	if m.layout().kind != layoutSingle || y != 1 {
		return 0, false
	}
	col := 0
	for _, s := range m.tabSegments() {
		w := len(s.text)
		if s.view == m.view {
			w += 2
		}
		if x >= col && x < col+w {
			return s.view, true
		}
		col += w + 1
	}
	return 0, false
}

// --- header and footer ---

func (m deskModel) headerView() string {
	s := m.styles
	parts := []string{s.Brand.Render(m.glyph.Forge) + " " + s.Header.Render(meta.AppName+" desk")}
	if m.layout().kind == layoutMid {
		if strip := m.runStrip(); strip != "" {
			parts = append(parts, strip)
		}
	}
	if w := m.waiting(); w > 0 || m.lostItems() > 0 {
		chip := fmt.Sprintf("queue: %d waiting", w)
		if n := m.lostItems(); n > 0 {
			chip += fmt.Sprintf(" · lost: %d", n)
		}
		parts = append(parts, s.Warn.Render(chip))
	}
	if c := m.replayChip(); c != "" {
		parts = append(parts, s.Accent.Render(c))
	}
	if n := len(m.failedSources()); n > 0 {
		parts = append(parts, s.Danger.Render(fmt.Sprintf("%d source failed", n)))
	}
	if e := m.sel(); e != nil {
		if c := m.ingestChip(e); c != "" {
			parts = append(parts, s.Warn.Render(c))
		}
	}
	return strings.Join(parts, s.Dim.Render("  ·  "))
}

// runStrip is the mid layout's stand-in for the run list: the selected run
// between ‹ and ›, with its place in the order.
func (m deskModel) runStrip() string {
	e := m.sel()
	if e == nil {
		return ""
	}
	order := m.visibleOrder()
	i := slices.Index(order, m.selected) + 1
	now := m.now()
	g, word, st := m.runLook(e, now)
	text := fmt.Sprintf("‹ %d/%d %s ", i, len(order), midCut(capSafe(e.key.name, 96), 16))
	return m.styles.Fg.Render(text) + st.Render(g+" "+word) + m.styles.Fg.Render(" "+m.runAge(e, now)+" ›")
}

// ingestChip names what the selected run's ingest left out.
func (m deskModel) ingestChip(e *deskRun) string {
	if e.partial {
		return "partial: over 32 MiB cap"
	}
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(e.dropped, "line dropped", "lines dropped")
	add(e.fields, "field dropped", "fields dropped")
	add(e.state.UnknownSteps, "unknown step", "unknown steps")
	add(e.state.BadExits, "bad exit", "bad exits")
	return strings.Join(parts, " · ")
}

// failedSources lists the sources that could not be used: those that failed
// to build and those whose list failed.
func (m deskModel) failedSources() []DeskSourceFailure {
	out := slices.Clone(m.failed)
	names := make([]string, 0, len(m.listErrs))
	for n := range m.listErrs {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		out = append(out, DeskSourceFailure{Name: n, Err: m.listErrs[n]})
	}
	return out
}

func (m deskModel) footerView() string {
	var first string
	switch {
	case m.filtering:
		first = m.styles.Accent.Render("/ ") + m.styles.Fg.Render(tailSafe(m.filter, cockpitFilterMax)) + m.styles.Accent.Render("▏")
	case m.footer != "":
		first = m.footer
	case m.pausedNote() != "":
		first = m.styles.Warn.Render(m.pausedNote())
	case len(m.filter) > 0:
		first = m.styles.Muted.Render("filter: " + capSafe(string(m.filter), cockpitFilterMax) + " (esc clears)")
	}
	mode := keymap.DeskBrowse
	switch md := m.topModal(); {
	case md != nil && md.kind == modalConfirm:
		mode = keymap.DeskConfirm
	case md != nil:
		mode = keymap.DeskModal
	case m.replay != nil:
		mode = keymap.DeskReplay
	case m.showHelp:
		mode = keymap.DeskHelp
	case m.filtering:
		mode = keymap.DeskFilter
	case m.sideShows() == viewQueue && m.layout().kind != layoutSmall:
		mode = keymap.DeskQueue
	}
	hint := m.help.ShortHelpView(keymap.DeskHelpMap{Keys: m.keys, Mode: mode}.ShortHelp())
	return first + "\n" + hint
}

// pausedNote is the events follow note, or "".
func (m deskModel) pausedNote() string {
	e := m.sel()
	if e == nil || m.follow[viewEvents] || !slices.Contains(m.shownViews(), viewEvents) {
		return ""
	}
	return fmt.Sprintf("paused · %d new · G to follow", max(len(e.events)-m.pausedAt, 0))
}

// --- scroll ---

// syncScroll settles every shown pane's cursor and scroll offset. It lives in
// Update so View only reads the model.
func (m *deskModel) syncScroll() {
	if m.showHelp {
		m.helpTop = min(m.helpTop, max(len(m.helpLines(m.width))-m.layout().panelH, 0))
		return
	}
	for _, v := range m.shownViews() {
		if v == 0 {
			m.settleList()
			continue
		}
		m.settle(v)
	}
}

func (m *deskModel) settleList() {
	order := m.visibleOrder()
	_, ih := m.paneInner(0)
	i := slices.Index(order, m.selected)
	if i >= 0 {
		if i < m.listTop {
			m.listTop = i
		}
		if i >= m.listTop+ih {
			m.listTop = i - ih + 1
		}
	}
	m.listTop = min(max(m.listTop, 0), max(len(order)-ih, 0))
}

func (m *deskModel) settle(v int) {
	_, ih := m.paneInner(v)
	if m.follow[v] {
		switch v {
		case viewFlow:
			if r := m.followStep(); r >= 0 {
				m.cur[v] = r
			}
		case viewEvents:
			m.cur[v] = max(m.rowCount(v)-1, 0)
		}
	}
	b := m.bodyFor(v)
	if b.rows > 0 {
		m.cur[v] = min(max(m.cur[v], 0), b.rows-1)
	}
	if b.cursorLine >= 0 {
		if b.cursorLine < m.top[v] {
			m.top[v] = b.cursorLine
		}
		if end := b.cursorLine + max(b.cursorH, 1); end > m.top[v]+ih {
			m.top[v] = min(end-ih, b.cursorLine)
		}
	}
	m.top[v] = min(max(m.top[v], 0), max(b.n-ih, 0))
}

// rowCount is the number of selectable rows in view v.
func (m deskModel) rowCount(v int) int { return m.bodyFor(v).rows }

// rowAtLine maps a line of view v to its selectable row.
func (m deskModel) rowAtLine(v, line int) (int, bool) {
	b := m.bodyFor(v)
	if b.rowOfLine == nil || line < 0 || line >= b.n {
		return 0, false
	}
	return b.rowOfLine(line)
}

// hit maps a screen cell to the pane it is in and the line inside that pane.
func (m deskModel) hit(x, y int) (pane, line int, ok bool) {
	l := m.layout()
	if l.kind == layoutSmall || m.showHelp {
		return 0, 0, false
	}
	line = y - l.y0 - 1
	if line < 0 || line >= l.panelH-2 || x < 0 || x >= m.width {
		return 0, 0, false
	}
	switch l.kind {
	case layoutSingle:
		return paneFlow, line, true
	case layoutMid:
		if x < l.flowW {
			return paneFlow, line, true
		}
		return paneSide, line, true
	}
	switch {
	case x < l.listW:
		return paneList, line, true
	case x < l.listW+l.flowW:
		return paneFlow, line, true
	}
	return paneSide, line, true
}

// --- help ---

func (m deskModel) helpView(h int) []string {
	lines := m.helpLines(m.width)
	top := min(m.helpTop, max(len(lines)-h, 0))
	var out []string
	for i := top; i < len(lines) && i < top+h; i++ {
		out = append(out, lines[i])
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// helpLines is the help screen: the keys, the legend, and the status detail
// that the header only has room to name.
func (m deskModel) helpLines(_ int) []string {
	s := m.styles
	g := m.glyph
	var lines []string
	lines = append(lines, s.Header.Render("status"))
	fails := m.failedSources()
	if len(fails) == 0 {
		lines = append(lines, "  "+s.Muted.Render("all sources loaded"))
	}
	for _, f := range fails {
		lines = append(lines, "  "+s.Danger.Render("failed")+" "+s.Fg.Render(capSafe(f.Name, 64))+s.Muted.Render(": "+capSafe(errText(f.Err), 400)))
	}
	if e := m.sel(); e != nil {
		if c := m.ingestChip(e); c != "" {
			lines = append(lines, "  "+s.Warn.Render(c))
		}
	}
	lines = append(lines, "", s.Header.Render("legend"))
	legend := [][3]string{
		{g.Pending, "pending / waiting", "not started"},
		{g.Running, "running / live", "events are arriving"},
		{g.Done, "done / ok", "closed, or ended with exit 0"},
		{g.Failed, "failed / exit N", "failed, or ended with a non-zero exit"},
		{g.Interrupted, "interrupted", "still running when the run ended"},
		{g.Rejected, "rej N/M", "the gate sent the step back N of M times"},
		{g.Resumed, "resumed", "restarted since the step's previous start"},
		{g.Stale, "stale", "no events within the run's limit"},
		{g.Lost, "lost", "the run's supervisor died"},
		{g.Skipped, "skipped", "the queue item or step was skipped"},
	}
	for _, r := range legend {
		lines = append(lines, "  "+s.Fg.Render(r[0])+" "+s.Fg.Render(fmt.Sprintf("%-18s", r[1]))+s.Muted.Render(r[2]))
	}
	lines = append(lines, "  "+s.Muted.Render("event tags: start ok FAIL rej rst end chk · (unmapped)"))

	lines = append(lines, "")
	lines = append(lines, s.Header.Render("keys"))
	for _, grp := range (keymap.DeskHelpMap{Keys: m.keys}).FullHelp() {
		for _, b := range grp {
			h := b.Help()
			lines = append(lines, "  "+s.Accent.Render(fmt.Sprintf("%-8s", h.Key))+" "+s.Fg.Render(h.Desc))
		}
	}
	lines = append(lines, "", s.Muted.Render("? or esc closes this screen"))
	return lines
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}
