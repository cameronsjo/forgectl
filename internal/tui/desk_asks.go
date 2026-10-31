// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// `forgectl desk ui`: the operator's asks and the desk's scripts on one
// screen, answered from the keyboard. The view reads the asks log through
// DeskAsksStore and never imports the cli package; scripts arrive as plain
// DeskAskScript rows from a loader the caller supplies. Every string an
// agent wrote (titles, steps, links, names) is untrusted and goes through
// termsafe before it is drawn.

// DeskAsksStore is the part of the asks store the view uses.
type DeskAsksStore interface {
	ReadFrom(off int64) ([]ask.Record, int64, int, error)
	Respond(ctx context.Context, r ask.Response) (ask.Item, ask.Response, error, error)
}

// DeskAskScript is one desk script shown read-only beside the asks.
type DeskAskScript struct {
	Name   string
	State  string // "waiting" or "running"
	What   string
	SHA256 string
}

// DeskAsksOptions configures RunDeskAsks and RenderDeskAsksFrame.
type DeskAsksOptions struct {
	Store DeskAsksStore
	// Scripts lists the desk's waiting and running scripts. nil shows none.
	Scripts func() ([]DeskAskScript, error)
	Theme   theme.Theme
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// Dir is the desk directory as shown in the header.
	Dir string
	// Poll is how often the log is re-read; 0 is one second.
	Poll time.Duration
}

const (
	deskAsksScriptEvery  = 5 * time.Second
	deskAsksAnsweredMax  = 5
	deskAsksSideMinWidth = 110
	deskAsksTitleMax     = 160
	deskAsksMinRows      = 8
	deskAsksMinCols      = 40
)

// RunDeskAsks runs the asks view until the operator quits.
func RunDeskAsks(ctx context.Context, opts DeskAsksOptions) error {
	m := newDeskAsksModel(ctx, opts)
	_, err := tea.NewProgram(m, keymap.ProgramOptions(ctx)...).Run()
	return err
}

// RenderDeskAsksFrame reads the store once and draws one frame.
func RenderDeskAsksFrame(ctx context.Context, opts DeskAsksOptions, width, height int) (string, error) {
	m := newDeskAsksModel(ctx, opts)
	if err := m.readLog(); err != nil {
		return "", err
	}
	m.loadScripts()
	m.width, m.height = width, height
	m.clampCursor()
	return m.View().Content, nil
}

type deskAsksMode int

const (
	deskAsksList deskAsksMode = iota
	deskAsksConfirmDone
	deskAsksReply
	deskAsksLink
	deskAsksSnooze
)

type (
	deskAsksTickMsg      struct{}
	deskAsksScriptTick   struct{}
	deskAsksRespondedMsg struct {
		item      ask.Item
		resp      ask.Response
		notifyErr error
		err       error
	}
)

// deskAsksRow is one line of the list: a heading, an ask, or a script.
type deskAsksRow struct {
	heading string
	item    *ask.Item
	script  *DeskAskScript
}

func (r deskAsksRow) selectable() bool { return r.heading == "" }

type deskAsksModel struct {
	ctx    context.Context
	opts   DeskAsksOptions
	styles theme.Styles
	now    func() time.Time

	records []ask.Record
	offset  int64
	bad     int
	items   []ask.Item
	loadErr string

	scripts   []DeskAskScript
	scriptErr string

	rows   []deskAsksRow
	cursor int

	width, height int
	detail        bool
	help          bool
	mode          deskAsksMode
	input         textinput.Model
	inputErr      string
	status        string
	statusWarn    bool
	busy          bool
}

func newDeskAsksModel(ctx context.Context, opts DeskAsksOptions) deskAsksModel {
	if opts.Poll <= 0 {
		opts.Poll = time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return deskAsksModel{ctx: ctx, opts: opts, styles: opts.Theme.Styles(), now: now, detail: true, input: textinput.New()}
}

func (m deskAsksModel) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return deskAsksTickMsg{} },
		func() tea.Msg { return deskAsksScriptTick{} },
	)
}

// readLog reads what the log gained since the last read and re-folds.
func (m *deskAsksModel) readLog() error {
	if m.opts.Store == nil {
		return nil
	}
	recs, next, bad, err := m.opts.Store.ReadFrom(m.offset)
	if err != nil {
		m.loadErr = err.Error()
		return err
	}
	m.loadErr = ""
	m.bad += bad
	m.records = append(m.records, recs...)
	m.offset = next
	m.refold()
	return nil
}

