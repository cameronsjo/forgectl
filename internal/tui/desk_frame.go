package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The desk dashboard (`forgectl desk`): one frame of it, drawn from a desk
// snapshot and nothing else. Everything in this file is pure, so a frame is
// the same bytes for the same inputs: the goldens and `desk --frame` rely on
// that. The interactive model (desk_model_unix.go) owns the I/O and calls in.
//
// Every value that came from a script or the desk directory (a name, WHAT,
// WHY, a script line, a step id or command, a refusal reason) goes through
// deskText, which is termsafe.SafeLine, before it is laid out.

// DeskFrameOptions is what a frame needs beyond the snapshot.
type DeskFrameOptions struct {
	// Host and Version go in the header line. Dir is the desk directory as
	// shown, with the home prefix already turned into "~" (see TildePath).
	Host    string
	Version string
	Dir     string
	// Started is when the desk came up, for "up 4h37m"; zero leaves it out.
	Started time.Time
	// Theme is the palette; nil means theme.Default().
	Theme *theme.Theme
	// ASCII draws the desk's marks, borders and bars in ASCII (--no-icons).
	ASCII bool
	// Steps holds a running or finished batch's status.tsv rows, by item
	// name. A batch with none draws as all waiting.
	Steps map[string][]desk.StepStatus
	// Records holds the bytes of items that are no longer pending (a running
	// item's record), by name, for the focus panel. Pending items carry their
	// own bytes in the snapshot.
	Records map[string][]byte
}

// RenderDeskFrame draws one frame of the desk at width x height as of now,
// with the first queue row selected. height <= 0 means "as tall as it needs".
func RenderDeskFrame(snap *desk.Snapshot, width, height int, now time.Time, opts DeskFrameOptions) string {
	return deskFrame{snap: snap, width: width, height: height, now: now, opts: opts}.render()
}

// Layout constants.
const (
	// deskWideMin is the narrowest window that gets the three stat tiles;
	// below it they collapse to one summary line and the bars shrink.
	deskWideMin = 72
	// deskShortHash is how many hex characters of an item's sha256 the focus
	// panel shows, the same short form `desk status` prints. Display only:
	// hashes are always compared in full.
	deskShortHash = 12
	// deskSparkHours is how many hourly buckets a tile's sparkline covers.
	deskSparkHours = 24
	// deskTileRows is a tile's height: its border plus three lines.
	deskTileRows = 5
	// deskRecentDone is how long a finished run stays in the queue panel, and
	// deskRecentMax how many of them may.
	deskRecentDone = 30 * time.Minute
	deskRecentMax  = 3
	// deskChangedFor is how long an item skipped as changed stays in the queue.
	deskChangedFor = 24 * time.Hour
	// deskFocusBody is how many script lines or steps the focus panel shows
	// when the window has room, and deskFocusBodyMin when it is short.
	deskFocusBody    = 3
	deskFocusBodyMin = 1
	// deskFocusBodyMax is the most it shows when the history leaves room.
	deskFocusBodyMax = 8
	// deskHistoryDefault caps history rows when the height is unknown.
	deskHistoryDefault = 10
	// deskTextMax caps any one untrusted value, in rendered runes, before the
	// panel cuts it to the width.
	deskTextMax = 512
	// deskNameMax caps the name column, so a wide window does not push a
	// row's bar and detail far from its name.
	deskNameMax = 36
)

// deskText renders untrusted text as one inert line. Tabs become spaces
// first, so a script's indentation reads as indentation rather than "\t".
func deskText(s string) string {
	return termsafe.SafeLineMax(strings.ReplaceAll(s, "\t", "    "), deskTextMax)
}

// TildePath shows dir with a leading home directory as "~".
func TildePath(dir, home string) string {
	if home != "" && home != "/" {
		if dir == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(dir, strings.TrimSuffix(home, "/")+"/"); ok {
			return "~/" + rest
		}
	}
	return dir
}

// rowKind is what a queue row is.
type rowKind int

const (
	rowWaiting rowKind = iota
	rowRefused
	rowRunning
	rowLost
	rowDone
	rowFailed
	rowChanged
)

// queueRow is one row of the queue panel.
type queueRow struct {
	kind rowKind
	item desk.Item
}

// runnable reports whether `y` can run the row.
func (r queueRow) runnable() bool { return r.kind == rowWaiting }

// deskRows builds the queue panel's rows from a snapshot: waiting items,
// then running ones, then runs that ended in the last deskRecentDone, then
// items skipped because they changed in the last deskChangedFor.
func deskRows(snap *desk.Snapshot, now time.Time) []queueRow {
	if snap == nil {
		return nil
	}
	var rows []queueRow
	for _, it := range snap.Pending {
		k := rowWaiting
		if it.State == desk.StateRefused {
			k = rowRefused
		}
		rows = append(rows, queueRow{k, it})
	}
	for _, it := range snap.Running {
		k := rowRunning
		if it.State == desk.StateLost {
			k = rowLost
		}
		rows = append(rows, queueRow{k, it})
	}
	recent := 0
	for _, it := range snap.Done { // most recent first
		if recent == deskRecentMax || it.Ended.IsZero() || now.Sub(it.Ended) > deskRecentDone {
			break
		}
		rows = append(rows, queueRow{doneKind(it), it})
		recent++
	}
	changed := 0
	for _, it := range snap.Skipped {
		if it.Meta.SkipReason != desk.SkipChanged || it.Meta.AddedAt == nil || now.Sub(*it.Meta.AddedAt) > deskChangedFor {
			continue
		}
		if changed == deskRecentMax {
			break
		}
		rows = append(rows, queueRow{rowChanged, it})
		changed++
	}
	return rows
}

