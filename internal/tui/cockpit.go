package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// The status cockpit (forgectl#13 lane 2): `forgectl status --tui`, a
// read-only view of the four status sections that refreshes in place. It
// knows nothing of where a section's data comes from: internal/cli turns each
// status section into a plain CockpitSection, so this package never imports
// the pr, bench or clean packages (TestCockpitImportGraph pins that).

// CockpitState is a section's collection outcome as the cockpit draws it.
type CockpitState int

const (
	// CockpitLoading is a section whose first result has not landed yet.
	CockpitLoading CockpitState = iota
	// CockpitOK is a source that answered in full.
	CockpitOK
	// CockpitDegraded is a source that answered with notes.
	CockpitDegraded
	// CockpitFailed is a source that produced no data; Error says why.
	CockpitFailed
)

// CockpitRowKind says what enter does on a row.
type CockpitRowKind int

const (
	// CockpitRowInfo is a row with nothing to open (a bench component).
	CockpitRowInfo CockpitRowKind = iota
	// CockpitRowProject is a local project; Path is its directory. Enter
	// will focus its terminal once the focus seam lands; today it says so.
	CockpitRowProject
	// CockpitRowPR is a pull request; Ref is what `forgectl pr` takes.
	CockpitRowPR
)

// CockpitRow is one list row. Every field is untrusted text (a project
// directory name, a PR title from the server) and is escaped and capped
// when drawn.
type CockpitRow struct {
	Kind   CockpitRowKind
	Label  string
	Detail string
	Path   string
	Ref    string
}

// CockpitSection is one status section's latest result.
type CockpitSection struct {
	Name     string
	State    CockpitState
	Error    string
	Notes    []string
	Headline string
	Rows     []CockpitRow
}

// CockpitSnapshot is the whole report as the cockpit sees it.
type CockpitSnapshot struct {
	Sections []CockpitSection
}

// CockpitSource is one section's loader. Load collects the section under
// its own deadline and returns it, plus a channel closed once the source's
// goroutine has really returned (nil when nothing outlives Load). The
// section stays busy, and refuses another refresh, until that channel
// closes, so a source that ignores its deadline cannot pile up goroutines.
//
// Auto marks the one kind of section the cockpit refreshes on its own timer,
// every cockpitAutoEvery: a local read (git). A section that reaches the
// network (prs) or walks the disk at length (clean, bench) refreshes only
// when asked.
type CockpitSource struct {
	Name string
	Auto bool
	Load func(ctx context.Context) (CockpitSection, <-chan struct{})
}

// CockpitOptions configures RunCockpit. BuildArgv turns a PR row's ref into
// the `pr <ref>` argv the hub's picker would run; nil falls back to the
// tree-blind PickerArgv.
type CockpitOptions struct {
	Sources   []CockpitSource
	BuildArgv ArgvBuilder
	NoIcons   bool
	Theme     theme.Theme
}

// statusCockpitArgv is what the hub's status row runs.
func statusCockpitArgv() []string { return []string{"status", "--tui"} }

const (
	// cockpitAutoEvery is the automatic refresh period for Auto sections.
	cockpitAutoEvery = 60 * time.Second
	// cockpitMinGap is the least time between two refreshes of one section,
	// manual or automatic.
	cockpitMinGap = 15 * time.Second
	// cockpitCollapsedRows is how many rows a section shows while another
	// section has the focus.
	cockpitCollapsedRows = 2

	// Display caps, in rendered runes, for every value a section carries.
	cockpitNameMax     = 16
	cockpitHeadlineMax = 160
	cockpitErrorMax    = 200
	cockpitNoteMax     = 200
	cockpitLabelMax    = 48
	cockpitDetailMax   = 96
	cockpitFilterMax   = 64
)

// cockpitResultMsg carries one section's result back from its Load.
type cockpitResultMsg struct {
	idx  int
	sec  CockpitSection
	done <-chan struct{}
}

// cockpitDoneMsg reports that a section's source goroutine has returned.
type cockpitDoneMsg struct{ idx int }

