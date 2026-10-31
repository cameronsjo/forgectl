package tui

import (
	"context"
	"fmt"
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
	// workersTailTimeout bounds one screen read.
	workersTailTimeout = 10 * time.Second
	// workersMaxTailLines caps the screen lines kept for the tail.
	workersMaxTailLines = 200
	// workersMaxTailLineRunes caps one escaped screen line.
	workersMaxTailLineRunes = 400

	wwName   = 22
	wwAddr   = 5
	wwStatus = 9
	wwAge    = 4
	wwModel  = 10
	wwReport = 26
	wwPR     = 14
	wwCI     = 6
)

// WorkersOptions configures RunWorkers.
type WorkersOptions struct {
	// Collect reads every worker once. force asks for fresher GitHub reads.
	Collect func(ctx context.Context, force bool) workerwatch.Snapshot
	// Read returns a terminal's screen; its text is untrusted.
	Read func(ctx context.Context, address string) (string, error)
	// Events, when set, signals each time hc reports a change, and closes
	// when the stream ends.
	Events func(ctx context.Context) <-chan struct{}
	// Poll is the collect interval when there is no event stream.
	Poll  time.Duration
	Theme theme.Theme
}

// RunWorkers runs the worker watch until the operator quits.
func RunWorkers(ctx context.Context, opts WorkersOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(newWorkersModel(ctx, opts), keymap.ProgramOptions(ctx)...).Run()
	return err
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

	tailAddr  string
	tailLines []string
	tailErr   string
	tailBusy  bool
}