// refold rebuilds the items and rows, keeping the cursor on the same ask or
// script when it is still listed.
func (m *deskAsksModel) refold() {
	keep := m.selectedKey()
	m.items = ask.Fold(m.records, m.now())
	m.rebuildRows()
	if keep != "" {
		for i, r := range m.rows {
			if rowKey(r) == keep {
				m.cursor = i
				break
			}
		}
	}
	m.clampCursor()
}

func (m *deskAsksModel) loadScripts() {
	if m.opts.Scripts == nil {
		return
	}
	s, err := m.opts.Scripts()
	if err != nil {
		m.scriptErr = err.Error()
		return
	}
	m.scriptErr = ""
	keep := m.selectedKey()
	m.scripts = s
	m.rebuildRows()
	if keep != "" {
		for i, r := range m.rows {
			if rowKey(r) == keep {
				m.cursor = i
				break
			}
		}
	}
	m.clampCursor()
}

func rowKey(r deskAsksRow) string {
	switch {
	case r.item != nil:
		return "ask:" + r.item.Ask.ID
	case r.script != nil:
		return "script:" + r.script.Name
	}
	return ""
}

func (m deskAsksModel) selectedKey() string {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return rowKey(m.rows[m.cursor])
	}
	return ""
}

// rebuildRows lays the list out: open asks per bucket, snoozed, scripts,
// then the most recently answered.
func (m *deskAsksModel) rebuildRows() {
	open, snoozed, answered := ask.Grouped(m.items)
	var rows []deskAsksRow
	add := func(heading string, items []ask.Item) {
		if len(items) == 0 {
			return
		}
		rows = append(rows, deskAsksRow{heading: heading})
		for i := range items {
			rows = append(rows, deskAsksRow{item: &items[i]})
		}
	}
	for _, b := range ask.Buckets {
		add(b.Label(), open[b])
	}
	add("Snoozed", snoozed)
	if len(m.scripts) > 0 {
		rows = append(rows, deskAsksRow{heading: "Scripts (run from the desk dashboard)"})
		for i := range m.scripts {
			rows = append(rows, deskAsksRow{script: &m.scripts[i]})
		}
	}
	if len(answered) > deskAsksAnsweredMax {
		answered = answered[:deskAsksAnsweredMax]
	}
	add("Recently answered", answered)
	m.rows = rows
}

// clampCursor keeps the cursor on a selectable row.
func (m *deskAsksModel) clampCursor() {
	if len(m.rows) == 0 {
		m.cursor = 0
		return
	}
	m.cursor = min(max(m.cursor, 0), len(m.rows)-1)
	if m.rows[m.cursor].selectable() {
		return
	}
	for i := m.cursor; i < len(m.rows); i++ {
		if m.rows[i].selectable() {
			m.cursor = i
			return
		}
	}
	for i := m.cursor; i >= 0; i-- {
		if m.rows[i].selectable() {
			m.cursor = i
			return
		}
	}
}

func (m *deskAsksModel) move(delta int) {
	for i := m.cursor + delta; i >= 0 && i < len(m.rows); i += delta {
		if m.rows[i].selectable() {
			m.cursor = i
			return
		}
	}
}

// selectedAsk is the ask under the cursor, if the row is one and it is not
// answered.
func (m deskAsksModel) selectedAsk() (ask.Item, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) || m.rows[m.cursor].item == nil {
		return ask.Item{}, false
	}
	return *m.rows[m.cursor].item, true
}

