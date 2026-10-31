package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
)

// The worker watch (`forgectl surface watch`): one row per chief-of-staff
// worker, the selected worker's screen below. It only reads. No key here
// sends anything to a worker.

const (
	// WorkersSafetyPoll is the collect interval while hc events are flowing:
	// events do the work, this catches anything they miss.
	WorkersSafetyPoll = 30 * time.Second
	// workersEventGap is the least time between two collects an event starts.
	workersEventGap = time.Second
	// workersTailEvery is how often the selected worker's screen is re-read.
	workersTailEvery = 2 * time.Second
	// workersStopGrace bounds the wait for the hc events child on quit; it
	// covers exec's 2s stream cancel grace.
	workersStopGrace = 3 * time.Second
	// workersTailTimeout bounds one screen read.
	workersTailTimeout = 10 * time.Second
	// workersMaxTailLines caps the screen lines kept for the tail.
	workersMaxTailLines = 200
	// workersMaxCellRunes caps one escaped table cell before it is cut to
	// its column width.
	workersMaxCellRunes = 200
	// workersMaxBanners caps the NEEDS YOU lines, so many blocked workers
	// cannot push the table off the screen.
	workersMaxBanners = 3
	// workersMaxTailLineRunes caps one escaped screen line.
	workersMaxTailLineRunes = 400

	wwName   = 22
	wwAddr   = 5
	wwStatus = 9
	wwAge    = 4
	wwModel  = 10
	wwReport = 26
	// wwReportShort is REPORTS without the file name ("1/2 ✓").
	wwReportShort = 7
	// wwNameMin is the narrowest NAME gets before other columns drop.
	wwNameMin = 12
	wwReview  = 12
	wwPR      = 16
	wwCI      = 6
)

// WorkersOptions configures RunWorkers.
type WorkersOptions struct {
	// Collect reads every worker once. force asks for fresher GitHub reads.
	Collect func(ctx context.Context, force bool) workerwatch.Snapshot
	// Read returns a terminal's screen; its text is untrusted.
	Read func(ctx context.Context, address string) (string, error)
	// Events, when set, starts the hc event subscription. RunWorkers
	// cancels it and waits for its Done before returning.
	Events func(ctx context.Context) workerwatch.EventStream
	// Poll is the collect interval when there is no event stream.
	Poll  time.Duration
	Theme theme.Theme
}

// RunWorkers runs the worker watch until the operator quits. It returns
// only after the hc event subscription has stopped (or workersStopGrace has
// passed), so quitting never leaves an `hc events` child behind.
func RunWorkers(ctx context.Context, opts WorkersOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stream *workerwatch.EventStream
	if opts.Events != nil {
		es := opts.Events(ctx)
		stream = &es
	}
	_, err := tea.NewProgram(newWorkersModel(ctx, opts, stream), keymap.ProgramOptions(ctx)...).Run()
	if stream != nil {
		stopStream(cancel, stream.Done, workersStopGrace)
	}
	return err
}

// stopStream cancels the subscription and waits for its child to be
// reaped, at most grace. It reports whether the child was reaped in time.
func stopStream(cancel context.CancelFunc, done <-chan struct{}, grace time.Duration) bool {
	cancel()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}

type (
	workersSnapMsg   struct{ snap workerwatch.Snapshot }
	workersTickMsg   struct{}
	workersKickMsg   struct{}
	workersEventMsg  struct{}
	workersEventsEnd struct{}
	workersTailMsg   struct {
		addr  string
		lines []string
		err   error
	}
	workersTailTickMsg struct{}
)

type workersModel struct {
	ctx    context.Context
	opts   WorkersOptions
	styles theme.Styles
	now    func() time.Time

	snap         workerwatch.Snapshot
	loaded       bool
	busy         bool
	pending      bool
	pendingForce bool
	lastStart    time.Time

	events   <-chan struct{}
	eventsUp bool

	cursor        int
	width, height int
	help          bool

	tailAddr  string
	tailLines []string
	tailErr   string
	tailBusy  bool
}