func doneKind(it desk.Item) rowKind {
	if it.ExitCode != nil && *it.ExitCode != 0 {
		return rowFailed
	}
	return rowDone
}

// deskFrame is one frame's inputs plus the model's view state.
type deskFrame struct {
	snap          *desk.Snapshot
	width, height int
	now           time.Time
	opts          DeskFrameOptions

	cursor int
	// footer replaces the key hints when set: a prompt or the last result.
	// It is already safe and styled.
	footer string
	// confirming marks a y/n prompt in the footer: a dashboard it crowds out
	// says so, rather than that the window is too small to run items, since
	// the prompt itself is how they run.
	confirming bool
}

func (f deskFrame) styles() (theme.Styles, theme.Theme) {
	th := theme.Default()
	if f.opts.Theme != nil {
		th = *f.opts.Theme
	}
	return th.Styles(), th
}

// render lays the frame out: header, tiles (or a summary line), queue,
// focus, history, footer. With a known height the frame is exactly that many
// lines; the history panel takes what the others leave.
func (f deskFrame) render() string {
	lines, _ := f.layout()
	return asciiFrame(strings.Join(lines, "\n"), f.opts.ASCII)
}

// deskMinWidth is the narrowest window the frame is drawn for; a narrower
// terminal cuts the right of every line.
const deskMinWidth = 40

// focusShown reports whether this frame, as drawn, shows the selected row's
// short sha256, what and why in full. y runs an item only when it does: the
// operator approves by matching that hash, so a frame too small to show it
// must not let y act (#1098).
func (f deskFrame) focusShown() bool {
	_, shown := f.layout()
	return shown
}

// focusShownFor is focusShown for the item named name: the frame's selected
// row must be that item, so the gate and the run cannot drift apart.
func (f deskFrame) focusShownFor(name string) bool {
	rows := deskRows(f.snap, f.now)
	c := min(max(f.cursor, 0), max(len(rows)-1, 0))
	return len(rows) > 0 && rows[c].item.Name == name && f.focusShown()
}

// layout builds the frame's lines and reports whether the selected row's
// focus head (with its short sha256), what and why are all on screen.
//
// The focus panel is what the operator approves from, so it is placed
// before everything but the header, the footer and one queue row: a short
// window gives up the tiles, then the summary line, then queue rows, then
// the history and the script preview. Below that minimum the frame says it
// is too small instead of drawing a focus panel with its hash cut off.
func (f deskFrame) layout() ([]string, bool) {
	st, _ := f.styles()
	width := max(f.width, deskMinWidth)
	rows := deskRows(f.snap, f.now)
	cursor := min(max(f.cursor, 0), max(len(rows)-1, 0))

	header := f.header(st, width)
	footer := f.footerLines(st, width)
	focusMin := f.focusPanel(st, width, rows, cursor, 0)
	if f.height <= 0 {
		top := []string{cut(f.summary(st), width)}
		if width >= deskWideMin {
			top = f.tiles(st, width)
		}
		queue := f.queuePanel(st, width, rows, cursor, max(len(rows), 1))
		focus := f.focusPanel(st, width, rows, cursor, deskFocusBody)
		lines := slices.Concat([]string{header}, top, queue, focus, f.historyPanel(st, width, -1), footer)
		return lines, f.essentialShown(lines, 1+len(top)+len(queue), focusMin, rows, cursor)
	}

	avail := f.height - 1 - len(footer)
	need := deskQueueMin + len(focusMin)
	var top []string
	switch {
	case width >= deskWideMin && avail-deskTileRows >= need:
		top = f.tiles(st, width)
	case avail-1 >= need:
		top = []string{cut(f.summary(st), width)}
	}
	if avail-len(top) < need || f.narrow() {
		return f.tooSmall(st, header, footer, rows, cursor), false
	}
	avail -= len(top)

	queueLines := min(max(len(rows), 1), max(deskQueueMin, avail/3), avail-2-len(focusMin))
	queue := f.queuePanel(st, width, rows, cursor, queueLines)

	body := deskFocusBody
	if f.snap != nil {
		// Rows the history will not use go to the script preview.
		histNeed := min(max(len(f.snap.Done), 1), deskHistoryDefault) + 2
		for b := deskFocusBodyMax; b > deskFocusBody; b-- {
			if len(queue)+len(f.focusPanel(st, width, rows, cursor, b))+histNeed <= avail {
				body = b
				break
			}
		}
	}
	focus := f.focusPanel(st, width, rows, cursor, body)
	histRows := avail - len(queue) - len(focus) - 2
	if histRows < 1 {
		focus = f.focusPanel(st, width, rows, cursor, deskFocusBodyMin)
		histRows = avail - len(queue) - len(focus) - 2
	}
	if len(queue)+len(focus) > avail {
		focus = focusMin
		histRows = avail - len(queue) - len(focus) - 2
	}
	history := f.historyPanel(st, width, max(histRows, 0)) // -1 is historyPanel's "unbounded"

	lines := fitHeight(slices.Concat([]string{header}, top, queue, focus, history), f.height-len(footer))
	lines = append(lines, footer...)
	return lines, f.essentialShown(lines, 1+len(top)+len(queue), focusMin, rows, cursor)
}

// deskQueueMin is the queue panel's height with one row: its border and the
// row.
const deskQueueMin = 3

// narrow reports a known window narrower than the frame, which cuts the
// right of every line. 0 means the width is unknown, drawn at the minimum.
func (f deskFrame) narrow() bool { return f.width > 0 && f.width < deskMinWidth }