func (m deskAsksModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		return m, nil
	case deskAsksTickMsg:
		_ = m.readLog() // the error shows in the header
		return m, tea.Tick(m.opts.Poll, func(time.Time) tea.Msg { return deskAsksTickMsg{} })
	case deskAsksScriptTick:
		m.loadScripts()
		return m, tea.Tick(deskAsksScriptEvery, func(time.Time) tea.Msg { return deskAsksScriptTick{} })
	case deskAsksRespondedMsg:
		m.busy = false
		m.status, m.statusWarn = respondedStatus(t)
		_ = m.readLog()
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(t)
	}
	if m.mode == deskAsksReply || m.mode == deskAsksLink {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// respondedStatus is the one-line outcome of a response.
func respondedStatus(t deskAsksRespondedMsg) (string, bool) {
	if t.err != nil {
		return "not recorded: " + termsafe.SafeLineMax(t.err.Error(), 160), true
	}
	verb := map[ask.Kind]string{ask.KindDone: "marked done", ask.KindReply: "replied to", ask.KindLink: "sent a link to", ask.KindSnooze: "snoozed"}[t.resp.Kind]
	s := verb + " " + termsafe.SafeLineMax(t.resp.Ask, 40)
	if t.resp.Kind == ask.KindSnooze && t.resp.Until != nil {
		s += " until " + t.resp.Until.Local().Format("Mon 15:04")
	}
	switch {
	case t.notifyErr != nil:
		return s + "; notify failed: " + termsafe.SafeLineMax(t.notifyErr.Error(), 120), true
	case t.item.Ask.Notify != "":
		return s + " (notified " + termsafe.SafeLineMax(t.item.Ask.Notify, 40) + ")", false
	}
	return s + " (no terminal to notify; the agent reads it with desk watch --responses)", false
}

func (m deskAsksModel) updateKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case deskAsksConfirmDone:
		m.mode = deskAsksList
		if key == "d" || key == "y" {
			return m.respond(ask.Response{Kind: ask.KindDone})
		}
		m.status, m.statusWarn = "not marked done", false
		return m, nil
	case deskAsksSnooze:
		m.mode = deskAsksList
		if until, ok := snoozeChoice(key, m.now()); ok {
			return m.respond(ask.Response{Kind: ask.KindSnooze, Until: &until})
		}
		m.status, m.statusWarn = "not snoozed", false
		return m, nil
	case deskAsksReply, deskAsksLink:
		return m.updateInput(k)
	}
	if m.help {
		if key == "?" || key == "esc" || key == "q" {
			m.help = false
		}
		return m, nil
	}
	switch key {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
	case "esc":
		m.status = ""
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "home", "g":
		m.cursor = 0
		m.clampCursor()
	case "end", "G":
		m.cursor = len(m.rows) - 1
		m.clampCursor()
	case "enter":
		m.detail = !m.detail
	case "d", "r", "l", "s":
		return m.startResponse(ask.Kind(map[string]string{"d": "done", "r": "reply", "l": "link", "s": "snooze"}[key]))
	}
	return m, nil
}

// startResponse opens the prompt for kind, when the selected ask is open and
// allows it. Anything else leaves the view as it was, with a status line
// saying why.
func (m deskAsksModel) startResponse(kind ask.Kind) (tea.Model, tea.Cmd) {
	it, ok := m.selectedAsk()
	switch {
	case m.busy:
		m.status, m.statusWarn = "still recording the last response", true
		return m, nil
	case !ok:
		m.status, m.statusWarn = "select an ask first (scripts run from the desk dashboard)", false
		return m, nil
	case it.State == ask.StateAnswered:
		m.status, m.statusWarn = "already answered", false
		return m, nil
	case !it.Ask.Allows(kind):
		m.status, m.statusWarn = fmt.Sprintf("this ask does not take %s", kind), false
		return m, nil
	}
	m.status, m.inputErr = "", ""
	switch kind {
	case ask.KindDone:
		m.mode = deskAsksConfirmDone
	case ask.KindSnooze:
		m.mode = deskAsksSnooze
	case ask.KindReply, ask.KindLink:
		m.mode = deskAsksReply
		m.input = textinput.New()
		m.input.Prompt = "reply> "
		m.input.CharLimit = ask.MaxReply
		if kind == ask.KindLink {
			m.mode = deskAsksLink
			m.input.Prompt = "link> "
			m.input.Placeholder = "https://…"
			m.input.CharLimit = ask.MaxURL
		}
		m.input.SetWidth(max(m.width-12, 20))
		return m, m.input.Focus()
	}
	return m, nil
}

func (m deskAsksModel) updateInput(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode, m.inputErr = deskAsksList, ""
		m.input.Blur()
		return m, nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		if v == "" {
			m.inputErr = "empty; type something or esc to cancel"
			return m, nil
		}
		if m.mode == deskAsksLink {
			if !ask.SafeURL(v) {
				m.inputErr = "only http and https links with a host are accepted"
				return m, nil
			}
			m.mode = deskAsksList
			m.input.Blur()
			return m.respond(ask.Response{Kind: ask.KindLink, URL: v})
		}
		m.mode = deskAsksList
		m.input.Blur()
		return m.respond(ask.Response{Kind: ask.KindReply, Text: v})
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	m.inputErr = ""
	return m, cmd
}