func newWorkersModel(ctx context.Context, opts WorkersOptions, stream *workerwatch.EventStream) workersModel {
	if opts.Poll <= 0 {
		opts.Poll = 5 * time.Second
	}
	// Init always starts the first collect, so the model starts busy.
	m := workersModel{ctx: ctx, opts: opts, styles: opts.Theme.Styles(), now: time.Now, busy: true}
	m.lastStart = m.now()
	if stream != nil {
		m.events = stream.Wake
		m.eventsUp = true
	}
	return m
}

func (m workersModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.collectCmd(false), m.scheduleTick(), m.scheduleTail()}
	if m.events != nil {
		cmds = append(cmds, waitEvent(m.events))
	}
	return tea.Batch(cmds...)
}

func (m workersModel) collectCmd(force bool) tea.Cmd {
	collect, ctx := m.opts.Collect, m.ctx
	return func() tea.Msg { return workersSnapMsg{snap: collect(ctx, force)} }
}

// requestCollect starts a collect, or queues one if a collect is running.
func (m *workersModel) requestCollect(force bool) tea.Cmd {
	if m.busy {
		m.pending = true
		m.pendingForce = m.pendingForce || force
		return nil
	}
	m.busy = true
	m.lastStart = m.now()
	return m.collectCmd(force)
}

func (m workersModel) scheduleTick() tea.Cmd {
	every := m.opts.Poll
	if m.eventsUp {
		every = WorkersSafetyPoll
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return workersTickMsg{} })
}

func (m workersModel) scheduleTail() tea.Cmd {
	return tea.Tick(workersTailEvery, func(time.Time) tea.Msg { return workersTailTickMsg{} })
}

func waitEvent(ch <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-ch; !ok {
			return workersEventsEnd{}
		}
		return workersEventMsg{}
	}
}

// readTail reads the selected worker's screen, unless a read is running or
// the row has no live terminal.
func (m *workersModel) readTail() tea.Cmd {
	w, ok := m.selected()
	if !ok || w.Address == "" || w.State != "live" || m.opts.Read == nil {
		m.tailAddr, m.tailLines, m.tailErr = "", nil, ""
		return nil
	}
	if m.tailBusy {
		return nil
	}
	m.tailBusy = true
	addr, read, ctx := w.Address, m.opts.Read, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, workersTailTimeout)
		defer cancel()
		text, err := read(ctx, addr)
		return workersTailMsg{addr: addr, lines: SanitizeScreen(text), err: err}
	}
}

func (m workersModel) selected() (workerwatch.Worker, bool) {
	if m.cursor < 0 || m.cursor >= len(m.snap.Workers) {
		return workerwatch.Worker{}, false
	}
	return m.snap.Workers[m.cursor], true
}

func (m workersModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		return m, nil
	case workersSnapMsg:
		prevAddr := m.tailAddr
		prevName := ""
		if w, ok := m.selected(); ok {
			prevName = w.Name
		}
		m.snap, m.loaded, m.busy = t.snap, true, false
		m.cursor = min(m.cursor, max(len(m.snap.Workers)-1, 0))
		// Keep the operator's worker selected when rows are added or removed.
		for i, w := range m.snap.Workers {
			if prevName != "" && w.Name == prevName {
				m.cursor = i
				break
			}
		}
		var cmds []tea.Cmd
		if m.pending {
			force := m.pendingForce
			m.pending, m.pendingForce = false, false
			if wait := workersEventGap - m.now().Sub(m.lastStart); wait > 0 && !force {
				m.pending = true
				cmds = append(cmds, tea.Tick(wait, func(time.Time) tea.Msg { return workersKickMsg{} }))
			} else {
				cmds = append(cmds, m.requestCollect(force))
			}
		}
		if w, ok := m.selected(); !ok || w.Address != prevAddr {
			cmds = append(cmds, m.readTail())
		}
		return m, tea.Batch(cmds...)
	case workersKickMsg:
		if !m.pending || m.busy {
			return m, nil
		}
		force := m.pendingForce
		m.pending, m.pendingForce = false, false
		return m, m.requestCollect(force)
	case workersTickMsg:
		return m, tea.Batch(m.requestCollect(false), m.scheduleTick())
	case workersEventMsg:
		var cmd tea.Cmd
		if wait := workersEventGap - m.now().Sub(m.lastStart); m.busy || wait > 0 {
			m.pending = true
			if !m.busy {
				cmd = tea.Tick(wait, func(time.Time) tea.Msg { return workersKickMsg{} })
			}
		} else {
			cmd = m.requestCollect(false)
		}
		return m, tea.Batch(cmd, waitEvent(m.events))
	case workersEventsEnd:
		m.eventsUp, m.events = false, nil
		return m, m.requestCollect(false)
	case workersTailTickMsg:
		return m, tea.Batch(m.readTail(), m.scheduleTail())
	case workersTailMsg:
		m.tailBusy = false
		if w, ok := m.selected(); !ok || w.Address != t.addr {
			return m, m.readTail() // the selection moved while it read
		}
		m.tailAddr, m.tailLines, m.tailErr = t.addr, t.lines, ""
		if t.err != nil {
			m.tailErr = "the screen could not be read"
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(t)
	}
	return m, nil
}