// cockpitTickMsg is the automatic refresh timer firing.
type cockpitTickMsg struct{}

// cockpitSection is a section's live state in the model.
type cockpitSection struct {
	src     CockpitSource
	snap    CockpitSection
	busy    bool
	started time.Time
	updated time.Time
	cursor  int
	offset  int
}

type cockpitModel struct {
	ctx    context.Context
	theme  theme.Theme
	styles theme.Styles
	glyph  glyphSet

	width, height int

	secs  []cockpitSection
	focus int

	filter    []rune
	filtering bool
	help      bool

	// footer is the last result line, already styled.
	footer string

	buildArgv ArgvBuilder
	now       func() time.Time
	// schedule arms the next automatic refresh; tests replace it so a model
	// driven by hand never sleeps.
	schedule func() tea.Cmd

	action Action
}

// RunCockpit drives the cockpit until the operator quits, and returns the
// deferred Action (a `pr <ref>` run), if any, for the caller to perform
// after the terminal is released.
func RunCockpit(ctx context.Context, opts CockpitOptions) (Action, error) {
	// Cancelled on return, so a refresh still in flight when the operator
	// quits (a gh search, a disk walk) is told to stop rather than running on
	// beside the command the cockpit hands back.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := tea.NewProgram(newCockpitModel(ctx, opts), tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return Action{}, err
	}
	if fm, ok := final.(cockpitModel); ok {
		return fm.action, nil
	}
	return Action{}, nil
}

func newCockpitModel(ctx context.Context, opts CockpitOptions) cockpitModel {
	build := opts.BuildArgv
	if build == nil {
		build = PickerArgv
	}
	m := cockpitModel{
		ctx:       ctx,
		theme:     opts.Theme,
		styles:    opts.Theme.Styles(),
		glyph:     pickGlyphs(opts.NoIcons),
		buildArgv: build,
		now:       time.Now,
		schedule: func() tea.Cmd {
			return tea.Tick(cockpitAutoEvery, func(time.Time) tea.Msg { return cockpitTickMsg{} })
		},
	}
	for _, src := range opts.Sources {
		m.secs = append(m.secs, cockpitSection{src: src, snap: CockpitSection{Name: src.Name}})
	}
	return m
}

// Init starts the one full collection the cockpit opens with, arms the
// automatic timer, and probes the background colour where that is safe.
func (m cockpitModel) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.secs)+2)
	for i := range m.secs {
		cmd, _ := m.refresh(i)
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, m.schedule())
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

// refresh starts section i's source, or says why it will not: one refresh
// in flight per section, and at least cockpitMinGap between two starts.
// Note the model is mutated through m.secs, which every copy shares.
func (m cockpitModel) refresh(i int) (tea.Cmd, string) {
	s := &m.secs[i]
	name := capSafe(s.src.Name, cockpitNameMax)
	if s.busy {
		return nil, name + ": a refresh is already running"
	}
	now := m.now()
	if !s.started.IsZero() {
		if wait := cockpitMinGap - now.Sub(s.started); wait > 0 {
			return nil, fmt.Sprintf("%s: refreshed moments ago; try again in %ds", name, int((wait+time.Second-1)/time.Second))
		}
	}
	s.busy = true
	s.started = now
	load, ctx := s.src.Load, m.ctx
	return func() tea.Msg {
		sec, done := load(ctx)
		return cockpitResultMsg{idx: i, sec: sec, done: done}
	}, ""
}

// Update handles msg, then scrolls the focused section so its cursor is in
// view. Scrolling lives here rather than in View so View only reads the
// model.
func (m cockpitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	out, cmd := m.update(msg)
	next := out.(cockpitModel)
	next.scrollToCursor()
	return next, cmd
}

// bodyHeight is how many lines the sections (or the help) get: the window
// less the header line and the two footer lines.
func (m cockpitModel) bodyHeight() int {
	return max(m.height-1-2, 3)
}