// fitHeight cuts lines to n, or pads them with blank lines to n.
func fitHeight(lines []string, n int) []string {
	n = max(n, 0)
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// essentialShown checks the drawn lines, not the plan: the smallest focus
// panel's top border, head, what and why (all of focusMin but its bottom
// border) must sit at start in lines exactly as drawn, inside the window,
// and the head must carry the row's short sha256.
func (f deskFrame) essentialShown(lines []string, start int, focusMin []string, rows []queueRow, cursor int) bool {
	if len(rows) == 0 || f.narrow() {
		return false
	}
	it := rows[cursor].item
	if !desk.ValidSHA256(it.Meta.SHA256) {
		return false
	}
	n := len(focusMin) - 1
	if start+n > len(lines) || (f.height > 0 && start+n > f.height) {
		return false
	}
	for i := range n {
		if lines[start+i] != focusMin[i] {
			return false
		}
	}
	return strings.Contains(ansi.Strip(lines[start+1]), "sha256 "+it.Meta.SHA256[:deskShortHash])
}

// tooSmall is the frame for a window that cannot show the focus panel: the
// header, a line saying which dimension is short, as much of the queue as
// fits, and the footer. A narrow window asks for the minimum width; past it,
// the rows asked for are measured at the current width, where what and why
// wrap as drawn. The key hints leave out y and a, which cannot run here.
func (f deskFrame) tooSmall(st theme.Styles, header string, footer []string, rows []queueRow, cursor int) []string {
	width := max(f.width, deskMinWidth)
	visible := width
	if f.narrow() {
		visible = f.width
	}
	var text string
	switch {
	case f.confirming:
		text = "dashboard hidden while you confirm"
	case f.narrow():
		text = fmt.Sprintf("too small: needs %d columns", deskMinWidth)
	default:
		need := 1 + deskQueueMin + len(f.focusPanel(st, width, rows, cursor, 0)) + 1
		text = fmt.Sprintf("too small to show the sha256 · make the window %d rows tall to run items", need)
		if ansi.StringWidth(text) > visible {
			text = fmt.Sprintf("too small: make it %d rows tall", need)
		}
	}
	if f.footer == "" {
		footer = []string{cut(f.hints(st, visible, false), visible)}
	}
	lines := []string{header, cut(st.Warn.Render(text), visible)}
	if room := f.height - len(lines) - len(footer); room >= deskQueueMin {
		lines = append(lines, f.queuePanel(st, width, rows, cursor, room-2)...)
	}
	return append(fitHeight(lines, f.height-len(footer)), footer...)
}

// deskPromptRoom is how many lines a footer prompt may take in a window
// height lines tall: half of it, and at least two. 0 or less means the
// height is unknown and there is no limit. The model pages the a prompt to
// this, so the frame never has to cut a hash out of it.
func deskPromptRoom(height int) int {
	if height <= 0 {
		return 0
	}
	return max(height/2, 2)
}

// footerLines is the footer cut to the width: the key hints, or the prompt
// or result the model set. A prompt may span lines (a full hash per item);
// the model keeps it within deskPromptRoom. One that is longer anyway is cut
// to that room and says so; nothing is ever drawn over the dashboard.
func (f deskFrame) footerLines(st theme.Styles, width int) []string {
	if f.footer == "" {
		return []string{cut(f.hints(st, width, true), width)}
	}
	lines := strings.Split(f.footer, "\n")
	if room := deskPromptRoom(f.height); room > 0 && len(lines) > room {
		more := len(lines) - (room - 1)
		lines = append(lines[:room-1], st.Muted.Render(fmt.Sprintf("  … %d more lines not shown", more)))
	}
	for i, l := range lines {
		lines[i] = cut(l, width)
	}
	return lines
}

// waiting counts the items that can run; tty counts the TTY ones among them.
func (f deskFrame) waiting() (n, tty int, oldest time.Time) {
	if f.snap == nil {
		return 0, 0, time.Time{}
	}
	for _, it := range f.snap.Pending {
		if it.State != desk.StateWaiting {
			continue
		}
		n++
		if it.TTY {
			tty++
		}
		if it.Meta.AddedAt != nil && (oldest.IsZero() || it.Meta.AddedAt.Before(oldest)) {
			oldest = *it.Meta.AddedAt
		}
	}
	return n, tty, oldest
}

func (f deskFrame) header(st theme.Styles, width int) string {
	n, _, _ := f.waiting()
	// The dot says what it means in words, so it reads without colour (#1107).
	dot := st.Dim.Render("○ nothing waiting")
	if n > 0 {
		dot = st.Accent.Render("● " + strconv.Itoa(n) + " waiting")
	}
	parts := []string{"forgectl " + deskText(f.opts.Version)}
	if f.opts.Host != "" {
		parts = append(parts, deskText(f.opts.Host))
	}
	if f.opts.Dir != "" {
		parts = append(parts, deskText(f.opts.Dir))
	}
	left := st.Header.Render("desk") + " " + dot + st.Muted.Render(" · "+strings.Join(parts, " · "))
	var right []string
	if !f.opts.Started.IsZero() {
		right = append(right, "up "+uptime(f.now.Sub(f.opts.Started)))
	}
	right = append(right, f.now.Format("15:04"))
	r := st.Muted.Render(strings.Join(right, " · "))
	room := width - ansi.StringWidth(r) - 2
	left = cut(left, room)
	return left + strings.Repeat(" ", max(width-ansi.StringWidth(left)-ansi.StringWidth(r), 1)) + r
}

// uptime is a compact duration: 12m, 4h37m, 3d4h.
func uptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
}

