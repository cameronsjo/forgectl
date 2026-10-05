//go:build unix

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// ErrDeskNeedsTerminal is RunDesk's refusal without a terminal on stdin and
// stdout (ADR-0008 rule 1): the dashboard asks for keypresses, so it never
// opens off a TTY.
var ErrDeskNeedsTerminal = errors.New("desk: the dashboard needs a terminal on stdin and stdout; use --frame for one frame, or desk status")

// DeskOptions configures RunDesk.
type DeskOptions struct {
	// Version is forgectl's version, for the header.
	Version string
	// Host is the machine name for the header; "" means os.Hostname().
	Host string
	// Home turns the desk directory's home prefix into "~"; "" shows the
	// directory as it is.
	Home  string
	Theme theme.Theme
	// Notify shows a desktop notification when an item arrives. nil means
	// herdr's, when the desk runs inside a herdr pane, and none otherwise.
	// title and body are already inert, capped lines.
	Notify func(ctx context.Context, title, body string) error
	// Poll is how often the desk re-reads the queue; 0 means deskPollEvery.
	Poll time.Duration
}

const (
	// deskPollEvery is the queue re-read period. The scan is a directory
	// listing plus small reads, so a second is cheap and feels live.
	deskPollEvery = time.Second
	// deskRingEvery re-rings the bell while anything waits.
	deskRingEvery = 5 * time.Minute
	// deskLogTail is how much of a log the l pager reads.
	deskLogTail = 256 << 10
	// deskNotifyTimeout bounds one notification call.
	deskNotifyTimeout = 5 * time.Second
	// deskScanTimeout bounds one queue scan.
	deskScanTimeout = 10 * time.Second
	// deskMessageMax caps a result line, which can quote an error.
	deskMessageMax = 400
	// deskNotifyMaxRunes caps a notification body: herdr's own cap
	// (herdr.NotificationMaxRunes), applied here too so the text is bounded
	// before it leaves the desk.
	deskNotifyMaxRunes = 200
)

// deskBackend is the part of *desk.Desk the model uses; tests replace Launch
// so no supervisor process is started.
type deskBackend interface {
	Path() string
	Scan() (*desk.Snapshot, error)
	Claim(name, wantSHA string) (*desk.Claimed, error)
	Launch(name string) (int, error)
	BeginRun(name string, pid int, fields ...string) (*desk.Run, error)
	Skip(name, reason string) error
	Unskip(name string) error
	Release(name, reason string) error
	CreateLog(name string) (*os.File, error)
	BatchStatus(name string) ([]desk.StepStatus, error)
	Record(name string) ([]byte, desk.Kind, error)
	LogTail(name string, maxBytes int64) ([]byte, bool, error)
}

// RunDesk drives the desk dashboard until the operator quits. It refuses
// with ErrDeskNeedsTerminal unless stdin and stdout are terminals.
func RunDesk(ctx context.Context, d *desk.Desk, opts DeskOptions) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return ErrDeskNeedsTerminal
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(newDeskModel(ctx, d, opts), tea.WithContext(ctx)).Run()
	return err
}

// target is an item as it was on screen: its name and the hash shown. lost
// marks a lost run: skipping it is final, so u never re-arms it.
type target struct {
	name, sha string
	lost      bool
	tty       bool // runs in this pane (y only; a never includes one)
}

// confirmKind is what a pending y/n prompt will do.
type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmSkip
	confirmAll
)

// deskPager is the v / l viewer: inert lines and a scroll offset.
type deskPager struct {
	title  string
	lines  []string
	offset int
}

type deskModel struct {
	ctx     context.Context
	d       deskBackend
	opts    DeskOptions
	theme   theme.Theme
	frame   DeskFrameOptions
	now     func() time.Time
	started time.Time
	poll    time.Duration
	// scanTimeout bounds one scan (deskScanTimeout; tests shorten it).
	scanTimeout time.Duration

	width, height int

	snap     *desk.Snapshot
	rows     []queueRow
	cursor   int
	scanning bool

	message string // the last result, already inert and styled

	confirm  confirmKind
	targets  []target // what a confirmed skip or run-all acts on
	lastSkip string   // what u returns to pending/
	busy     bool     // an action is in flight

	pager *deskPager

	// seen is the waiting set at the last scan, for arrival bells.
	seen     map[string]bool
	scanned  bool
	lastRing time.Time

	notify func(ctx context.Context, title, body string) error
	// ttyArgv builds the foreground command for a TTY item; tests replace it.
	ttyArgv func(rcPath string) []string
}