// respond records r against the selected ask, off the update loop: the
// notify runs hc, which can take seconds.
func (m deskAsksModel) respond(r ask.Response) (tea.Model, tea.Cmd) {
	it, ok := m.selectedAsk()
	if !ok || m.opts.Store == nil {
		return m, nil
	}
	r.Ask, r.Via = it.Ask.ID, "ui"
	m.busy = true
	m.status, m.statusWarn = "recording…", false
	store, ctx := m.opts.Store, m.ctx
	return m, func() tea.Msg {
		item, saved, notifyErr, err := store.Respond(ctx, r)
		if err == nil && item.Ask.ID == "" {
			item = it
		}
		return deskAsksRespondedMsg{item: item, resp: saved, notifyErr: notifyErr, err: err}
	}
}

// snoozeChoice maps the snooze prompt's keys to an end time.
func snoozeChoice(key string, now time.Time) (time.Time, bool) {
	switch key {
	case "1":
		return now.Add(time.Hour), true
	case "2":
		return now.Add(4 * time.Hour), true
	case "3":
		l := now.Local()
		return time.Date(l.Year(), l.Month(), l.Day()+1, 9, 0, 0, 0, l.Location()), true
	case "4":
		return now.Add(7 * 24 * time.Hour), true
	}
	return time.Time{}, false
}

// --- view ---

func (m deskAsksModel) View() tea.View {
	width, height := m.width, m.height
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	var lines []string
	switch {
	case width < deskAsksMinCols || height < deskAsksMinRows:
		lines = []string{m.styles.Warn.Render(fmt.Sprintf("too small: need %dx%d · q quit", deskAsksMinCols, deskAsksMinRows))}
	case m.help:
		lines = clipLines(m.helpLines(), height)
	default:
		lines = m.mainLines(width, height)
	}
	content := lipgloss.NewStyle().MaxWidth(width).Render(strings.Join(lines, "\n"))
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m deskAsksModel) headerLine() string {
	open, snoozed, _ := ask.Grouped(m.items)
	parts := []string{"q quit · ? help"}
	for _, b := range ask.Buckets {
		if n := len(open[b]); n > 0 {
			s := fmt.Sprintf("%d %s", n, b)
			if b == ask.BucketNow {
				s = m.styles.Danger.Render(s)
			}
			parts = append(parts, s)
		}
	}
	if len(snoozed) > 0 {
		parts = append(parts, fmt.Sprintf("%d snoozed", len(snoozed)))
	}
	if len(m.scripts) > 0 {
		parts = append(parts, plural(len(m.scripts), "script", "scripts"))
	}
	if m.opts.Dir != "" {
		parts = append(parts, termsafe.SafeLineMax(m.opts.Dir, 60))
	}
	return m.styles.Brand.Render("◆") + " " + m.styles.Header.Render("desk") + "  " + m.styles.Muted.Render(strings.Join(parts, " · "))
}

func (m deskAsksModel) mainLines(width, height int) []string {
	top := []string{m.headerLine()}
	if m.loadErr != "" {
		top = append(top, m.styles.Danger.Render("cannot read the asks log: "+termsafe.SafeLineMax(m.loadErr, 160)))
	}
	if m.scriptErr != "" {
		top = append(top, m.styles.Warn.Render("scripts: "+termsafe.SafeLineMax(m.scriptErr, 160)))
	}
	if m.bad > 0 {
		top = append(top, m.styles.Warn.Render(plural(m.bad, "log line", "log lines")+" could not be read and were skipped"))
	}
	top = append(top, "")
	footer := m.footerLines(width)
	room := max(height-len(top)-len(footer), 1)

	side := m.detail && width >= deskAsksSideMinWidth
	listWidth := width
	if side {
		listWidth = width * 11 / 20
	}
	list := m.listLines(listWidth)
	var body []string
	switch {
	case !m.detail:
		body = window(list, m.cursorLine(), room)
	case side:
		left := window(list, m.cursorLine(), room)
		right := clipLines(m.detailLines(width-listWidth-3), room)
		body = joinColumns(left, right, listWidth, room)
	default:
		listRoom := min(len(list), max(room/2, min(4, room)))
		body = window(list, m.cursorLine(), listRoom)
		det := m.detailLines(width - 2)
		if len(det) > 0 && room-len(body) > 1 {
			body = append(body, m.styles.Dim.Render(strings.Repeat("─", min(width, 80))))
			body = append(body, clipLines(det, room-len(body))...)
		}
	}
	for len(body) < room {
		body = append(body, "")
	}
	return clipLines(append(append(top, body...), footer...), height)
}