// clock renders a run's length as m:ss or h:mm:ss.
func clock(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	h, m, s := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// shortDur is a median's length: 41s, 3m, 2h.
func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
}

// agoLabel is "23m ago", or "-" when the time is unknown.
func agoLabel(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	a := ageLabel(now.Sub(t))
	if a == "" {
		a = "<1m"
	}
	return a + " ago"
}

// stats are the tiles' numbers.
type deskStats struct {
	waiting, tty int
	oldest       time.Time
	arrivals     []float64
	startedToday int
	median       time.Duration
	hasMedian    bool
	runs         []float64
	ok, failed   int
	noExit       int
	changed      int
	lost         int
	failures     []float64
}

// hourBucket returns which of the deskSparkHours hourly buckets ending at now
// t falls in, or -1 when it is outside them.
func hourBucket(now, t time.Time) int {
	if t.IsZero() || t.After(now) {
		return -1
	}
	ago := int(now.Sub(t) / time.Hour)
	if ago >= deskSparkHours {
		return -1
	}
	return deskSparkHours - 1 - ago
}

func (f deskFrame) stats() deskStats {
	s := deskStats{
		arrivals: make([]float64, deskSparkHours),
		runs:     make([]float64, deskSparkHours),
		failures: make([]float64, deskSparkHours),
	}
	s.waiting, s.tty, s.oldest = f.waiting()
	if f.snap == nil {
		return s
	}
	y, mo, d := f.now.Date()
	today := time.Date(y, mo, d, 0, 0, 0, 0, f.now.Location())
	var durations []time.Duration
	all := slices.Concat(f.snap.Pending, f.snap.Running, f.snap.Done, f.snap.Skipped)
	for _, it := range all {
		if it.Meta.AddedAt != nil {
			if b := hourBucket(f.now, *it.Meta.AddedAt); b >= 0 {
				s.arrivals[b]++
			}
		}
	}
	for _, it := range slices.Concat(f.snap.Running, f.snap.Done) {
		if b := hourBucket(f.now, it.Started); b >= 0 {
			s.runs[b]++
		}
		if !it.Started.IsZero() && !it.Started.Before(today) {
			s.startedToday++
			if it.State == desk.StateDone && !it.Ended.IsZero() {
				durations = append(durations, it.Ended.Sub(it.Started))
			}
		}
	}
	for _, it := range f.snap.Done {
		switch {
		case it.ExitCode == nil:
			s.noExit++
		case *it.ExitCode == 0:
			s.ok++
		default:
			s.failed++
			if b := hourBucket(f.now, it.Ended); b >= 0 {
				s.failures[b]++
			}
		}
	}
	for _, it := range f.snap.Skipped {
		if it.Meta.SkipReason == desk.SkipChanged {
			s.changed++
		}
	}
	for _, it := range f.snap.Running {
		if it.State == desk.StateLost {
			s.lost++
		}
	}
	if len(durations) > 0 {
		s.median, s.hasMedian = median(durations), true
	}
	return s
}

func median(ds []time.Duration) time.Duration {
	ds = slices.Clone(ds)
	slices.Sort(ds)
	n := len(ds)
	if n%2 == 1 {
		return ds[n/2]
	}
	return (ds[n/2-1] + ds[n/2]) / 2
}

func (f deskFrame) tiles(st theme.Styles, width int) []string {
	s := f.stats()
	tw := width / 3
	widths := [3]int{tw, tw, width - 2*tw}
	oldest := "nothing waiting"
	if !s.oldest.IsZero() {
		oldest = "oldest " + agoLabel(f.now, s.oldest)
		oldest = strings.TrimSuffix(oldest, " ago")
	}
	med := "median -"
	if s.hasMedian {
		med = "median " + shortDur(s.median)
	}
	outcome := st.OK.Render("✓") + " " + strconv.Itoa(s.ok) + "  " + st.Danger.Render("✗") + " " + strconv.Itoa(s.failed)
	if s.noExit > 0 {
		outcome += "  " + st.Muted.Render("?") + " " + strconv.Itoa(s.noExit)
	}
	changed := strconv.Itoa(s.changed) + " changed"
	if s.lost > 0 {
		changed += " · " + strconv.Itoa(s.lost) + " lost"
	}
	panels := [3]string{
		Panel(st, widths[0], "waiting", "", []string{
			st.Header.Render(strconv.Itoa(s.waiting)),
			st.Muted.Render(oldest),
			st.Accent.Render(Sparkline(s.arrivals, min(widths[0]-4, deskSparkHours))),
		}),
		Panel(st, widths[1], "started today", "", []string{
			st.Header.Render(strconv.Itoa(s.startedToday)),
			st.Muted.Render(med),
			st.Active.Render(Sparkline(s.runs, min(widths[1]-4, deskSparkHours))),
		}),
		Panel(st, widths[2], "outcomes", "", []string{
			outcome,
			st.Muted.Render(changed),
			st.Danger.Render(Sparkline(s.failures, min(widths[2]-4, deskSparkHours))),
		}),
	}
	split := [3][]string{}
	for i, p := range panels {
		split[i] = strings.Split(p, "\n")
	}
	out := make([]string, deskTileRows)
	for r := range out {
		out[r] = split[0][r] + split[1][r] + split[2][r]
	}
	return out
}