func newDeskModel(ctx context.Context, d deskBackend, opts DeskOptions) deskModel {
	host := opts.Host
	if host == "" {
		// An unknown name leaves the header without one; it is display only.
		if h, err := os.Hostname(); err == nil {
			host = h
		}
	}
	notify := opts.Notify
	if notify == nil {
		notify = herdrNotify
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = deskPollEvery
	}
	th := opts.Theme
	m := deskModel{
		ctx:         ctx,
		d:           d,
		opts:        opts,
		theme:       th,
		now:         time.Now,
		poll:        poll,
		scanTimeout: deskScanTimeout,
		notify:      notify,
		ttyArgv:     ttyArgv,
		seen:        map[string]bool{},
	}
	m.started = m.now()
	m.frame = DeskFrameOptions{
		Host:    host,
		Version: opts.Version,
		Dir:     TildePath(d.Path(), opts.Home),
		Started: m.started,
	}
	return m
}

// Messages.
type (
	deskTickMsg struct{}
	deskScanMsg struct {
		snap    *desk.Snapshot
		steps   map[string][]desk.StepStatus
		records map[string][]byte
		err     error
	}
	// deskResultMsg is an action's outcome; it always triggers a rescan.
	deskResultMsg struct {
		text      string
		err       error
		skipped   string // set when a skip succeeded, for u
		unskipped bool
	}
	// deskTTYReadyMsg is a claimed TTY item with its run begun, ready to hand
	// the terminal to.
	deskTTYReadyMsg struct {
		run  *ttyRun
		err  error
		name string
	}
	deskTTYDoneMsg struct {
		run *ttyRun
		err error
	}
	deskPagerMsg struct {
		pager *deskPager
		err   error
	}
)

func (m deskModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.scanCmd(), m.tick()}
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

func (m deskModel) tick() tea.Cmd {
	return tea.Tick(m.poll, func(time.Time) tea.Msg { return deskTickMsg{} })
}

// scanCmd reads the queue, plus the step states and records of running items.
// A scan that has not answered within deskScanTimeout is reported as an
// error so the next tick scans again: a hung read must not freeze the queue
// behind a clock that still ticks. (The readers refuse FIFOs, so a hang is
// not expected; this is the backstop.)
func (m deskModel) scanCmd() tea.Cmd {
	scan, limit := m.scanOnce(), m.scanTimeout
	return func() tea.Msg {
		got := make(chan tea.Msg, 1)
		go func() { got <- scan() }()
		select {
		case msg := <-got:
			return msg
		case <-time.After(limit):
			return deskScanMsg{err: fmt.Errorf("desk: the scan did not finish in %s; retrying", limit)}
		}
	}
}

func (m deskModel) scanOnce() func() tea.Msg {
	d := m.d
	return func() tea.Msg {
		snap, err := d.Scan()
		if err != nil {
			return deskScanMsg{err: err}
		}
		steps := map[string][]desk.StepStatus{}
		records := map[string][]byte{}
		for _, it := range snap.Running {
			if data, _, err := d.Record(it.Name); err == nil {
				records[it.Name] = data
			}
			if it.Kind == desk.KindBatch {
				if s, err := d.BatchStatus(it.Name); err == nil {
					steps[it.Name] = s
				}
			}
		}
		return deskScanMsg{snap: snap, steps: steps, records: records}
	}
}