func (m workersModel) updateKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "?":
		m.help = !m.help
		return m, nil
	case "esc":
		m.help = false // esc closes help; it never quits
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			return m, m.readTail()
		}
	case "down", "j":
		if m.cursor < len(m.snap.Workers)-1 {
			m.cursor++
			return m, m.readTail()
		}
	case "home", "g":
		m.cursor = 0
		return m, m.readTail()
	case "end", "G":
		m.cursor = max(len(m.snap.Workers)-1, 0)
		return m, m.readTail()
	case "r":
		return m, m.requestCollect(true)
	}
	return m, nil
}

// SanitizeScreen turns untrusted terminal text into inert display lines:
// escape sequences (colour, cursor moves, OSC 52 clipboard writes,
// hyperlinks, screen clears) are removed, and every control or bidi
// character left is shown escaped. Trailing blank lines are dropped and at
// most the last workersMaxTailLines are kept.
func SanitizeScreen(text string) []string {
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "�")
	}
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(raw) > workersMaxTailLines*2 {
		raw = raw[len(raw)-workersMaxTailLines*2:]
	}
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		l = ansi.Strip(l)
		l = strings.ReplaceAll(l, "\t", "    ")
		l = strings.TrimRight(termsafe.SafeLineMax(l, workersMaxTailLineRunes), " ")
		lines = append(lines, l)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > workersMaxTailLines {
		lines = lines[len(lines)-workersMaxTailLines:]
	}
	return lines
}

// --- view ---

// workersMinRows is the smallest terminal the view lays out in; below it
// the view says so instead of cutting the header or keys off.
const workersMinRows = 7

func (m workersModel) View() tea.View {
	width, height := m.width, m.height
	if width <= 0 {
		width = 200
	}
	if height <= 0 {
		height = 30
	}
	var content string
	switch {
	case height < workersMinRows:
		content = m.styles.Warn.Render(fmt.Sprintf("terminal too small (%d rows; need %d) · q quit", height, workersMinRows))
	case m.help:
		content = strings.Join(clipLines(m.helpLines(), height), "\n")
	default:
		content = strings.Join(m.mainLines(width, height), "\n")
	}
	content = lipgloss.NewStyle().MaxWidth(width).Render(content)
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m workersModel) mainLines(width, height int) []string {
	cols, hidden := workersLayout(width)
	top := []string{m.headerLine()}
	top = append(top, m.bannerLines(width, height)...)
	top = append(top, m.columnsLine(cols, hidden))
	rows := make([]string, 0, len(m.snap.Workers))
	for i, w := range m.snap.Workers {
		rows = append(rows, m.rowLine(cols, i == m.cursor, w))
	}
	footer := m.footerLines()

	// The table gets what it needs up to half the screen; the screen tail
	// gets the rest.
	room := max(height-len(top)-len(footer), 1)
	tableRoom := min(len(rows), max(room/2, min(3, room)))
	if len(rows) == 0 {
		tableRoom = 1
		rows = []string{m.styles.Muted.Render(m.emptyText())}
	}
	start := 0
	if m.cursor >= tableRoom {
		start = m.cursor - tableRoom + 1
	}
	rows = rows[start:min(len(rows), start+tableRoom)]
	tail := m.tailBlock(room - len(rows))
	for len(rows)+len(tail) < room {
		tail = append(tail, "")
	}
	return clipLines(append(append(append(top, rows...), tail...), footer...), height)
}