// summary is the narrow window's stand-in for the three tiles.
func (f deskFrame) summary(st theme.Styles) string {
	s := f.stats()
	parts := []string{"◌ " + strconv.Itoa(s.waiting) + " waiting"}
	if !s.oldest.IsZero() {
		parts = append(parts, "oldest "+strings.TrimSuffix(agoLabel(f.now, s.oldest), " ago"))
	}
	parts = append(parts, strconv.Itoa(s.startedToday)+" started today")
	if s.hasMedian {
		parts = append(parts, "median "+shortDur(s.median))
	}
	parts = append(parts, "✓ "+strconv.Itoa(s.ok)+" ✗ "+strconv.Itoa(s.failed))
	if s.changed > 0 {
		parts = append(parts, strconv.Itoa(s.changed)+" changed")
	}
	return st.Muted.Render(strings.Join(parts, " · "))
}

// rowLook is a row's glyph, label and style.
func rowLook(st theme.Styles, k rowKind) (glyph, label string, style func(...string) string) {
	switch k {
	case rowRefused:
		return "⊘", "refused", st.Danger.Render
	case rowRunning:
		return "●", "running", st.Active.Render
	case rowLost:
		return "?", "lost", st.Warn.Render
	case rowDone:
		return "✓", "done", st.OK.Render
	case rowFailed:
		return "✗", "failed", st.Danger.Render
	case rowChanged:
		return "!", "changed", st.Warn.Render
	default:
		return "◌", "waiting", st.Accent.Render
	}
}

// itemLabel is "17 merge-when-green-1201": the number, a space, the stem.
func itemLabel(name string) string {
	num, stem, ok := strings.Cut(name, "-")
	if !ok || num == "" || strings.Trim(num, "0123456789") != "" {
		// A legacy done/ name with no NN- number ("07b-cleanup",
		// "operator-grow") is shown whole.
		return deskText(name)
	}
	return deskText(num + " " + stem)
}

// queueWidths returns the name and bar widths for a content width cw.
func queueWidths(width int) (nameW, barW, detailW int) {
	cw := width - 4
	barW, detailW = 16, 12
	if width < deskWideMin {
		barW, detailW = 8, 9
	}
	fixed := 2 + 2 + 9 + 3 + barW + 2 + detailW
	return min(max(cw-fixed, 8), deskNameMax), barW, detailW
}

func (f deskFrame) queuePanel(st theme.Styles, width int, rows []queueRow, cursor, lines int) []string {
	n, tty, _ := f.waiting()
	// A lost run is not running: it is counted on its own (#1106).
	running, lost := 0, 0
	if f.snap != nil {
		for _, it := range f.snap.Running {
			if it.State == desk.StateLost {
				lost++
			} else {
				running++
			}
		}
	}
	strip := st.Active.Render("●") + " " + strconv.Itoa(running) + " running  " + st.Accent.Render("◌") + " " + strconv.Itoa(n) + " waiting"
	if lost > 0 {
		strip += "  " + st.Warn.Render("?") + " " + strconv.Itoa(lost) + " lost"
	}
	if tty > 0 {
		strip += "  " + st.Steel.Render("⌨") + " " + strconv.Itoa(tty) + " tty"
	}
	var content []string
	if len(rows) == 0 {
		// An empty queue says how it fills, not only that it is empty (#1107).
		content = []string{
			st.Muted.Render("nothing waiting"),
			st.Muted.Render("Claude queues scripts with forgectl desk add; they appear here"),
		}
	} else {
		start := 0
		if cursor >= lines {
			start = cursor - lines + 1
		}
		for i := start; i < len(rows) && i < start+lines; i++ {
			content = append(content, f.queueLine(st, width, rows[i], i == cursor))
		}
	}
	return strings.Split(Panel(st, width, "queue", strip, content), "\n")
}

func (f deskFrame) queueLine(st theme.Styles, width int, r queueRow, selected bool) string {
	nameW, barW, detailW := queueWidths(width)
	glyph, label, look := rowLook(st, r.kind)
	pick := "  "
	name := padTo(cut(itemLabel(r.item.Name), nameW), nameW)
	if selected {
		pick = st.Accent.Render("▸ ")
		name = st.Selected.Render(name)
	} else {
		name = st.Fg.Render(name)
	}
	ttyMark := "   "
	if r.item.TTY {
		ttyMark = " " + st.Steel.Render("⌨") + " "
	}
	bar, detail := f.rowBar(st, r, barW)
	return pick + look(glyph) + " " + look(padTo(label, 8)) + " " + name + ttyMark + bar + "  " + cut(detail, detailW)
}

// rowBar draws a row's bar and its detail text.
func (f deskFrame) rowBar(st theme.Styles, r queueRow, w int) (string, string) {
	it := r.item
	switch r.kind {
	case rowWaiting:
		detail := st.Accent.Render("needs you")
		switch {
		case it.Stale:
			detail = st.Warn.Render("stale")
		case it.TTY:
			detail = st.Steel.Render("tty")
		}
		return BarSolid(st, w, 0), detail
	case rowRefused:
		return BarSolid(st, w, 0), st.Danger.Render("refused")
	case rowLost:
		return BarSolid(st, w, 0), st.Warn.Render("lost")
	case rowChanged:
		return BarSolid(st, w, 0), st.Warn.Render("changed")
	case rowDone, rowFailed:
		fill := st
		detail := st.Muted.Render(clock(it.Ended.Sub(it.Started)))
		fill.Active = st.OK
		if r.kind == rowFailed {
			fill.Active = st.Danger
			detail = st.Danger.Render("exit " + strconv.Itoa(*it.ExitCode))
		}
		if it.Started.IsZero() || it.Ended.IsZero() {
			detail = st.Muted.Render("-")
			if r.kind == rowFailed {
				detail = st.Danger.Render("exit " + strconv.Itoa(*it.ExitCode))
			}
		}
		return BarSolid(fill, w, 1), detail
	}
	// running
	elapsed := time.Duration(0)
	if !it.Started.IsZero() {
		elapsed = f.now.Sub(it.Started)
	}
	if it.Kind == desk.KindBatch {
		steps := f.opts.Steps[it.Name]
		segs := make([]Seg, len(steps))
		finished := 0
		for i, s := range steps {
			segs[i] = stepSeg(s.State)
			if s.State != desk.StepPending && s.State != desk.StepRunning {
				finished++
			}
		}
		detail := "starting"
		if len(steps) > 0 {
			detail = fmt.Sprintf("%d/%d steps", finished, len(steps))
		}
		return BarSegments(st, w, segs), st.Active.Render(detail)
	}
	detail := st.Active.Render(clock(elapsed))
	if typical, ok := f.typical(it.Stem); ok && typical > 0 {
		return BarSolid(st, w, float64(elapsed)/float64(typical)), detail
	}
	// No earlier run to compare with: the shimmer is activity, not progress,
	// and the detail says the length so far (#1107).
	if w >= 16 {
		detail = st.Active.Render(clock(elapsed) + " so far")
	}
	return BarShimmer(st, w, int(elapsed/time.Second)), detail
}