func (m deskModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		return m, nil
	case tea.BackgroundColorMsg:
		if m.theme.Mode() == theme.ModeAuto {
			m.theme = m.theme.WithDark(t.IsDark())
		}
		return m, nil
	case deskTickMsg:
		cmds := []tea.Cmd{m.tick()}
		if !m.scanning {
			m.scanning = true
			cmds = append(cmds, m.scanCmd())
		}
		return m, tea.Batch(cmds...)
	case deskScanMsg:
		m.scanning = false
		if t.err != nil {
			m.message = m.styles().Danger.Render(safeMessage("scan: " + t.err.Error()))
			return m, nil
		}
		return m.applyScan(t)
	case deskResultMsg:
		m.busy = false
		if t.skipped != "" {
			m.lastSkip = t.skipped
		}
		if t.unskipped {
			// An undone skip is back in pending/ but is not an arrival.
			if t.err == nil {
				m.seen[m.lastSkip] = true
			}
			m.lastSkip = ""
		}
		m.message = m.resultLine(t.text, t.err)
		return m, m.rescan()
	case deskTTYReadyMsg:
		if t.err != nil {
			m.busy = false
			m.message = m.resultLine("", t.err)
			return m, m.rescan()
		}
		run := t.run
		return m, tea.Exec(run, func(err error) tea.Msg { return deskTTYDoneMsg{run: run, err: err} })
	case deskTTYDoneMsg:
		m.busy = false
		rc, err := t.run.finish(t.err)
		text := fmt.Sprintf("%s ended: exit %d", itemLabel(t.run.claimed.Name), rc)
		if rc == 0 {
			text = itemLabel(t.run.claimed.Name) + " ended: ok"
		}
		m.message = m.resultLine(text, err)
		return m, m.rescan()
	case deskPagerMsg:
		if t.err != nil {
			m.message = m.resultLine("", t.err)
			return m, nil
		}
		m.pager = t.pager
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(t)
	}
	return m, nil
}

// rescan reads the queue now, unless a scan is already on its way.
func (m *deskModel) rescan() tea.Cmd {
	if m.scanning {
		return nil
	}
	m.scanning = true
	return m.scanCmd()
}

// applyScan takes a new snapshot: keeps the cursor on the same item when it
// is still listed, and rings for arrivals.
func (m deskModel) applyScan(t deskScanMsg) (tea.Model, tea.Cmd) {
	selected := ""
	if m.cursor < len(m.rows) {
		selected = m.rows[m.cursor].item.Name
	}
	now := m.now()
	m.snap, m.frame.Steps, m.frame.Records = t.snap, t.steps, t.records
	m.rows = deskRows(t.snap, now)
	m.cursor = min(m.cursor, max(len(m.rows)-1, 0))
	for i, r := range m.rows {
		if r.item.Name == selected {
			m.cursor = i
			break
		}
	}

	waiting := map[string]bool{}
	var arrived []desk.Item
	for _, it := range t.snap.Pending {
		if it.State != desk.StateWaiting {
			continue
		}
		waiting[it.Name] = true
		if m.scanned && !m.seen[it.Name] {
			arrived = append(arrived, it)
		}
	}
	first := !m.scanned
	m.seen, m.scanned = waiting, true
	switch {
	case first:
		m.lastRing = now
	case len(arrived) > 0:
		m.lastRing = now
		return m, m.ring(len(waiting), arrived[0])
	case len(waiting) > 0 && now.Sub(m.lastRing) >= deskRingEvery:
		m.lastRing = now
		return m, m.ring(len(waiting), desk.Item{})
	}
	return m, nil
}

// ring is the terminal bell plus a notification. first, when set, is the
// item that just arrived.
func (m deskModel) ring(waiting int, first desk.Item) tea.Cmd {
	title, body := deskNotification(waiting, first)
	notify, ctx := m.notify, m.ctx
	return tea.Batch(tea.Raw("\a"), func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, deskNotifyTimeout)
		defer cancel()
		_ = notify(ctx, title, body) // best effort: the bell has already rung
		return nil
	})
}

// deskNotification builds the notification text: inert, capped lines.
func deskNotification(waiting int, first desk.Item) (title, body string) {
	title = fmt.Sprintf("desk: %d waiting", waiting)
	if first.Name == "" {
		return title, "still waiting for you"
	}
	body = itemLabel(first.Name)
	if first.What != "" {
		body += ": " + first.What
	}
	return title, termsafe.SafeLineMax(body, deskNotifyMaxRunes)
}