// clipLines keeps at most n lines.
func clipLines(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	return lines
}

func (m workersModel) emptyText() string {
	if !m.loaded {
		return "  reading workers…"
	}
	return "  no workers: the workers file is empty and --cos found no cos-* terminal"
}

func (m workersModel) headerLine() string {
	brand := m.styles.Brand.Render("◆") + " " + m.styles.Header.Render(meta.AppName+" surface watch")
	// The quit and help keys lead, so a narrow or short terminal that cuts
	// the line's end or loses the footer still shows them.
	parts := []string{"q quit · ? help", fmt.Sprintf("%d workers", len(m.snap.Workers))}
	if n := len(m.snap.NeedingYou()); n > 0 {
		parts = append(parts, m.styles.Danger.Render(fmt.Sprintf("%d need you", n)))
	}
	if m.eventsUp {
		parts = append(parts, "live (hc events)")
	} else {
		parts = append(parts, fmt.Sprintf("polling every %s", m.opts.Poll))
	}
	switch {
	case m.busy && !m.loaded:
		parts = append(parts, "loading…")
	case m.busy:
		parts = append(parts, "refreshing…")
	case m.loaded:
		if age := ageLabel(m.now().Sub(m.snap.Taken)); age != "" {
			parts = append(parts, "updated "+age+" ago")
		}
	}
	return brand + "  " + m.styles.Muted.Render(strings.Join(parts, " · "))
}

// bannerLines is the needs-you banner: one line per blocked worker, naming
// the command that shows its dialog, at most workersMaxBanners lines (two
// on a short terminal). The view never answers a dialog.
func (m workersModel) bannerLines(width, height int) []string {
	limit := workersMaxBanners
	if height < 14 {
		limit = 1
	}
	needing := m.snap.NeedingYou()
	shown := needing
	if len(needing) > limit {
		shown = needing[:limit-1]
	}
	var out []string
	for _, w := range shown {
		addr := w.Address
		if addr == "" {
			addr = w.Name
		}
		a := capSafe(addr, 32)
		msg := fmt.Sprintf(" NEEDS YOU  %s is blocked: a dialog is waiting on its screen. See it: hc read %s · answer it: hc attach %s ", capSafe(w.Name, wwName), a, a)
		if lipgloss.Width(msg) > width {
			msg = fmt.Sprintf(" NEEDS YOU  %s blocked · hc read %s ", capSafe(w.Name, wwName), a)
		}
		out = append(out, m.styles.Danger.Reverse(true).Render(msg))
	}
	if rest := needing[len(shown):]; len(rest) > 0 {
		names := make([]string, 0, 3)
		for _, w := range rest[:min(len(rest), 3)] {
			names = append(names, capSafe(w.Name, wwName))
		}
		more := ""
		if len(rest) > 3 {
			more = ", …"
		}
		out = append(out, m.styles.Danger.Reverse(true).Render(fmt.Sprintf(" NEEDS YOU  %d more blocked: %s%s (BLOCKED rows; ↑/↓ to reach them) ", len(rest), strings.Join(names, ", "), more)))
	}
	return out
}

// workersCol is one table column. Columns are shown in kind order and
// dropped by prio (highest number first) when the terminal is narrow.
type workersCol struct {
	kind  int
	title string
	width int
	prio  int
}

const (
	colName = iota
	colStatus
	colAge
	colAddr
	colModel
	colReports
	colReportsShort
	colPR
	colCI
	colReview
)