// typical is the median length of earlier finished runs with the same stem.
func (f deskFrame) typical(stem string) (time.Duration, bool) {
	if f.snap == nil {
		return 0, false
	}
	var ds []time.Duration
	for _, it := range f.snap.Done {
		if it.Stem == stem && !it.Started.IsZero() && !it.Ended.IsZero() {
			ds = append(ds, it.Ended.Sub(it.Started))
		}
	}
	if len(ds) == 0 {
		return 0, false
	}
	return median(ds), true
}

func stepSeg(state string) Seg {
	switch state {
	case desk.StepOK:
		return SegDone
	case desk.StepRunning:
		return SegRunning
	case desk.StepFailed, desk.StepCancelled:
		return SegFailed
	default:
		return SegWaiting
	}
}

func stepGlyph(st theme.Styles, state string) string {
	switch state {
	case desk.StepOK:
		return st.OK.Render("✓")
	case desk.StepRunning:
		return st.Active.Render("●")
	case desk.StepFailed, desk.StepCancelled:
		return st.Danger.Render("✗")
	case desk.StepSkipped:
		return st.Muted.Render("-")
	default:
		return st.Dim.Render("◌")
	}
}

// focusPanel draws the selected row: what it is, why, and what it runs.
// body is how many script lines or steps to show; 0 leaves the script
// section out entirely.
func (f deskFrame) focusPanel(st theme.Styles, width int, rows []queueRow, cursor, body int) []string {
	if len(rows) == 0 {
		return strings.Split(Panel(st, width, "focus", "", []string{st.Muted.Render("the selected item's sha256, what and why show here")}), "\n")
	}
	r := rows[cursor]
	it := r.item
	label := itemLabel(it.Name)
	var hash string
	// A changed item names both of its hashes in its note instead: the head
	// would show only the one it was queued at (#1106).
	if desk.ValidSHA256(it.Meta.SHA256) && r.kind != rowChanged {
		// The short hash the agent reports and `desk status` prints; the a
		// prompt shows it in full. A long name is cut first so the hash is
		// never the part the panel cuts off. The panel's own cut ends in a
		// "…" cell, so the hash must end one cell short of the edge.
		hash = st.Muted.Render(" · sha256 ") + st.Fg.Render(it.Meta.SHA256[:deskShortHash])
		label = cut(label, max(width-5-ansi.StringWidth(hash), 8))
	}
	head := st.Selected.Render(label) + hash
	head += st.Muted.Render(" · " + f.focusState(r))
	lines := []string{head}
	// A lost or changed item says what happened and what to do next, wrapped
	// under the head so the next step is never the part cut off. The smallest
	// panel (body 0) leaves it out: it is not what y needs, and y, s and u
	// say the same in the footer.
	if note := stateNote(r); note != "" && body > 0 {
		for _, l := range strings.Split(ansi.Wrap(note, max(width-4, 10), ""), "\n") {
			lines = append(lines, st.Warn.Render(strings.TrimRight(l, " ")))
		}
	}
	// The panel's text area is the width less the border and its padding.
	lines = append(lines, wrapField(st, "what", st.Fg.Bold(true), it.What, width-4)...)
	lines = append(lines, wrapField(st, "why", st.Meta, it.Why, width-4)...)
	content := it.Content
	if content == nil {
		content = f.opts.Records[it.Name]
	}
	bar := st.Dim.Render("┆ ")
	switch {
	case body == 0:
		// The smallest panel: the head, what and why, which y needs on screen.
	case r.kind == rowDone || r.kind == rowFailed:
		lines = append(lines, bar+st.Muted.Render("l to view the log"))
	case content == nil:
	case it.Kind == desk.KindBatch:
		lines = append(lines, f.batchLines(st, it.Name, content, body)...)
	default:
		shown, hidden := scriptLines(content, body)
		if len(shown) > 0 {
			lines = append(lines, bar+st.Muted.Render("script · the first lines it runs"))
		}
		for _, l := range shown {
			lines = append(lines, bar+st.Fg.Render(deskText(l)))
		}
		if hidden > 0 {
			lines = append(lines, bar+st.Muted.Render(fmt.Sprintf("… %d more lines · v to view", hidden)))
		}
	}
	return strings.Split(Panel(st, width, "focus", "", lines), "\n")
}

// deskFieldLines caps how many lines `what` or `why` may wrap to, so one
// long header cannot push the script and the history out of a short window.
const deskFieldLines = 4