// herdrNotify shows a herdr notification when the desk runs in a herdr pane,
// and does nothing anywhere else.
func herdrNotify(ctx context.Context, title, body string) error {
	if herdr.CheckSession(os.LookupEnv) != nil {
		return nil
	}
	path, err := osexec.LookPath(herdr.Binary)
	if err != nil {
		return nil //nolint:nilerr // no herdr binary on PATH: nothing to notify through
	}
	return herdr.NotificationShow(ctx, exec.NewOSSensitiveRunner(), path, herdr.Notification{
		Title: title, Body: body, Sound: herdr.SoundRequest,
	})
}

func (m deskModel) styles() theme.Styles { return m.theme.Styles() }

// resultLine renders an action's outcome for the footer.
func (m deskModel) resultLine(text string, err error) string {
	st := m.styles()
	if err != nil {
		return st.Danger.Render(safeMessage(err.Error()))
	}
	return st.OK.Render(safeMessage(text))
}

func safeMessage(s string) string { return termsafe.SafeLineMax(s, deskMessageMax) }

func (m deskModel) selected() (queueRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return queueRow{}, false
	}
	return m.rows[m.cursor], true
}

func (m deskModel) updateKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.pager != nil {
		m.pagerKey(key)
		return m, nil
	}
	if m.confirm != confirmNone {
		return m.confirmKey(key)
	}
	// A result stays until the next key, then the hints come back.
	m.message = ""
	switch key {
	case "q":
		return m, tea.Quit
	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "y":
		return m.run()
	case "s":
		return m.askSkip()
	case "u":
		return m.undo()
	case "a":
		return m.askAll()
	case "v":
		return m, m.view()
	case "l":
		return m, m.log()
	}
	return m, nil
}

// run starts the selected item: a TTY item in this pane, anything else under
// a detached supervisor.
func (m deskModel) run() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok || m.busy {
		return m, nil
	}
	st := m.styles()
	if !r.runnable() {
		m.message = st.Warn.Render(safeMessage(itemLabel(r.item.Name) + " is " + rowLabel(r.kind) + "; only a waiting item runs"))
		return m, nil
	}
	// One key, as the old desk had: the focus panel's short hash is what the
	// operator matches to the agent's report. a, which runs many, confirms.
	m.busy = true
	return m.start(target{name: r.item.Name, sha: r.item.Meta.SHA256, tty: r.item.TTY})
}

// start runs one confirmed item at the hash that was on screen.
func (m deskModel) start(t target) (tea.Model, tea.Cmd) {
	if t.tty {
		m.message = ""
		return m, m.startTTY(t)
	}
	d := m.d
	return m, func() tea.Msg {
		text, err := launch(d, t)
		return deskResultMsg{text: text, err: err}
	}
}

func rowLabel(k rowKind) string {
	_, label, _ := rowLook(theme.Default().Styles(), k)
	return label
}

// launch claims one item at the hash shown and starts its supervisor.
func launch(d deskBackend, t target) (string, error) {
	if _, err := d.Claim(t.name, t.sha); err != nil {
		return "", err
	}
	if _, err := d.Launch(t.name); err != nil {
		return "", err
	}
	return "started " + itemLabel(t.name), nil
}

func (m deskModel) askSkip() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok || m.busy {
		return m, nil
	}
	if r.kind != rowWaiting && r.kind != rowRefused && r.kind != rowLost {
		m.message = m.styles().Warn.Render(safeMessage(itemLabel(r.item.Name) + " is " + rowLabel(r.kind) + "; there is nothing to skip"))
		return m, nil
	}
	m.confirm, m.targets = confirmSkip, []target{{name: r.item.Name, sha: r.item.Meta.SHA256, lost: r.kind == rowLost}}
	return m, nil
}