var workersAllCols = []workersCol{
	{colName, "NAME", wwName, 1},
	{colStatus, "STATUS*", wwStatus, 2},
	{colAge, "AGE", wwAge, 7},
	{colAddr, "HC", wwAddr, 8},
	{colModel, "MODEL", wwModel, 9},
	{colReports, "REPORTS", wwReport, 5},
	{colPR, "PR", wwPR, 6},
	{colCI, "CI", wwCI, 4},
	{colReview, "REVIEW*", wwReview, 3},
}

// workersVerdictPrio is the lowest priority NAME narrows for: NAME,
// STATUS, REVIEW, CI and REPORTS.
const workersVerdictPrio = 5

// workersHintWidth is the room the "→ N more columns" hint takes.
const workersHintWidth = 17

// workersLayout picks the columns that fit width, by priority, and how many
// were left out. REPORTS falls back to its short form before it is dropped.
// NAME first gets its full width; if that would push out a verdict column
// (STATUS, REVIEW, CI), NAME narrows to wwNameMin and takes back whatever
// room is left.
func workersLayout(width int) ([]workersCol, int) {
	fit := func(avail, nameWidth int) ([]workersCol, bool) {
		byPrio := slices.Clone(workersAllCols)
		slices.SortStableFunc(byPrio, func(a, b workersCol) int { return a.prio - b.prio })
		used := 2 // the selection pointer
		var keep []workersCol
		lostVerdict := false
		for _, c := range byPrio {
			if c.kind == colName {
				c.width = nameWidth
			}
			if c.kind == colReports && used+c.width+1 > avail {
				c = workersCol{colReportsShort, "REPORTS", wwReportShort, c.prio}
			}
			if used+c.width+1 > avail && c.kind != colName {
				lostVerdict = lostVerdict || c.prio <= workersVerdictPrio
				continue
			}
			keep = append(keep, c)
			used += c.width + 1
		}
		// Give NAME any room left, up to its full width.
		for i := range keep {
			if keep[i].kind == colName {
				keep[i].width = min(wwName, keep[i].width+max(avail-used, 0))
			}
		}
		slices.SortStableFunc(keep, func(a, b workersCol) int { return a.kind - b.kind })
		return keep, lostVerdict
	}
	layout := func(avail int) []workersCol {
		cols, lost := fit(avail, wwName)
		if lost {
			cols, _ = fit(avail, wwNameMin)
		}
		return cols
	}
	cols := layout(width)
	if len(cols) < len(workersAllCols) {
		cols = layout(width - workersHintWidth)
	}
	return cols, len(workersAllCols) - len(cols)
}

func (m workersModel) columnsLine(cols []workersCol, hidden int) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, fitCell(c.title, c.width))
	}
	line := m.styles.Muted.Render("  " + strings.Join(parts, " "))
	if hidden > 0 {
		line += " " + m.styles.Warn.Render(fmt.Sprintf("→ %d more columns", hidden))
	}
	return line
}

func (m workersModel) rowLine(cols []workersCol, selected bool, w workerwatch.Worker) string {
	pointer := "  "
	if selected {
		pointer = m.styles.Accent.Render("▸ ")
	}
	cells := make([]string, 0, len(cols))
	for _, c := range cols {
		text, style := m.cell(c.kind, selected, w)
		cells = append(cells, style.Render(fitCell(text, c.width)))
	}
	return pointer + strings.Join(cells, " ")
}

// cell is one column's plain text and style for w. Worker text is escaped
// here; fitCell then cuts it by display width.
func (m workersModel) cell(kind int, selected bool, w workerwatch.Worker) (string, lipgloss.Style) {
	s := m.styles
	switch kind {
	case colName:
		style := s.Fg
		switch {
		case w.NeedsYou():
			style = s.Danger
		case selected:
			style = s.Selected
		}
		return safeText(w.Name), style
	case colStatus:
		return m.statusCell(w)
	case colAge:
		return ageCell(w.AgeSeconds), s.Muted
	case colAddr:
		return dash(safeText(w.Address)), s.Meta
	case colModel:
		return dash(safeText(w.Model)), s.Muted
	case colReports, colReportsShort:
		return m.reportCell(w, kind == colReportsShort)
	case colPR:
		return m.prCell(w)
	case colCI:
		return m.ciCell(w)
	case colReview:
		return m.reviewCell(w)
	}
	return "", s.Muted
}