// wrapField draws "label  text", wrapping text to width under the label
// instead of cutting it. Beyond deskFieldLines the last line ends in "…".
func wrapField(st theme.Styles, label string, text lipgloss.Style, value string, width int) []string {
	if value == "" {
		return nil
	}
	const labelW = 6
	value = deskText(value)
	wrapped := strings.Split(ansi.Wrap(value, max(width-labelW, 10), ""), "\n")
	if len(wrapped) > deskFieldLines {
		wrapped = wrapped[:deskFieldLines]
		wrapped[deskFieldLines-1] = cut(wrapped[deskFieldLines-1]+"…", max(width-labelW, 10))
	}
	out := make([]string, len(wrapped))
	for i, l := range wrapped {
		lead := strings.Repeat(" ", labelW)
		if i == 0 {
			lead = st.Muted.Render(label + strings.Repeat(" ", labelW-len(label)))
		}
		out[i] = lead + text.Render(strings.TrimRight(l, " "))
	}
	return out
}

// focusState is the head line's second half.
func (f deskFrame) focusState(r queueRow) string {
	it := r.item
	switch r.kind {
	case rowWaiting:
		s := "unchanged since queued"
		if it.Meta.AddedAt != nil {
			s += " " + agoLabel(f.now, *it.Meta.AddedAt)
		}
		if it.Stale {
			s += " · stale"
		}
		if it.TTY {
			s += " · runs in this pane"
		}
		return s
	case rowRefused:
		return "refused: " + deskText(it.Refusal)
	case rowRunning:
		if it.Started.IsZero() {
			return "starting"
		}
		return "running " + clock(f.now.Sub(it.Started))
	case rowLost:
		return "lost"
	case rowChanged:
		return "changed"
	}
	outcome := "ok"
	if it.ExitCode == nil {
		outcome = "no exit recorded"
	} else if *it.ExitCode != 0 {
		outcome = "exit " + strconv.Itoa(*it.ExitCode)
	}
	if !it.Started.IsZero() && !it.Ended.IsZero() {
		outcome += " · " + clock(it.Ended.Sub(it.Started))
	}
	return outcome + " · ended " + agoLabel(f.now, it.Ended)
}

// The next step for a lost run and a changed item, shared by the focus
// panel, y's reply and u's reply so the three cannot drift apart.
const (
	lostNext    = "s clears it · l shows its output"
	changedNext = "ask Claude to queue it again"
)

// stateNote is what happened to a lost or changed item and what to do next,
// in plain words; "" for every other row. Both hashes of a changed item are
// shown only when they are valid sha256 values, and the second only when it
// differs from the first.
func stateNote(r queueRow) string {
	switch r.kind {
	case rowLost:
		return "the desk stopped watching it mid-run, so it may have partly run · " + lostNext
	case rowChanged:
		m := r.item.Meta
		hashes := "its bytes changed after it was queued"
		if desk.ValidSHA256(m.SHA256) {
			hashes = "queued " + m.SHA256[:deskShortHash] + ", since changed"
			if desk.ValidSHA256(m.ChangedSHA256) && m.ChangedSHA256 != m.SHA256 {
				hashes = "queued " + m.SHA256[:deskShortHash] + ", now " + m.ChangedSHA256[:deskShortHash]
			}
		}
		return hashes + " · not run, moved to skipped · " + changedNext
	}
	return ""
}

// scriptLines returns the first n lines of a script that are neither blank
// nor comments, and how many more such lines follow.
func scriptLines(data []byte, n int) (shown []string, hidden int) {
	for _, l := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(strings.TrimSuffix(l, "\r"))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if len(shown) < n {
			shown = append(shown, strings.TrimSuffix(l, "\r"))
			continue
		}
		hidden++
	}
	return shown, hidden
}

// batchLines lists a manifest's steps in wave order, each with its state's
// glyph: "┆ 1 ✓ build  make all".
func (f deskFrame) batchLines(st theme.Styles, name string, data []byte, n int) []string {
	bar := st.Dim.Render("┆ ")
	m, err := desk.LoadManifest(data, name+".manifest")
	if err != nil {
		return []string{bar + st.Danger.Render(deskText(err.Error()))}
	}
	state := map[string]string{}
	for _, s := range f.opts.Steps[name] {
		state[s.ID] = s.State
	}
	waves := m.Waves()
	out := []string{bar + st.Muted.Render("order ") + st.Fg.Render(deskText(desk.OrderLine(waves)))}
	total, shown := len(m.Steps), 0
	for wi, wave := range waves {
		for _, id := range wave {
			if shown == n {
				break
			}
			s, _ := m.Step(id)
			out = append(out, bar+st.Muted.Render(strconv.Itoa(wi+1)+" ")+stepGlyph(st, state[id])+" "+
				st.Fg.Render(padTo(deskText(id), 8))+" "+st.Muted.Render(deskText(s.Command)))
			shown++
		}
	}
	if hidden := total - shown; hidden > 0 {
		out = append(out, bar+st.Muted.Render(fmt.Sprintf("… %d more steps · v to view", hidden)))
	}
	return out
}