// askAll captures every runnable item on screen now, at the hash shown. TTY
// items need the pane, so they are left for y; refused items cannot run.
// What arrives after this moment is not in the set.
func (m deskModel) askAll() (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	var ts []target
	for _, r := range m.rows {
		if r.runnable() && !r.item.TTY {
			ts = append(ts, target{name: r.item.Name, sha: r.item.Meta.SHA256})
		}
	}
	if len(ts) == 0 {
		m.message = m.styles().Warn.Render("nothing to run: a TTY item runs with y, one at a time")
		return m, nil
	}
	m.confirm, m.targets = confirmAll, ts
	return m, nil
}

func (m deskModel) confirmKey(key string) (tea.Model, tea.Cmd) {
	kind, ts := m.confirm, m.targets
	m.confirm, m.targets = confirmNone, nil
	if key != "y" {
		m.message = m.styles().Muted.Render("cancelled")
		return m, nil
	}
	m.busy = true
	d := m.d
	switch kind {
	case confirmSkip:
		name, lost := ts[0].name, ts[0].lost
		return m, func() tea.Msg {
			if lost {
				// A run that began is skipped as lost (Skip records SkipLost
				// for anything leaving running/) and is never offered to u.
				if err := d.Skip(name, desk.SkipLost); err != nil {
					return deskResultMsg{err: err}
				}
				return deskResultMsg{text: "skipped lost run " + itemLabel(name) + "; queue a new item to run it again"}
			}
			if err := d.Skip(name, desk.SkipOperator); err != nil {
				return deskResultMsg{err: err}
			}
			return deskResultMsg{text: "skipped " + itemLabel(name) + " · u to undo", skipped: name}
		}
	default:
		return m, func() tea.Msg { return runAll(d, ts) }
	}
}

// runAll starts exactly ts, each at the hash it had on screen. An item that
// changed since is not run (Claim moves it to skipped/), and one another
// desk took, or that a rescan already moved to skipped/, counts as no
// longer waiting.
func runAll(d deskBackend, ts []target) deskResultMsg {
	var started, changed, gone int
	var errs []error
	for _, t := range ts {
		_, err := launch(d, t)
		switch {
		case err == nil:
			started++
		case errors.Is(err, desk.ErrChanged):
			changed++
		case errors.Is(err, desk.ErrClaimed):
			gone++
		default:
			errs = append(errs, err)
		}
	}
	text := fmt.Sprintf("started %d", started)
	if changed > 0 {
		text += fmt.Sprintf(" · %d changed, not run", changed)
	}
	if gone > 0 {
		text += fmt.Sprintf(" · %d no longer waiting", gone)
	}
	return deskResultMsg{text: text, err: errors.Join(errs...)}
}

func (m deskModel) undo() (tea.Model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	name := m.lastSkip
	if name == "" {
		m.message = m.styles().Muted.Render("nothing to undo")
		return m, nil
	}
	m.busy = true
	d := m.d
	return m, func() tea.Msg {
		if err := d.Unskip(name); err != nil {
			return deskResultMsg{err: err, unskipped: true}
		}
		return deskResultMsg{text: "returned " + itemLabel(name) + " to the queue", unskipped: true}
	}
}

// view opens the selected item's script (or manifest) in the pager. A
// waiting item shows the bytes that were hashed, which are the bytes y runs.
func (m deskModel) view() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	it, d := r.item, m.d
	return func() tea.Msg {
		data := it.Content
		if data == nil {
			rec, _, err := d.Record(it.Name)
			if err != nil {
				return deskPagerMsg{err: err}
			}
			data = rec
		}
		return deskPagerMsg{pager: &deskPager{title: "script · " + itemLabel(it.Name), lines: pagerLines(string(data), false)}}
	}
}

// log opens the selected item's log, or the latest run's when the selected
// item has not run.
func (m deskModel) log() tea.Cmd {
	name := ""
	if r, ok := m.selected(); ok && r.kind != rowWaiting && r.kind != rowRefused && r.kind != rowChanged {
		name = r.item.Name
	}
	if name == "" && m.snap != nil && len(m.snap.Done) > 0 {
		name = m.snap.Done[0].Name
	}
	if name == "" {
		return func() tea.Msg { return deskPagerMsg{err: errors.New("no log yet: nothing has run")} }
	}
	d := m.d
	return func() tea.Msg {
		data, cut, err := d.LogTail(name, deskLogTail)
		if err != nil {
			return deskPagerMsg{err: err}
		}
		lines := pagerLines(string(data), true)
		if cut {
			lines = append([]string{"… earlier output not shown"}, lines...)
		}
		return deskPagerMsg{pager: &deskPager{title: "log · " + itemLabel(name), lines: lines}}
	}
}