func (m workersModel) statusCell(w workerwatch.Worker) (string, lipgloss.Style) {
	s := m.styles
	switch {
	case w.HCError != "":
		return "gone", s.Warn
	case w.State == "finished":
		return "exited", s.Muted
	case w.Status == workerwatch.StatusWorking:
		return w.Status, s.Active
	case w.Status == workerwatch.StatusDone:
		return w.Status, s.OK
	case w.Status == workerwatch.StatusBlocked:
		return "BLOCKED", s.Danger
	case w.Status == workerwatch.StatusError:
		return w.Status, s.Danger
	}
	return dash(w.Status), s.Muted
}

func (m workersModel) reportCell(w workerwatch.Worker, short bool) (string, lipgloss.Style) {
	s := m.styles
	r := w.Reports
	newest, ok := r.Newest()
	switch {
	case w.BriefDir == "":
		return "—", s.Dim
	case r.Error != "":
		return "unreadable", s.Warn
	case !ok && short:
		return "none", s.Muted
	case !ok:
		return "none yet", s.Muted
	}
	text := fmt.Sprintf("%d/%d", r.CompleteCount(), len(r.Files))
	if !short {
		text += " " + safeText(newest.Name)
	}
	if newest.Complete {
		return text + " ✓", s.OK
	}
	return text + " …", s.Warn
}

func (m workersModel) prCell(w workerwatch.Worker) (string, lipgloss.Style) {
	s := m.styles
	if w.PR == nil {
		switch w.PRIssue {
		case workerwatch.PRIssueNoPR:
			return "no PR yet", s.Muted
		case workerwatch.PRIssueGH:
			return "gh error", s.Warn
		case workerwatch.PRIssueNotGitHub:
			return "not GitHub", s.Muted
		case workerwatch.PRIssueNoOrigin:
			return "no origin", s.Muted
		case workerwatch.PRIssueNoBranch:
			return "no branch", s.Muted
		}
		if w.Worktree != "" && w.PRAt.IsZero() {
			return "…", s.Muted
		}
		return "—", s.Muted
	}
	text := fmt.Sprintf("#%d", w.PR.Number)
	if len(w.PR.Head) >= 7 {
		text += " " + w.PR.Head[:7]
	}
	if w.PRIssue == workerwatch.PRIssueGH {
		// The last good read, kept beside a failed one: say so.
		return text + " !", s.Warn
	}
	switch w.PR.State {
	case "MERGED":
		return text, s.OK
	case "CLOSED":
		return text, s.Muted
	}
	return text, s.Accent
}

func (m workersModel) ciCell(w workerwatch.Worker) (string, lipgloss.Style) {
	s := m.styles
	if w.PR == nil {
		return "", s.Muted
	}
	switch w.PR.Checks {
	case "SUCCESS":
		return "✓ pass", s.OK
	case "FAILURE", "ERROR":
		return "✗ fail", s.Danger
	case "PENDING", "EXPECTED":
		return "… run", s.Warn
	}
	return "—", s.Muted
}

// reviewCell is the compact verdict: sweep's sum over the markers at head,
// "c<crit> i<imp>", with "×N" when N markers count. Reviewer names are in
// --json and the help screen explains the rule.
func (m workersModel) reviewCell(w workerwatch.Worker) (string, lipgloss.Style) {
	s := m.styles
	switch {
	case w.PR == nil:
		return "", s.Muted
	case w.PR.Review == nil:
		return "none", s.Muted
	case w.PR.Review.AtHead == 0:
		return "stale", s.Muted
	}
	r := w.PR.Review
	text := fmt.Sprintf("c%d i%d", r.Crit, r.Imp)
	if r.AtHead > 1 {
		text += fmt.Sprintf(" ×%d", r.AtHead)
	}
	switch {
	case r.Crit > 0:
		return text, s.Danger
	case r.Imp > 0:
		return text, s.Warn
	}
	return text, s.OK
}