// focusRoom is how many row lines the focused section gets: whatever the
// other sections and its own title and notes leave of the body.
func (m cockpitModel) focusRoom() int {
	used := 0
	for i := range m.secs {
		if i == m.focus {
			used += 1 + len(m.secs[i].snap.Notes)
			continue
		}
		used += len(m.collapsedView(i))
	}
	return max(m.bodyHeight()-used, 1)
}

// scrollToCursor moves the focused section's window so its cursor shows.
func (m *cockpitModel) scrollToCursor() {
	if m.focus < 0 || m.focus >= len(m.secs) {
		return
	}
	s := &m.secs[m.focus]
	room := m.focusRoom()
	if s.cursor < s.offset {
		s.offset = s.cursor
	}
	if s.cursor >= s.offset+room {
		s.offset = s.cursor - room + 1
	}
	if s.offset < 0 {
		s.offset = 0
	}
}

func (m cockpitModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		return m, nil
	case tea.BackgroundColorMsg:
		if m.theme.Mode() == theme.ModeAuto {
			m.theme = m.theme.WithDark(t.IsDark())
			m.styles = m.theme.Styles()
		}
		return m, nil
	case cockpitResultMsg:
		if t.idx < 0 || t.idx >= len(m.secs) {
			return m, nil
		}
		s := &m.secs[t.idx]
		s.snap = t.sec
		s.updated = m.now()
		m.clampCursor(t.idx)
		if t.done == nil {
			s.busy = false
			return m, nil
		}
		done, idx := t.done, t.idx
		return m, func() tea.Msg {
			<-done
			return cockpitDoneMsg{idx: idx}
		}
	case cockpitDoneMsg:
		if t.idx >= 0 && t.idx < len(m.secs) {
			m.secs[t.idx].busy = false
		}
		return m, nil
	case cockpitTickMsg:
		cmds := []tea.Cmd{}
		for i := range m.secs {
			if !m.secs[i].src.Auto {
				continue
			}
			if cmd, _ := m.refresh(i); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		cmds = append(cmds, m.schedule())
		return m, tea.Batch(cmds...)
	case tea.PasteMsg:
		if m.filtering {
			m.typeFilter(t.Content)
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(t)
	}
	return m, nil
}

func (m cockpitModel) updateKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.help {
		switch key {
		case "?", "esc":
			m.help = false
		case "q":
			return m, tea.Quit
		}
		return m, nil
	}
	if m.filtering {
		switch key {
		case "enter":
			m.filtering = false
		case "esc":
			m.filtering = false
			m.filter = nil
			m.resetCursors()
		case "backspace":
			if len(m.filter) > 0 {
				m.filter = m.filter[:len(m.filter)-1]
			}
			m.resetCursors()
		default:
			if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
				m.typeFilter(k.Text)
			}
		}
		return m, nil
	}

	switch key {
	case "q":
		return m, tea.Quit
	case "esc":
		if len(m.filter) > 0 {
			m.filter = nil
			m.resetCursors()
			return m, nil
		}
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "/":
		m.filtering = true
		return m, nil
	case "tab":
		if len(m.secs) > 0 {
			m.focus = (m.focus + 1) % len(m.secs)
		}
		return m, nil
	case "shift+tab":
		if len(m.secs) > 0 {
			m.focus = (m.focus + len(m.secs) - 1) % len(m.secs)
		}
		return m, nil
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		return m, nil
	case "enter":
		return m.activate()
	case "r":
		if len(m.secs) == 0 {
			return m, nil
		}
		cmd, why := m.refresh(m.focus)
		if cmd == nil {
			m.footer = m.styles.Warn.Render("! " + why)
			return m, nil
		}
		m.footer = m.styles.Muted.Render("refreshing " + capSafe(m.secs[m.focus].src.Name, cockpitNameMax) + "…")
		return m, cmd
	case "R":
		return m.refreshAll()
	}
	// No 1-9 here, unlike the hub: rows carry no visible numbers, and on a
	// PR row a digit would start a review on a row the operator never saw
	// numbered.
	return m, nil
}