// pagerLines splits text into inert lines. A log is terminal output: its
// SGR and cursor sequences are dropped, and a line a carriage return
// rewrote shows only its last version, as the terminal showed it.
//
// Every line is escaped whole, with no rune cap: v promises the bytes y runs,
// and a cut would hide part of them. Lines are soft-wrapped to the window
// when drawn (pagerRows), never cut. Input is already bounded (a record is
// at most 1 MiB, a log tail 256 KiB).
func pagerLines(text string, isLog bool) []string {
	raw := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	out := make([]string, len(raw))
	for i, l := range raw {
		if isLog {
			l = ansi.Strip(l)
			if j := strings.LastIndex(strings.TrimRight(l, "\r"), "\r"); j >= 0 {
				l = l[j+1:]
			}
			l = strings.TrimRight(l, "\r")
		}
		out[i] = termsafe.SafeLine(strings.ReplaceAll(l, "\t", "    "))
	}
	return out
}

// pagerWrapMark starts a soft-wrapped continuation row.
const pagerWrapMark = "↳ "

// pagerRows soft-wraps the pager's lines to width. A line that fits is one
// row; a longer one continues on rows that start with pagerWrapMark. The
// lines are already escaped, and an escape is plain text, so wrapping can
// split one across rows but never makes it live.
func pagerRows(lines []string, width int) []string {
	width = max(width, 8)
	var rows []string
	for _, l := range lines {
		if ansi.StringWidth(l) <= width {
			rows = append(rows, l)
			continue
		}
		first := ansi.Truncate(l, width, "")
		rows = append(rows, first)
		rest := strings.Split(ansi.Hardwrap(ansi.TruncateLeft(l, ansi.StringWidth(first), ""), width-ansi.StringWidth(pagerWrapMark), true), "\n")
		for _, r := range rest {
			rows = append(rows, pagerWrapMark+r)
		}
	}
	return rows
}

func (m *deskModel) pagerKey(key string) {
	p := m.pager
	page := max(m.height-2, 1)
	last := max(len(pagerRows(p.lines, m.width))-page, 0)
	switch key {
	case "q", "esc", "v", "l":
		m.pager = nil
		return
	case "j", "down":
		p.offset++
	case "k", "up":
		p.offset--
	case "space", "f", "pgdown":
		p.offset += page
	case "b", "pgup":
		p.offset -= page
	case "g", "home":
		p.offset = 0
	case "G", "end":
		p.offset = last
	}
	p.offset = min(max(p.offset, 0), last)
}

