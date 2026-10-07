package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The timeline (t on the dashboard) is the desk's day, read top down: every
// item, newest first, grouped by day, each at the moment that matters for
// it (queued, started, ended or skipped), with what it is in its own words
// and what became of it in plain ones. It answers "what happened while I
// was away" without reading a log or a hash.
//
// It is not where anything runs. A waiting item's entry takes the operator
// back to the dashboard with that item selected, so y still acts only on
// the focus panel's hash (#1098). Like the frame, everything here is pure.

// tlEntry is one timeline entry: an item and the moment it is placed at.
type tlEntry struct {
	row queueRow
	at  time.Time
}

// deskTimeline builds the timeline's entries from a snapshot, newest first.
// Entries with no known time sort last, under their own heading.
func deskTimeline(snap *desk.Snapshot) []tlEntry {
	if snap == nil {
		return nil
	}
	var out []tlEntry
	for _, it := range snap.Pending {
		k := rowWaiting
		if it.State == desk.StateRefused {
			k = rowRefused
		}
		out = append(out, tlEntry{queueRow{k, it}, metaTime(it.Meta.AddedAt)})
	}
	for _, it := range snap.Running {
		k := rowRunning
		if it.State == desk.StateLost {
			k = rowLost
		}
		out = append(out, tlEntry{queueRow{k, it}, firstTime(it.Started, metaTime(it.Meta.ClaimedAt), metaTime(it.Meta.AddedAt))})
	}
	for _, it := range snap.Done {
		out = append(out, tlEntry{queueRow{doneKind(it), it}, firstTime(it.Ended, it.Started, metaTime(it.Meta.AddedAt))})
	}
	for _, it := range snap.Skipped {
		k := rowSkipped
		if it.Meta.SkipReason == desk.SkipChanged {
			k = rowChanged
		}
		out = append(out, tlEntry{queueRow{k, it}, firstTime(metaTime(it.Meta.SkippedAt), metaTime(it.Meta.EndedAt), metaTime(it.Meta.AddedAt))})
	}
	// What needs the operator first, then the rest; each newest first, an
	// unknown time last; the name breaks ties so a frame is the same bytes
	// for the same snapshot.
	slices.SortStableFunc(out, func(a, b tlEntry) int {
		switch {
		case a.needsYou() != b.needsYou():
			if a.needsYou() {
				return -1
			}
			return 1
		case a.at.IsZero() != b.at.IsZero():
			if a.at.IsZero() {
				return 1
			}
			return -1
		case !a.at.Equal(b.at):
			return b.at.Compare(a.at)
		}
		return strings.Compare(b.row.item.Name, a.row.item.Name)
	})
	return out
}

func metaTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// needsYou reports an entry only the operator can move on: a waiting item
// to run or skip, or a lost run to clear. These lead the timeline, under
// their own heading, wherever their day is.
func (e tlEntry) needsYou() bool {
	return e.row.kind == rowWaiting || e.row.kind == rowLost
}

// tlNeedsYou is the leading group's heading.
const tlNeedsYou = "Needs you"

// tlGroup is the heading an entry sits under.
func tlGroup(now time.Time, e tlEntry) string {
	if e.needsYou() {
		return tlNeedsYou
	}
	return tlDay(now, e.at)
}

// hasRun reports whether the entry's item started a run, so it has a log
// and a run view. A skip the operator made after a run was lost did run.
func (e tlEntry) hasRun() bool {
	switch e.row.kind {
	case rowRunning, rowLost, rowDone, rowFailed:
		return true
	case rowSkipped:
		return e.row.item.Meta.SkipReason == desk.SkipLost
	}
	return false
}

// tlNew reports whether an entry happened after seen, the moment the
// operator last closed the timeline. An entry that needs you is never
// "new": it leads the timeline under its own heading instead.
func tlNew(e tlEntry, seen time.Time) bool {
	return !e.needsYou() && !e.at.IsZero() && e.at.After(seen)
}