// historyPanel lists finished runs, most recent first. rows < 0 means up to
// deskHistoryDefault; 0 or less room than a panel needs drops the panel.
func (f deskFrame) historyPanel(st theme.Styles, width, rows int) []string {
	var done []desk.Item
	if f.snap != nil {
		done = f.snap.Done
	}
	if rows < 0 {
		rows = min(max(len(done), 1), deskHistoryDefault)
	}
	if rows < 1 {
		return nil
	}
	var content []string
	if len(done) == 0 {
		content = []string{st.Muted.Render("no finished runs yet")}
	}
	show := done
	if len(show) > rows {
		show = done[:rows-1]
	}
	var longest time.Duration
	for _, it := range show {
		if !it.Started.IsZero() && !it.Ended.IsZero() {
			longest = max(longest, it.Ended.Sub(it.Started))
		}
	}
	for _, it := range show {
		content = append(content, f.historyLine(st, width, it, longest))
	}
	if len(show) < len(done) {
		content = append(content, st.Muted.Render(fmt.Sprintf("… %d older", len(done)-len(show))))
	}
	// The bars have a legend, so they are not read as progress (#1107).
	legend := ""
	if width >= deskWideMin && len(done) > 0 {
		legend = st.Muted.Render("bar: run length, longest full")
	}
	return strings.Split(Panel(st, width, "history", legend, content), "\n")
}

func (f deskFrame) historyLine(st theme.Styles, width int, it desk.Item, longest time.Duration) string {
	cw := width - 4
	spark := 0
	if width >= deskWideMin {
		spark = 8
	}
	const outcomeW, ageW, durW = 16, 9, 7
	nameW := min(max(cw-2-spark-1-outcomeW-1-ageW-1-durW-1, 8), deskNameMax)
	glyph, outcome := st.OK.Render("✓"), st.OK.Render(padTo("ok", outcomeW))
	switch {
	case it.ExitCode == nil:
		glyph, outcome = st.Muted.Render("?"), st.Muted.Render(padTo("no exit recorded", outcomeW))
	case *it.ExitCode != 0:
		glyph, outcome = st.Danger.Render("✗"), st.Danger.Render(padTo("exit "+strconv.Itoa(*it.ExitCode), outcomeW))
	}
	dur, length := "-", time.Duration(0)
	if !it.Started.IsZero() && !it.Ended.IsZero() {
		length = it.Ended.Sub(it.Started)
		dur = clock(length)
	}
	line := glyph + " " + st.Fg.Render(padTo(cut(itemLabel(it.Name), nameW), nameW)) + " "
	if spark > 0 {
		cells := 0
		if longest > 0 && length > 0 {
			cells = max(1, int(float64(spark)*float64(length)/float64(longest)+0.5))
		}
		line += st.Dim.Render(padTo(strings.Repeat("▅", cells), spark)) + " "
	}
	return line + outcome + " " + st.Muted.Render(padTo(agoLabel(f.now, it.Ended), ageW)) + " " + st.Muted.Render(dur)
}

// hints is the key-hint footer for what is on screen, built by deskKeys.
// canRun is false in a window too small to show the focus panel.
func (f deskFrame) hints(st theme.Styles, width int, canRun bool) string {
	rows := deskRows(f.snap, f.now)
	var sel queueRow
	if len(rows) > 0 {
		sel = rows[min(max(f.cursor, 0), len(rows)-1)]
	}
	n, _, _ := f.waiting()
	return deskKeys(st, width, keyState{
		rows:    len(rows) > 0,
		run:     canRun && len(rows) > 0 && sel.kind == rowWaiting,
		skip:    len(rows) > 0 && (sel.kind == rowWaiting || sel.kind == rowRefused || sel.kind == rowLost),
		waiting: n > 0,
	})
}

// keyState is what the keys can act on now.
type keyState struct {
	rows, run, skip, waiting bool
}

// deskBinding is one dashboard key: its footer label, what ? says it does,
// and its place in the footer's drop order (0 is dropped last). The footer
// and the ? screen both come from deskBindings, so they cannot drift (#1108).
type deskBinding struct {
	key, label, help string
	prio             int
}

// deskBindings are the dashboard's keys in footer order.
var deskBindings = []deskBinding{
	{"y", "run", "run the selected item at once; check its short sha256 first", 2},
	{"s", "skip", "skip the selected item, or clear a lost run (asks first)", 4},
	{"u", "undo", "undo the last skip", 6},
	{"v", "view", "view the selected item's script: the bytes y runs", 5},
	{"a", "all", "run every waiting item on screen (asks first, listing each full sha256)", 3},
	{"l", "log", "view the selected run's log", 7},
	{"r", "runs", "open the run view: flow, events, replay", 8},
	{"j/k", "move", "move the selection (also the arrow keys)", 9},
	{"?", "help", "show these keys", 1},
	{"q", "quit", "quit; detached runs keep running", 0},
}

// deskKeys is the key-hint footer from deskBindings: a key shows only when
// it can act on what is on screen (#1107), and a narrow window drops the
// least important first with the shared fitHints, so q quit and ? help are
// always there (#1108). u, l, r and q always can act (u and l answer when
// there is nothing to undo or no log).
func deskKeys(st theme.Styles, width int, s keyState) string {
	gated := map[string]bool{"y": s.run, "s": s.skip, "v": s.rows, "a": s.waiting, "j/k": s.rows}
	var hints []string
	var shown []deskBinding
	for _, b := range deskBindings {
		if ok, isGated := gated[b.key]; isGated && !ok {
			continue
		}
		hints = append(hints, st.Accent.Render(b.key)+" "+st.Muted.Render(b.label))
		shown = append(shown, b)
	}
	prio := make([]int, len(shown))
	for i := range prio {
		prio[i] = i
	}
	slices.SortStableFunc(prio, func(a, b int) int { return shown[a].prio - shown[b].prio })
	return " " + fitHints(width-1, hints, prio)
}

// deskKeyLines is the ? screen: every key and what it does, from
// deskBindings.
func deskKeyLines() []string {
	lines := make([]string, 0, len(deskBindings))
	for _, b := range deskBindings {
		lines = append(lines, fmt.Sprintf("%-4s %s", b.key, b.help))
	}
	return lines
}