func (m deskModel) View() tea.View {
	width, height := m.width, m.height
	if width <= 0 {
		width, height = 80, 24
	}
	var content string
	if m.pager != nil {
		content = m.pagerView(width, height)
	} else {
		th := m.theme
		opts := m.frame
		opts.Theme = &th
		content = deskFrame{
			snap: m.snap, width: width, height: height, now: m.now(), opts: opts,
			cursor: m.cursor, footer: m.footer(),
		}.render()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	n := 0
	for _, r := range m.rows {
		if r.kind == rowWaiting {
			n++
		}
	}
	v.WindowTitle = fmt.Sprintf("desk ● %d waiting", n)
	return v
}

// frameWidth is the width the frame is drawn at.
func (m deskModel) frameWidth() int {
	if m.width <= 0 {
		return 80
	}
	return max(m.width, 40)
}

// hashLines renders a full sha256 for the a prompt: "sha256 <64 hex>"
// on one line when it fits, else wrapped in two halves, so the whole hash is
// always on screen. A hash that is not 64 hex characters is never drawn.
func hashLines(st theme.Styles, sha string, width int) []string {
	if !desk.ValidSHA256(sha) {
		return []string{"    " + st.Danger.Render("no valid hash recorded")}
	}
	const label = "    sha256 "
	if len(label)+len(sha) <= width {
		return []string{st.Muted.Render(label) + st.Fg.Render(sha)}
	}
	pad := strings.Repeat(" ", len(label))
	return []string{st.Muted.Render(label) + st.Fg.Render(sha[:32]), pad + st.Fg.Render(sha[32:])}
}

// footer is the prompt, or the last result; "" leaves the key hints.
func (m deskModel) footer() string {
	st := m.styles()
	switch m.confirm {
	case confirmSkip:
		return " " + st.Warn.Render("skip "+itemLabel(m.targets[0].name)+"?") + st.Muted.Render("  y skip · any other key cancels")
	case confirmAll:
		lines := []string{" " + st.Warn.Render(fmt.Sprintf("run these %d?", len(m.targets))) + st.Muted.Render("  y run · any other key cancels")}
		for _, t := range m.targets {
			lines = append(lines, "  "+st.Fg.Render(itemLabel(t.name)))
			lines = append(lines, hashLines(st, t.sha, m.frameWidth())...)
		}
		return strings.Join(lines, "\n")
	}
	if m.message != "" {
		return " " + m.message
	}
	return ""
}

func (m deskModel) pagerView(width, height int) string {
	st := m.styles()
	p := m.pager
	rows := max(height-2, 1)
	all := pagerRows(p.lines, width)
	pos := fmt.Sprintf("%d-%d/%d", min(p.offset+1, len(all)), min(p.offset+rows, len(all)), len(all))
	lines := []string{cut(st.Header.Render(p.title)+st.Muted.Render("  "+pos), width)}
	for i := p.offset; i < len(all) && i < p.offset+rows; i++ {
		lines = append(lines, all[i])
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, cut(st.Muted.Render(" j/k scroll · space/b page · g/G top/bottom · q close"), width))
	return strings.Join(lines, "\n")
}

// startTTY claims a TTY item at the hash shown and begins its run with this
// process as the owner, then hands it to tea.Exec.
func (m deskModel) startTTY(t target) tea.Cmd {
	d, argv := m.d, m.ttyArgv
	return func() tea.Msg {
		// The rc file comes first: once Claim succeeds, every later failure
		// leaves the item in running/ with no owner.
		rcFile, err := os.CreateTemp("", "forgectl-desk-rc-*")
		if err != nil {
			return deskTTYReadyMsg{err: fmt.Errorf("desk: rc file for %s: %w", itemLabel(t.name), err), name: t.name}
		}
		rcPath := rcFile.Name()
		if err := rcFile.Close(); err != nil {
			_ = os.Remove(rcPath)
			return deskTTYReadyMsg{err: err, name: t.name}
		}
		c, err := d.Claim(t.name, t.sha)
		if err != nil {
			_ = os.Remove(rcPath)
			return deskTTYReadyMsg{err: err, name: t.name}
		}
		run, err := newTTYRun(d, c, rcPath, argv)
		return deskTTYReadyMsg{run: run, err: err, name: t.name}
	}
}

// ttyRun is a TTY item run in the desk's own terminal. It is a tea.ExecCommand:
// tea suspends the dashboard, sets stdin and stdout to the terminal, and calls
// Run.
//
// The command is script(1) recording to done/<name>.log, around bash reading
// the verified bytes from fd 3 (see desk.AttachScript): the running/ record is
// never executed. The rc comes from a file the wrapper writes, since script(1)
// does not reliably pass its child's status back. Nothing here starts a new
// process group, so the script keeps the terminal's foreground and its
// prompts work.
type ttyRun struct {
	claimed *desk.Claimed
	run     *desk.Run
	cmd     *osexec.Cmd
	rcPath  string
	// log is done/<name>.log, created exclusively through the pinned root
	// and handed to script(1) as fd 4 (ttyLogFD): script never resolves a
	// path into the desk, so a symlink or a renamed desk cannot redirect it.
	log *os.File
}

// ttyLogFD is the log operand script(1) gets: the log the desk created, on
// fd 4, after the script on fd 3.
const ttyLogFD = "/dev/fd/4"

func newTTYRun(d deskBackend, c *desk.Claimed, rcPath string, argv func(rcPath string) []string) (*ttyRun, error) {
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		// The claim has no owner: end it in skipped/ rather than leave it
		// in running/ until the claim grace calls it lost.
		_ = os.Remove(rcPath)
		return nil, errors.Join(err, d.Release(c.Name, desk.SkipLaunchFailed))
	}
	logF, err := d.CreateLog(c.Name)
	if errors.Is(err, fs.ErrExist) {
		// Another run's log took the name after BeginRun's check: end this
		// run in skipped/ (name-reused) and leave that record alone.
		_ = os.Remove(rcPath)
		return nil, errors.Join(err, run.Abandon(desk.SkipReused))
	}
	if err != nil {
		// The run has begun (RUN-START is written): end it, as the
		// supervisor does when its log cannot be created.
		_ = os.Remove(rcPath)
		return nil, errors.Join(err, run.Finish(2, "internal"))
	}
	a := argv(rcPath)
	cmd := osexec.CommandContext(context.Background(), a[0], a[1:]...) //nolint:gosec // G204: script(1) and bash at fixed paths; the item arrives on fd 3
	return &ttyRun{claimed: c, run: run, cmd: cmd, rcPath: rcPath, log: logF}, nil
}