// tlOutcome is what became of an entry's item, in plain words.
func tlOutcome(e tlEntry, now time.Time, steps []desk.StepStatus) string {
	it := e.row.item
	length := func() string {
		if it.Started.IsZero() || it.Ended.IsZero() {
			return ""
		}
		return clock(it.Ended.Sub(it.Started))
	}
	switch e.row.kind {
	case rowWaiting:
		s := "waiting for you"
		if !e.at.IsZero() {
			s += " · queued " + agoLabel(now, e.at)
		}
		if it.TTY {
			s += " · runs in the desk's pane"
		}
		if it.Stale {
			s += " · stale"
		}
		return s
	case rowRefused:
		return "can't run · refused: " + deskText(it.Refusal)
	case rowRunning:
		s := "running"
		if it.Kind == desk.KindBatch && len(steps) > 0 {
			finished := 0
			for _, st := range steps {
				if st.State != desk.StepPending && st.State != desk.StepRunning {
					finished++
				}
			}
			s += fmt.Sprintf(" · %d of %d steps", finished, len(steps))
		}
		if !it.Started.IsZero() {
			s += " · " + clock(now.Sub(it.Started)) + " so far"
		}
		return s
	case rowLost:
		return "lost mid-run · it may have partly run · " + lostNext
	case rowDone:
		if it.ExitCode == nil {
			return "ended with no exit recorded"
		}
		if l := length(); l != "" {
			return "ran ok in " + l
		}
		return "ran ok"
	case rowFailed:
		s := "failed · exit " + strconv.Itoa(*it.ExitCode)
		if l := length(); l != "" {
			s += " after " + l
		}
		return s
	case rowChanged:
		return "not run · its bytes changed after it was queued · " + changedNext
	case rowSkipped:
		return skipOutcome(it.Meta)
	}
	return ""
}

// skipOutcome says why a skipped item did not run. The "dashboard" and
// "cli" values are desk.SkippedByDashboard and desk.SkippedByCLI, which
// live in a Unix-only file.
func skipOutcome(m desk.Meta) string {
	note := ""
	if m.SkipNote != "" {
		note = ": " + deskText(m.SkipNote)
	}
	switch reason := m.SkipReason; {
	case reason == desk.SkipOperator && m.SkippedBy == "cli":
		return "skipped from the command line" + note
	case reason == desk.SkipOperator:
		return "skipped by you" + note
	case reason == desk.SkipLost:
		return "cleared after it was lost mid-run" + note
	case reason == desk.SkipLaunchFailed:
		return "never started · its run could not begin"
	case reason == desk.SkipReused:
		return "not run · its number was already used"
	case strings.HasPrefix(reason, "refused"):
		return "not run · " + deskText(reason)
	}
	return "skipped" + note
}