// cursorLine is the index in listLines of the selected row.
func (m deskAsksModel) cursorLine() int { return m.cursor }

// window keeps n lines of lines around index at.
func window(lines []string, at, n int) []string {
	if len(lines) <= n {
		return lines
	}
	start := 0
	if at >= n {
		start = at - n + 1
	}
	return lines[start : start+n]
}

// joinColumns puts left and right side by side, left padded to width w.
func joinColumns(left, right []string, w, n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		l = ansi.Truncate(l, w, "…")
		pad := max(w-lipgloss.Width(l), 0)
		out = append(out, l+strings.Repeat(" ", pad)+" │ "+r)
	}
	return out
}

func (m deskAsksModel) listLines(width int) []string {
	if len(m.rows) == 0 {
		return []string{m.styles.Muted.Render("  nothing on the desk: no open asks and no waiting scripts")}
	}
	lines := make([]string, 0, len(m.rows))
	for i, r := range m.rows {
		sel := i == m.cursor
		switch {
		case r.heading != "":
			lines = append(lines, m.styles.Header.Render(r.heading))
		case r.item != nil:
			lines = append(lines, m.askRowLine(*r.item, sel, width))
		case r.script != nil:
			lines = append(lines, m.scriptRowLine(*r.script, sel, width))
		}
	}
	return lines
}

func (m deskAsksModel) marker(sel bool) string {
	if sel {
		return m.styles.Selected.Render("▸ ")
	}
	return "  "
}

func (m deskAsksModel) askRowLine(it ask.Item, sel bool, width int) string {
	title := capSafe(it.Ask.Title, max(min(width-24, deskAsksTitleMax), 10))
	var tail string
	switch it.State {
	case ask.StateSnoozed:
		tail = "until " + it.SnoozedUntil.Local().Format("Mon 15:04")
	case ask.StateAnswered:
		tail = string(it.Answer.Kind) + " " + ageLabel(m.now().Sub(it.Answer.At)) + " ago"
	default:
		if a := ageLabel(m.now().Sub(it.Ask.At)); a != "" {
			tail = a
		}
	}
	if sel {
		title = m.styles.Selected.Render(title)
	} else if it.State == ask.StateAnswered {
		title = m.styles.Muted.Render(title)
	}
	return m.marker(sel) + title + "  " + m.styles.Dim.Render(tail)
}

func (m deskAsksModel) scriptRowLine(s DeskAskScript, sel bool, width int) string {
	sha := s.SHA256
	if len(sha) > 12 {
		sha = sha[:12]
	}
	name := capSafe(s.Name, 28)
	what := capSafe(s.What, max(width-len(name)-30, 10))
	if sel {
		name = m.styles.Selected.Render(name)
	}
	return m.marker(sel) + name + "  " + what + "  " + m.styles.Dim.Render(termsafe.SafeLine(s.State)+" "+termsafe.SafeLine(sha))
}