// Run starts the command with the verified bytes on fd 3 and waits for it.
func (r *ttyRun) Run() error {
	defer r.closeLog()
	release, err := r.claimed.AttachScript(r.cmd)
	if err != nil {
		return err
	}
	r.cmd.ExtraFiles = append(r.cmd.ExtraFiles, r.log) // fd 4, after the script on fd 3
	err = r.cmd.Start()
	release()
	r.closeLog() // the child holds its own copy
	if err != nil {
		return err
	}
	return r.cmd.Wait()
}

func (r *ttyRun) closeLog() {
	if r.log != nil {
		_ = r.log.Close()
		r.log = nil
	}
}

func (r *ttyRun) SetStdin(in io.Reader) {
	if r.cmd.Stdin == nil {
		r.cmd.Stdin = in
	}
}

func (r *ttyRun) SetStdout(out io.Writer) {
	if r.cmd.Stdout == nil {
		r.cmd.Stdout = out
	}
}

func (r *ttyRun) SetStderr(out io.Writer) {
	if r.cmd.Stderr == nil {
		r.cmd.Stderr = out
	}
}

// finish reads the rc the wrapper wrote and records the end of the run.
// runErr is what Run returned: a command that never started is rc 127.
func (r *ttyRun) finish(runErr error) (int, error) {
	r.closeLog()              // Run may never have been called
	defer os.Remove(r.rcPath) //nolint:errcheck // a leftover temp file is harmless
	rc := 2
	var startErr *osexec.Error
	switch data, err := os.ReadFile(r.rcPath); {
	case errors.As(runErr, &startErr) || (runErr != nil && r.cmd.Process == nil):
		rc = 127
	case err == nil && strings.TrimSpace(string(data)) != "":
		if v, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
			rc = v
		}
	}
	reason := desk.StepOK
	if rc != 0 {
		reason = desk.StepFailed
	}
	if err := r.run.Finish(rc, reason); err != nil {
		return rc, err
	}
	if rc == 127 && runErr != nil {
		return rc, fmt.Errorf("desk: could not start %s: %w", itemLabel(r.claimed.Name), runErr)
	}
	return rc, nil
}

// ttyWrapper runs the script and writes its status to the rc file:
// $1 is bash, $2 the script operand (desk.ScriptFDPath), $3 the rc file. It
// first closes fd 4, the log script(1) was handed: script holds its own
// descriptor on the log, and the item and anything it starts must not hold
// a writable one on the done/ record. (fd 3 is closed by the desk's prelude
// inside the script itself.)
const ttyWrapper = `exec 4>&-; "$1" "$2"; echo $? > "$3"`