// tailBlock is the selected worker's screen: a rule naming it, then the
// last lines that fit in room.
func (m workersModel) tailBlock(room int) []string {
	if room < 2 {
		return nil
	}
	w, ok := m.selected()
	if !ok {
		return nil
	}
	title := fmt.Sprintf("── screen: %s", capSafe(w.Name, wwName))
	if w.Address != "" {
		a := capSafe(w.Address, 32)
		title += fmt.Sprintf(" (%s) · hc read %s · hc attach %s to take over", a, a, a)
	}
	out := []string{m.styles.Steel.Render(title + " ──")}
	body := room - 1
	var lines []string
	switch {
	case w.HCError != "":
		lines = []string{m.styles.Warn.Render("  " + safeText(w.HCError))}
	case w.State == "finished":
		lines = []string{m.styles.Muted.Render("  the terminal has exited")}
	case m.tailErr != "" && m.tailAddr == w.Address:
		lines = []string{m.styles.Warn.Render("  " + m.tailErr)}
	case m.tailAddr != w.Address:
		lines = []string{m.styles.Muted.Render("  reading the screen…")}
	default:
		lines = m.tailLines
		if len(lines) > body {
			lines = lines[len(lines)-body:]
		}
	}
	return append(out, lines...)
}

func (m workersModel) footerLines() []string {
	keys := m.styles.Muted.Render("↑/↓ select · r refresh · ? help · q quit · * = a claim, see ?")
	if len(m.snap.Notes) == 0 {
		return []string{keys}
	}
	note := capSafe(m.snap.Notes[0], 160)
	if more := len(m.snap.Notes) - 1; more > 0 {
		note += fmt.Sprintf(" (+%d more)", more)
	}
	return []string{m.styles.Warn.Render("note: " + note), keys}
}

// helpLines is the ? screen.
func (m workersModel) helpLines() []string {
	s := m.styles
	lines := []string{
		s.Header.Render("surface watch: keys and what the columns mean"),
		"",
		"  ↑/↓ or j/k   select a worker; its screen shows below the table",
		"  g / G        first / last worker",
		"  r            refresh now (GitHub reads older than 15s too)",
		"  ?            this help; ? or esc closes it",
		"  q, ctrl+c    quit",
		"",
		"  STATUS*  the agent's own report through hc (OSC 7501), not an observation.",
		"  REVIEW*  cadence-review markers summed the way forgectl sweep sums them:",
		"           c<crit> i<imp> over every marker at the PR's head, ×N markers;",
		"           only [sweep] reviewers count when that list is set. Markers are",
		"           claims: any session on your gh login, a worker included, can post one.",
		"  PR !     the last good read, kept because the newest GitHub read failed.",
		"  → N more columns  widen the terminal to see the rest (or use --json).",
		"",
		"  The watch only reads: it never types into, kills, or briefs a worker.",
	}
	for _, n := range m.snap.Notes {
		lines = append(lines, s.Warn.Render("  note: "+capSafe(n, 300)))
	}
	return lines
}

// safeText escapes worker-supplied text for one terminal line.
func safeText(s string) string { return termsafe.SafeLineMax(strings.TrimSpace(s), workersMaxCellRunes) }

// fitCell cuts s to n display cells (an ellipsis marks a cut) and pads it
// to exactly n, so wide runes cannot shift the columns after it.
func fitCell(s string, n int) string {
	if lipgloss.Width(s) > n {
		s = ansi.Truncate(s, n, "…")
	}
	if w := lipgloss.Width(s); w < n {
		s += strings.Repeat(" ", n-w)
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ageCell is a terminal's age, or a dash when hc gave no start time.
func ageCell(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}
	return ageLabel(time.Duration(seconds) * time.Second)
}