// tlDay is a group's heading: "Today", "Yesterday", or "Mon 5 Oct", and
// "Undated" for entries with no known time.
func tlDay(now, t time.Time) string {
	if t.IsZero() {
		return "Undated"
	}
	t = t.In(now.Location())
	day := func(x time.Time) time.Time {
		y, m, d := x.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	}
	switch day(now).Sub(day(t)) / (24 * time.Hour) {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	if t.Year() != now.Year() {
		return t.Format("Mon 2 Jan 2006")
	}
	return t.Format("Mon 2 Jan")
}

// deskTimelineView is the open timeline's state: the selected entry, by
// name and kind so a rescan keeps it, and the first line on screen.
type deskTimelineView struct {
	cursor int
	name   string
	kind   rowKind
	offset int
}

// tlFrame is one frame of the timeline: its inputs and nothing else.
type tlFrame struct {
	snap          *desk.Snapshot
	steps         map[string][]desk.StepStatus
	width, height int
	now, seen     time.Time
	theme         *theme.Theme
	cursor        int
	offset        int
	footer        string
}

func (f tlFrame) styles() theme.Styles {
	if f.theme != nil {
		return f.theme.Styles()
	}
	return theme.Default().Styles()
}

// tlLine is a drawn line and the entry it belongs to (-1 for a heading).
type tlLine struct {
	text  string
	entry int
}

// tlTimeW is the time column's width, "19:05", and tlGutter the selection
// mark before it.
const (
	tlTimeW  = 5
	tlGutter = 2
	// tlMetaMin is the narrowest window that keeps an entry's name and kind
	// on its title line, right-aligned.
	tlMetaMin = 72
)

// body lays out every entry, headings included, at the frame's width.
func (f tlFrame) body(st theme.Styles, entries []tlEntry) []tlLine {
	width := max(f.width, deskMinWidth)
	var lines []tlLine
	for i := 0; i < len(entries); {
		day := tlGroup(f.now, entries[i])
		j := i
		for j < len(entries) && tlGroup(f.now, entries[j]) == day {
			j++
		}
		if i > 0 {
			lines = append(lines, tlLine{"", -1})
		}
		lines = append(lines, tlLine{f.dayHeading(st, width, day, entries[i].at, j-i), -1})
		for k := i; k < j; k++ {
			title, preview := f.entryLines(st, width, entries[k], k == f.cursor, k == j-1)
			lines = append(lines, tlLine{title, k}, tlLine{preview, k})
		}
		i = j
	}
	return lines
}

// dayHeading is "Today · Wed 7 Oct ───────── 4": the day, its date, a rule
// and how many entries it holds. The needs-you group says what to do.
func (f tlFrame) dayHeading(st theme.Styles, width int, day string, t time.Time, n int) string {
	head := " " + st.Header.Render(day)
	switch day {
	case "Today", "Yesterday":
		head += st.Muted.Render(" · " + t.In(f.now.Location()).Format("Mon 2 Jan"))
	case tlNeedsYou:
		head += st.Muted.Render(" · enter takes you to it")
	}
	count := st.Muted.Render(strconv.Itoa(n))
	rule := width - ansi.StringWidth(head) - ansi.StringWidth(count) - 3
	if rule < 2 {
		return cut(head, width)
	}
	return head + " " + st.Dim.Render(strings.Repeat("─", rule)) + " " + count
}

// entryLines draws one entry's two lines:
//
//	▸ 19:05  ◌ Merge the feature branch       • needs you   17 merge-feature
//	         │ waiting for you · queued 12m ago
//
// The rail runs down from each mark to the next entry of the same day and
// stops at the last one.
func (f tlFrame) entryLines(st theme.Styles, width int, e tlEntry, selected, last bool) (string, string) {
	glyph, label, look := rowLook(st, e.row.kind)
	it := e.row.item

	pick := strings.Repeat(" ", tlGutter)
	if selected {
		pick = st.Accent.Render("▸ ")
	}
	// A day group's entries carry their clock time; the needs-you group
	// spans days, so its entries carry how long they have waited.
	clockText := "-"
	switch {
	case e.at.IsZero():
	case e.needsYou():
		clockText = strings.TrimSuffix(agoLabel(f.now, e.at), " ago")
	default:
		clockText = e.at.In(f.now.Location()).Format("15:04")
	}
	lead := pick + st.Muted.Render(fmt.Sprintf("%*s", tlTimeW, clockText)) + "  " + look(glyph) + " "
	leadW := tlGutter + tlTimeW + 2 + 2

	title := deskText(it.What)
	if title == "" {
		title = itemLabel(it.Name)
	}
	// The needs-you heading already says what those entries want, so they
	// carry no badge; a day's entries carry "new" when they are.
	var badge string
	switch {
	case e.needsYou():
	case tlNew(e, f.seen):
		badge = st.Accent.Render("• new")
	}
	meta := ""
	if width >= tlMetaMin && it.What != "" {
		meta = itemLabel(it.Name)
		if it.Kind == desk.KindBatch {
			meta += " · batch"
		} else if it.TTY {
			meta += " · tty"
		}
		meta = cut(meta, deskNameMax)
	}
	room := width - leadW - ansi.StringWidth(meta) - 1
	if badge != "" {
		room -= ansi.StringWidth(badge) + 2
	}
	room = max(room, 8)
	titleStyle := st.Fg.Bold(true)
	if selected {
		titleStyle = st.Selected
	}
	line := lead + titleStyle.Render(cut(title, room))
	if badge != "" {
		line += "  " + badge
	}
	if meta != "" {
		gap := width - ansi.StringWidth(line) - ansi.StringWidth(meta) - 1
		line += strings.Repeat(" ", max(gap, 1)) + st.Muted.Render(meta)
	}

	rail := st.Dim.Render("│")
	if last {
		rail = " "
	}
	outcome := tlOutcome(e, f.now, f.steps[it.Name])
	if outcome == "" {
		outcome = label
	}
	previewStyle := st.Muted
	switch e.row.kind {
	case rowFailed, rowRefused:
		previewStyle = st.Danger.Bold(false)
	case rowLost, rowChanged:
		previewStyle = st.Warn
	case rowRunning:
		previewStyle = st.Active
	}
	pad := strings.Repeat(" ", tlGutter+tlTimeW+2)
	preview := pad + rail + " " + previewStyle.Render(cut(outcome, max(width-leadW-1, 8)))
	return cut(line, width), cut(preview, width)
}

// counts are the header's numbers.
func (f tlFrame) counts(entries []tlEntry) (waiting, running, fresh int) {
	for _, e := range entries {
		switch {
		case e.needsYou():
			waiting++
		case e.row.kind == rowRunning:
			running++
		}
		if tlNew(e, f.seen) {
			fresh++
		}
	}
	return waiting, running, fresh
}

// header is "timeline  ◌ 3 need you · ● 2 running · • 4 new        19:07".
func (f tlFrame) header(st theme.Styles, width int, entries []tlEntry) string {
	waiting, running, fresh := f.counts(entries)
	var parts []string
	if waiting > 0 {
		parts = append(parts, st.Accent.Render("◌ "+strconv.Itoa(waiting)+" need you"))
	}
	if running > 0 {
		parts = append(parts, st.Active.Render("● "+strconv.Itoa(running)+" running"))
	}
	if fresh > 0 {
		parts = append(parts, st.Accent.Render("• "+strconv.Itoa(fresh)+" new"))
	}
	if len(parts) == 0 {
		parts = append(parts, st.Muted.Render("all caught up"))
	}
	left := st.Header.Render("desk timeline") + "  " + strings.Join(parts, st.Muted.Render(" · "))
	right := st.Muted.Render(f.now.Format("15:04"))
	left = cut(left, width-ansi.StringWidth(right)-2)
	return left + strings.Repeat(" ", max(width-ansi.StringWidth(left)-ansi.StringWidth(right), 1)) + right
}

// tlHints is the timeline's footer: what enter does for the selected entry
// first, then the keys that can act on it.
func tlHints(st theme.Styles, width int, e tlEntry, ok bool) string {
	type hint struct {
		key, label string
		prio       int
	}
	var hs []hint
	if ok {
		switch {
		case e.row.kind == rowWaiting:
			hs = append(hs, hint{"enter", "review to run", 1})
		case e.hasRun():
			hs = append(hs, hint{"enter", "run view", 2})
		}
		if e.hasRun() {
			hs = append(hs, hint{"l", "log", 3})
		}
		hs = append(hs, hint{"v", "script", 4}, hint{"j/k", "move", 5})
	}
	hs = append(hs, hint{"t", "back", 0})
	hints := make([]string, len(hs))
	prio := make([]int, len(hs))
	for i, h := range hs {
		hints[i] = st.Accent.Render(h.key) + " " + st.Muted.Render(h.label)
		prio[i] = i
	}
	slices.SortStableFunc(prio, func(a, b int) int { return hs[a].prio - hs[b].prio })
	return " " + fitHints(width-1, hints, prio)
}

// tlScroll returns the first body line to show so the selected entry's two
// lines are on screen (and its day's heading, when it opens the day),
// moving as little as possible from offset.
func tlScroll(lines []tlLine, cursor, offset, rows int) int {
	if rows <= 0 {
		return 0
	}
	first, lastLine := -1, -1
	for i, l := range lines {
		if l.entry == cursor {
			if first < 0 {
				first = i
			}
			lastLine = i
		}
	}
	if first < 0 {
		return min(max(offset, 0), max(len(lines)-rows, 0))
	}
	// Keep the heading right above an entry that opens its day.
	if first > 0 && lines[first-1].entry == -1 && lines[first-1].text != "" {
		first--
	}
	switch {
	case first < offset:
		offset = first
	case lastLine >= offset+rows:
		offset = lastLine - rows + 1
	}
	return min(max(offset, 0), max(len(lines)-rows, 0))
}

// tlEntriesFrom counts the entries whose title line is at or after line i.
func tlEntriesFrom(lines []tlLine, i int) int {
	n := 0
	for j := max(i, 0); j < len(lines); j++ {
		if lines[j].entry >= 0 && (j == 0 || lines[j-1].entry != lines[j].entry) {
			n++
		}
	}
	return n
}

// panelLines are the timeline's lines cut to rows, for the dashboard's panel:
// whole entries only, no heading left hanging, and a last line saying how
// many entries were left out.
func (f tlFrame) panelLines(st theme.Styles, entries []tlEntry, rows int) []string {
	body := f.body(st, entries)
	if len(body) <= rows {
		return tlTexts(body)
	}
	keep := max(rows-1, 0)
	// Drop an entry cut between its two lines, then a heading or blank line
	// with nothing under it.
	if keep > 0 && body[keep].entry >= 0 && body[keep].entry == body[keep-1].entry {
		keep--
	}
	for keep > 0 && body[keep-1].entry < 0 {
		keep--
	}
	out := tlTexts(body[:keep])
	left := tlEntriesFrom(body, keep)
	return append(out, st.Muted.Render(fmt.Sprintf("… %d more · t to open", left)))
}

func tlTexts(lines []tlLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}

// render draws the frame and returns the scroll offset it used.
func (f tlFrame) render() (string, int) {
	st := f.styles()
	width := max(f.width, deskMinWidth)
	entries := deskTimeline(f.snap)
	cursor := min(max(f.cursor, 0), max(len(entries)-1, 0))
	f.cursor = cursor

	header := f.header(st, width, entries)
	footer := f.footer
	if footer == "" {
		var sel tlEntry
		if len(entries) > 0 {
			sel = entries[cursor]
		}
		footer = tlHints(st, width, sel, len(entries) > 0)
	}
	var body []tlLine
	if len(entries) == 0 {
		body = []tlLine{
			{"", -1},
			{"  " + st.Muted.Render("nothing on the desk yet"), -1},
			{"  " + st.Muted.Render("Claude queues scripts with forgectl desk add; each one shows here as it is queued, runs and ends"), -1},
		}
	} else {
		body = f.body(st, entries)
	}
	rows := len(body)
	if f.height > 0 {
		rows = max(f.height-3, 1) // header, a blank line, the footer
		if len(body) > rows {
			rows = max(rows-1, 1) // and a line saying how much is below
		}
	}
	offset := tlScroll(body, cursor, f.offset, rows)
	out := []string{cut(header, width), ""}
	for i := offset; i < len(body) && i < offset+rows; i++ {
		out = append(out, cut(body[i].text, width))
	}
	if f.height > 0 {
		if below := tlEntriesFrom(body, offset+rows); below > 0 {
			out = append(out, cut(st.Muted.Render(fmt.Sprintf("  ↓ %d more below", below)), width))
		}
		out = fitHeight(out, f.height-1)
	}
	out = append(out, cut(footer, width))
	return strings.Join(out, "\n"), offset
}