// refreshAll asks every section to refresh, each under its own rules, and
// reports in one footer line which ones started and which declined.
func (m cockpitModel) refreshAll() (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var started, declined []string
	for i := range m.secs {
		cmd, why := m.refresh(i)
		if cmd == nil {
			declined = append(declined, why)
			continue
		}
		cmds = append(cmds, cmd)
		started = append(started, capSafe(m.secs[i].src.Name, cockpitNameMax))
	}
	// Every piece is escaped and capped already: the names here, the
	// reasons by refresh.
	var parts []string
	if len(started) > 0 {
		parts = append(parts, "refreshing "+strings.Join(started, ", ")+"…")
	}
	parts = append(parts, declined...)
	line := strings.Join(parts, " · ")
	if len(started) == 0 {
		m.footer = m.styles.Warn.Render("! " + line)
	} else {
		m.footer = m.styles.Muted.Render(line)
	}
	return m, tea.Batch(cmds...)
}

// typeFilter appends typed or pasted text to the filter, dropping control
// and bidi runes: no row label can usefully match one, and the filter line
// should not have to show them.
func (m *cockpitModel) typeFilter(s string) {
	for _, r := range s {
		if len(m.filter) >= cockpitFilterMax {
			break
		}
		if termsafe.IsUnsafeTerminalRune(r) {
			continue
		}
		m.filter = append(m.filter, r)
	}
	m.resetCursors()
}

func (m *cockpitModel) resetCursors() {
	for i := range m.secs {
		m.secs[i].cursor, m.secs[i].offset = 0, 0
	}
}

// visibleRows is section i's rows that match the filter.
func (m cockpitModel) visibleRows(i int) []CockpitRow {
	if i < 0 || i >= len(m.secs) {
		return nil
	}
	rows := m.secs[i].snap.Rows
	if len(m.filter) == 0 {
		return rows
	}
	needle := strings.ToLower(string(m.filter))
	var out []CockpitRow
	for _, r := range rows {
		// Only the fields a row draws: a match on a hidden path or ref would
		// show a row with nothing on screen explaining why it matched.
		hay := strings.ToLower(r.Label + "\x00" + r.Detail)
		if strings.Contains(hay, needle) {
			out = append(out, r)
		}
	}
	return out
}