func newWorkersModel(ctx context.Context, opts WorkersOptions) workersModel {
	if opts.Poll <= 0 {
		opts.Poll = 5 * time.Second
	}
	// Init always starts the first collect, so the model starts busy.
	m := workersModel{ctx: ctx, opts: opts, styles: opts.Theme.Styles(), now: time.Now, busy: true}
	m.lastStart = m.now()
	if opts.Events != nil {
		m.events = opts.Events(ctx)
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
		m.snap, m.loaded, m.busy = t.snap, true, false
		m.cursor = min(m.cursor, max(len(m.snap.Workers)-1, 0))
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
	case "ctrl+c", "q", "esc":
		return m, tea.Quit
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

func (m workersModel) View() tea.View {
	height := max(m.height, 12)
	var top []string
	top = append(top, m.headerLine())
	if banner := m.bannerLines(); len(banner) > 0 {
		top = append(top, banner...)
	}
	top = append(top, m.styles.Muted.Render(workersColumns()))
	rows := m.rowLines()
	footer := m.footerLines()

	// The table gets what it needs up to half the screen; the screen tail
	// gets the rest.
	room := height - len(top) - len(footer)
	tableRoom := min(len(rows), max(room/2, 3))
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

	lines := append(append(append(top, rows...), tail...), footer...)
	content := strings.Join(lines, "\n")
	if m.width > 0 {
		content = lipgloss.NewStyle().MaxWidth(m.width).Render(content)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m workersModel) emptyText() string {
	switch {
	case !m.loaded:
		return "  reading workers…"
	default:
		return "  no workers: the workers file is empty and --cos found no cos-* terminal"
	}
}

func (m workersModel) headerLine() string {
	brand := m.styles.Brand.Render("◆") + " " + m.styles.Header.Render(meta.AppName+" surface watch")
	parts := []string{fmt.Sprintf("%d workers", len(m.snap.Workers))}
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
// the command that shows its dialog. The view never answers it.
func (m workersModel) bannerLines() []string {
	var out []string
	for _, w := range m.snap.NeedingYou() {
		addr := w.Address
		if addr == "" {
			addr = w.Name
		}
		msg := fmt.Sprintf(" NEEDS YOU  %s is blocked: a dialog is waiting on its screen. See it: hc read %s · answer it: hc attach %s ",
			capSafe(w.Name, wwName), capSafe(addr, 32), capSafe(addr, 32))
		out = append(out, m.styles.Danger.Reverse(true).Render(msg))
	}
	return out
}

func workersColumns() string {
	return "  " + padRight("NAME", wwName) + " " + padRight("HC", wwAddr) + " " + padRight("STATUS*", wwStatus) + " " +
		padRight("AGE", wwAge) + " " + padRight("MODEL", wwModel) + " " + padRight("REPORTS", wwReport) + " " +
		padRight("PR", wwPR) + " " + padRight("CI", wwCI) + " REVIEW"
}

func (m workersModel) rowLines() []string {
	out := make([]string, 0, len(m.snap.Workers))
	for i, w := range m.snap.Workers {
		out = append(out, m.rowLine(i == m.cursor, w))
	}
	return out
}

func (m workersModel) rowLine(selected bool, w workerwatch.Worker) string {
	s := m.styles
	pointer := "  "
	if selected {
		pointer = s.Accent.Render("▸ ")
	}
	nameStyle := s.Fg
	if selected {
		nameStyle = s.Selected
	}
	if w.NeedsYou() {
		nameStyle = s.Danger
	}
	cells := []string{
		nameStyle.Render(padRight(capSafe(w.Name, wwName), wwName)),
		s.Meta.Render(padRight(capSafe(dash(w.Address), wwAddr), wwAddr)),
		m.statusCell(w),
		s.Muted.Render(padRight(ageCell(w.AgeSeconds), wwAge)),
		s.Muted.Render(padRight(capSafe(dash(w.Model), wwModel), wwModel)),
		m.reportCell(w),
		m.prCell(w),
		m.ciCell(w),
		m.reviewCell(w),
	}
	return pointer + strings.Join(cells, " ")
}

func (m workersModel) statusCell(w workerwatch.Worker) string {
	s := m.styles
	text, style := dash(w.Status), s.Muted
	switch {
	case w.HCError != "":
		text, style = "gone", s.Warn
	case w.State == "finished":
		text, style = "exited", s.Muted
	case w.Status == workerwatch.StatusWorking:
		style = s.Active
	case w.Status == workerwatch.StatusDone:
		style = s.OK
	case w.Status == workerwatch.StatusBlocked:
		text, style = "BLOCKED", s.Danger
	case w.Status == workerwatch.StatusError:
		style = s.Danger
	}
	return style.Render(padRight(text, wwStatus))
}

func (m workersModel) reportCell(w workerwatch.Worker) string {
	s := m.styles
	r := w.Reports
	newest, ok := r.Newest()
	switch {
	case w.BriefDir == "":
		return s.Dim.Render(padRight("—", wwReport))
	case r.Error != "":
		return s.Warn.Render(padRight("unreadable", wwReport))
	case !ok:
		return s.Muted.Render(padRight("none yet", wwReport))
	}
	text := fmt.Sprintf("%d/%d %s", r.CompleteCount(), len(r.Files), capSafe(newest.Name, wwReport-8))
	if newest.Complete {
		return s.OK.Render(padRight(text+" ✓", wwReport))
	}
	return s.Warn.Render(padRight(text+" …", wwReport))
}

func (m workersModel) prCell(w workerwatch.Worker) string {
	s := m.styles
	if w.PR == nil {
		note := "—"
		switch {
		case w.PRNote == "no PR yet":
			note = "no PR yet"
		case w.PRNote != "":
			note = "?"
		case w.Worktree != "" && w.PRAt.IsZero():
			note = "…"
		}
		return s.Muted.Render(padRight(note, wwPR))
	}
	text := fmt.Sprintf("#%d", w.PR.Number)
	if len(w.PR.Head) >= 7 {
		text += " " + w.PR.Head[:7]
	}
	style := s.Accent
	switch w.PR.State {
	case "MERGED":
		style = s.OK
	case "CLOSED":
		style = s.Muted
	}
	return style.Render(padRight(text, wwPR))
}

func (m workersModel) ciCell(w workerwatch.Worker) string {
	s := m.styles
	if w.PR == nil {
		return padRight("", wwCI)
	}
	switch w.PR.Checks {
	case "SUCCESS":
		return s.OK.Render(padRight("✓ pass", wwCI))
	case "FAILURE", "ERROR":
		return s.Danger.Render(padRight("✗ fail", wwCI))
	case "PENDING", "EXPECTED":
		return s.Warn.Render(padRight("… run", wwCI))
	}
	return s.Muted.Render(padRight("—", wwCI))
}

func (m workersModel) reviewCell(w workerwatch.Worker) string {
	s := m.styles
	if w.PR == nil || w.PR.Marker == nil {
		if w.PR != nil {
			return s.Muted.Render("no marker")
		}
		return ""
	}
	mk := w.PR.Marker
	text := fmt.Sprintf("%s crit=%d imp=%d", capSafe(mk.Reviewer, 20), mk.Crit, mk.Imp)
	switch {
	case !mk.AtHead:
		return s.Muted.Render(text + " stale")
	case mk.Crit > 0:
		return s.Danger.Render(text + " @head")
	case mk.Imp > 0:
		return s.Warn.Render(text + " @head")
	}
	return s.OK.Render(text + " @head")
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
		title += fmt.Sprintf(" (%s) · hc read %s · hc attach %s to take over", capSafe(w.Address, 32), capSafe(w.Address, 32), capSafe(w.Address, 32))
	}
	out := []string{m.styles.Steel.Render(title + " ──")}
	body := room - 1
	var lines []string
	switch {
	case w.HCError != "":
		lines = []string{m.styles.Warn.Render("  " + w.HCError)}
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
	keys := m.styles.Muted.Render("↑/↓ select · r refresh · q quit · * status is the agent's own report (hc), not an observation")
	if len(m.snap.Notes) == 0 {
		return []string{keys}
	}
	note := capSafe(m.snap.Notes[0], 160)
	if more := len(m.snap.Notes) - 1; more > 0 {
		note += fmt.Sprintf(" (+%d more)", more)
	}
	return []string{m.styles.Warn.Render("note: " + note), keys}
}

// padRight pads plain text to n display cells. Text is already escaped and
// capped, so its width is its rune count except for wide glyphs, which
// lipgloss measures.
func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
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