// detailLines describes the selected row.
func (m deskAsksModel) detailLines(width int) []string {
	width = max(width, 20)
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	r := m.rows[m.cursor]
	s := m.styles
	if r.script != nil {
		sc := *r.script
		return []string{
			s.Header.Render(capSafe(sc.Name, width)),
			capSafe(sc.What, width),
			s.Muted.Render("sha256 ") + termsafe.SafeLineMax(sc.SHA256, 64),
			s.Muted.Render("state  ") + termsafe.SafeLine(sc.State),
			"",
			s.Muted.Render("Read only here: run or skip it from the desk dashboard (forgectl desk)."),
		}
	}
	if r.item == nil {
		return nil
	}
	it := *r.item
	a := it.Ask
	lines := []string{s.Header.Render(wrapSafe(a.Title, width)[0])}
	for _, l := range wrapSafe(a.Title, width)[1:] {
		lines = append(lines, s.Header.Render(l))
	}
	meta := []string{termsafe.SafeLine(a.ID), termsafe.SafeLine(string(a.Bucket))}
	if a.From != "" {
		meta = append(meta, "from "+capSafe(a.From, 40))
	}
	if a.Notify != "" {
		meta = append(meta, "notifies "+capSafe(a.Notify, 30))
	}
	lines = append(lines, s.Muted.Render(strings.Join(meta, " · ")), "")
	if a.Steps != "" {
		for _, raw := range strings.Split(a.Steps, "\n") {
			lines = append(lines, wrapSafe(raw, width)...)
		}
		lines = append(lines, "")
	}
	for _, l := range a.Links {
		label := l.Label
		if label == "" {
			label = "link"
		}
		lines = append(lines, s.Accent.Render(capSafe(label, 40))+" "+termsafe.SafeLineMax(l.URL, max(width-lipgloss.Width(label)-2, 20)))
	}
	if len(a.Links) > 0 {
		lines = append(lines, "")
	}
	switch it.State {
	case ask.StateAnswered:
		lines = append(lines, s.OK.Render("answered: "+answerText(*it.Answer)))
	default:
		var keys []string
		for _, k := range []struct {
			key  string
			kind ask.Kind
			text string
		}{{"d", ask.KindDone, "d done"}, {"r", ask.KindReply, "r reply"}, {"l", ask.KindLink, "l link"}, {"s", ask.KindSnooze, "s snooze"}} {
			if a.Allows(k.kind) {
				keys = append(keys, k.text)
			}
		}
		lines = append(lines, s.Accent.Render("respond: "+strings.Join(keys, " · ")))
	}
	for _, n := range it.Notified {
		if !n.OK {
			lines = append(lines, s.Warn.Render("notify to "+capSafe(n.Address, 30)+" failed: "+capSafe(n.Error, 100)))
		}
	}
	return lines
}

func answerText(r ask.Response) string {
	switch r.Kind {
	case ask.KindReply:
		return "reply: " + termsafe.SafeLineMax(r.Text, 200)
	case ask.KindLink:
		return "link: " + termsafe.SafeLineMax(r.URL, 200)
	}
	return string(r.Kind)
}

// wrapSafe makes untrusted text inert and wraps it to width. It always
// returns at least one line.
func wrapSafe(s string, width int) []string {
	s = strings.ReplaceAll(s, "\t", "    ")
	safe := termsafe.SafeLine(s)
	wrapped := ansi.Wordwrap(safe, width, " ")
	out := strings.Split(ansi.Hardwrap(wrapped, width, true), "\n")
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func (m deskAsksModel) footerLines(width int) []string {
	s := m.styles
	var lines []string
	switch m.mode {
	case deskAsksConfirmDone:
		lines = append(lines, s.Warn.Render("mark done? d or y confirms, any other key cancels"))
	case deskAsksSnooze:
		lines = append(lines, s.Warn.Render("snooze for: 1 one hour · 2 four hours · 3 until 9:00 tomorrow · 4 one week · other key cancels"))
	case deskAsksReply, deskAsksLink:
		if m.inputErr != "" {
			lines = append(lines, s.Danger.Render(m.inputErr))
		}
		lines = append(lines, m.input.View(), s.Muted.Render("enter sends · esc cancels"))
		return lines
	}
	if m.status != "" {
		st := s.OK
		if m.statusWarn {
			st = s.Warn
		}
		lines = append(lines, st.Render(ansi.Truncate(m.status, width, "…")))
	}
	lines = append(lines, s.Muted.Render(ansi.Truncate("↑/↓ select · enter details · d done · r reply · l link · s snooze · ? help · q quit", width, "…")))
	return lines
}

func (m deskAsksModel) helpLines() []string {
	return []string{
		m.styles.Header.Render("desk ui: keys"),
		"",
		"  ↑/↓ or j/k   select an ask or script",
		"  g / G        first / last",
		"  enter        show or hide the details pane",
		"  d            mark the ask done; press d or y again to confirm",
		"  r            reply with text; enter sends, esc cancels",
		"  l            answer with a link (http or https only)",
		"  s            snooze: 1 one hour, 2 four hours, 3 until 9:00 tomorrow, 4 one week",
		"  ?            this help; ? or esc closes it",
		"  q, ctrl+c    quit",
		"",
		"  An ask offers only the responses its agent allowed. A response is",
		"  appended to the desk's asks log and, when the ask names a terminal,",
		"  sent there with hc. Scripts are read-only here: run or skip them",
		"  from the desk dashboard (forgectl desk).",
	}
}