func (m *cockpitModel) moveCursor(delta int) {
	if len(m.secs) == 0 {
		return
	}
	s := &m.secs[m.focus]
	n := len(m.visibleRows(m.focus))
	s.cursor += delta
	if s.cursor >= n {
		s.cursor = n - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

func (m *cockpitModel) clampCursor(i int) {
	s := &m.secs[i]
	n := len(m.visibleRows(i))
	if s.cursor >= n {
		s.cursor = max(n-1, 0)
	}
	if s.offset > s.cursor {
		s.offset = s.cursor
	}
}

// selectedRow is the focused section's row under the cursor.
func (m cockpitModel) selectedRow() (CockpitRow, bool) {
	if len(m.secs) == 0 {
		return CockpitRow{}, false
	}
	rows := m.visibleRows(m.focus)
	s := m.secs[m.focus]
	if s.cursor < 0 || s.cursor >= len(rows) {
		return CockpitRow{}, false
	}
	return rows[s.cursor], true
}

// activate is enter on the focused section's selected row.
func (m cockpitModel) activate() (tea.Model, tea.Cmd) {
	row, ok := m.selectedRow()
	if !ok {
		return m, nil
	}
	switch row.Kind {
	case CockpitRowPR:
		argv, err := m.buildArgv([]string{"pr"}, row.Ref, false)
		if err != nil {
			m.footer = errStatus("pr: ", err, m.styles)
			return m, nil
		}
		m.action = Action{Kind: ActionRunVerb, Argv: argv}
		return m, tea.Quit
	case CockpitRowProject:
		m.footer = m.styles.Muted.Render("enter opens PR rows; a project row has no action")
		return m, nil
	default:
		m.footer = m.styles.Muted.Render("nothing to open on this row")
		return m, nil
	}
}

// --- view ---

func (m cockpitModel) View() tea.View {
	header := m.headerView()
	footer := m.footerView()
	bodyHeight := m.bodyHeight()
	var body []string
	if m.help {
		body = m.helpLines()
	} else {
		body = m.sectionLines()
	}
	if len(body) > bodyHeight {
		body = body[:bodyHeight]
	}
	for len(body) < bodyHeight {
		body = append(body, "")
	}
	content := strings.Join(append(append([]string{header}, body...), footer), "\n")
	if m.width > 0 {
		content = lipgloss.NewStyle().MaxWidth(m.width).Render(content)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// headerView is the brand, then each section's age since its last result
// ("git 2m ago"), "refreshing…" while a refresh runs, or "loading…" before
// its first result.
func (m cockpitModel) headerView() string {
	brand := m.styles.Brand.Render(m.glyph.Forge) + "  " + m.styles.Header.Render(meta.AppName+" status")
	now := m.now()
	parts := make([]string, 0, len(m.secs))
	for _, s := range m.secs {
		name := capSafe(s.src.Name, cockpitNameMax)
		switch {
		case s.busy && s.updated.IsZero():
			parts = append(parts, name+" loading…")
		case s.busy:
			parts = append(parts, name+" refreshing…")
		default:
			age := ageLabel(now.Sub(s.updated))
			if age == "" {
				age = "<1m"
			}
			parts = append(parts, name+" "+age+" ago")
		}
	}
	return brand + m.styles.Muted.Render("  ·  "+strings.Join(parts, " · "))
}

func (m cockpitModel) glyphFor(st CockpitState) string {
	marks := m.theme.Marks()
	switch st {
	case CockpitOK:
		return marks.OK
	case CockpitDegraded:
		return marks.Warn
	case CockpitFailed:
		return marks.Fail
	default:
		return m.styles.Dim.Render("…")
	}
}

// sectionLines lays out every section in order. The focused one gets every
// line the others leave free for its rows (focusRoom), from the offset
// Update set; the others show their first cockpitCollapsedRows rows.
func (m cockpitModel) sectionLines() []string {
	free := m.focusRoom()
	var lines []string
	for i := range m.secs {
		if i == m.focus {
			lines = append(lines, m.focusedLines(i, free)...)
		} else {
			lines = append(lines, m.collapsedView(i)...)
		}
	}
	return lines
}

func (m cockpitModel) titleLine(i int) string {
	s := m.secs[i]
	name := fmt.Sprintf("%-5s", capSafe(s.src.Name, cockpitNameMax))
	if i == m.focus {
		name = m.styles.Selected.Render(name)
	} else {
		name = m.styles.Fg.Render(name)
	}
	var text string
	switch s.snap.State {
	case CockpitLoading:
		text = m.styles.Muted.Render("loading…")
	case CockpitFailed:
		text = m.styles.Danger.Render("failed: " + capSafe(s.snap.Error, cockpitErrorMax))
	default:
		text = m.styles.Fg.Render(capSafe(s.snap.Headline, cockpitHeadlineMax))
	}
	return m.glyphFor(s.snap.State) + " " + name + "  " + text
}

func (m cockpitModel) noteLines(i int) []string {
	var out []string
	for _, n := range m.secs[i].snap.Notes {
		out = append(out, "    "+m.styles.Warn.Render("note: "+capSafe(n, cockpitNoteMax)))
	}
	return out
}

func (m cockpitModel) collapsedView(i int) []string {
	lines := []string{m.titleLine(i)}
	lines = append(lines, m.noteLines(i)...)
	rows := m.visibleRows(i)
	for j := 0; j < len(rows) && j < cockpitCollapsedRows; j++ {
		lines = append(lines, m.rowLine(rows[j], false))
	}
	if hidden := len(rows) - cockpitCollapsedRows; hidden > 0 {
		lines = append(lines, m.styles.Muted.Render(fmt.Sprintf("    … %d more (tab to this section)", hidden)))
	}
	return lines
}

// focusedLines draws the focused section's rows from its offset, at most
// room of them. It reads the model only; Update keeps the offset in step.
func (m cockpitModel) focusedLines(i, room int) []string {
	lines := []string{m.titleLine(i)}
	lines = append(lines, m.noteLines(i)...)
	rows := m.visibleRows(i)
	s := m.secs[i]
	if len(rows) == 0 {
		if len(m.filter) > 0 && len(s.snap.Rows) > 0 {
			lines = append(lines, m.styles.Muted.Render("    (no rows match the filter)"))
		}
		return lines
	}
	for j := s.offset; j < len(rows) && j < s.offset+room; j++ {
		lines = append(lines, m.rowLine(rows[j], j == s.cursor))
	}
	return lines
}

func (m cockpitModel) rowLine(r CockpitRow, selected bool) string {
	label := capSafe(r.Label, cockpitLabelMax)
	detail := capSafe(r.Detail, cockpitDetailMax)
	pad := ""
	if n := utf8.RuneCountInString(label); n < 24 {
		pad = strings.Repeat(" ", 24-n)
	}
	if selected {
		return "  " + m.styles.Accent.Render("> ") + m.styles.Selected.Render(label) + pad + "  " + m.styles.Muted.Render(detail)
	}
	return "    " + m.styles.Fg.Render(label) + pad + "  " + m.styles.Muted.Render(detail)
}

func (m cockpitModel) helpLines() []string {
	s := m.styles
	rows := [][2]string{
		{"↑↓ j k", "move in the focused section"},
		{"tab ⇧tab", "next / previous section"},
		{"enter", "on a PR: quit and run forgectl pr <ref>, which starts a review"},
		{"/", "filter every section's rows; enter keeps it, esc clears it"},
		{"r", "refresh the focused section"},
		{"R", "refresh every section"},
		{"?", "close this help"},
		{"q esc", "quit (esc clears a filter first)"},
	}
	lines := []string{s.Header.Render("keys"), ""}
	for _, r := range rows {
		lines = append(lines, "  "+s.Accent.Render(fmt.Sprintf("%-10s", r[0]))+"  "+s.Fg.Render(r[1]))
	}
	lines = append(lines, "",
		s.Muted.Render("git refreshes itself every minute; prs, clean and bench refresh only when you ask."),
		s.Muted.Render("A section refreshes at most once every 15s, one refresh at a time. Only enter on a PR acts: it hands off to pr <ref>."))
	return lines
}

// footerView is two lines: the last result (or the filter being typed), then
// the key hints.
func (m cockpitModel) footerView() string {
	var first string
	switch {
	case m.filtering:
		first = m.styles.Accent.Render("/ ") + m.styles.Fg.Render(tailSafe(m.filter, cockpitFilterMax)) + m.styles.Accent.Render("▏")
	case len(m.filter) > 0 && m.footer == "":
		first = m.styles.Muted.Render("filter: " + capSafe(string(m.filter), cockpitFilterMax) + " (esc clears)")
	default:
		first = m.footer
	}
	// Hints drop from least to most important until the line fits, so the
	// quit and help keys are the last to go. enter shows only on a row it
	// acts on: it opens a PR, and does nothing on a project row (forgectl#1108).
	enter := ""
	if row, ok := m.selectedRow(); ok && row.Kind == CockpitRowPR {
		enter = "enter open"
	}
	hints := []string{"↑↓/jk move", "tab section", enter, "r refresh", "R all", "/ filter", "? help", "q quit"}
	prio := []int{7, 6, 0, 2, 5, 1, 3, 4}
	switch {
	case m.help:
		hints, prio = []string{"? / esc close help", "q quit"}, []int{1, 0}
	case m.filtering:
		hints, prio = []string{"type to filter", "enter keep", "esc clear"}, []int{2, 0, 1}
	}
	width := m.width
	if width <= 0 {
		width = 1 << 16
	}
	return first + "\n" + m.styles.Muted.Render(fitHints(width, hints, prio))
}
