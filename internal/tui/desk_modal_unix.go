// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	binding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The dialogs over the dashboard (a step, an event, the run switcher, and the
// approve dialog), replay, and the queue verbs.
//
// The approve dialog is the operator's approval control. It works on a copy
// of the queue item frozen when it opened, and hands Claim that copy's full
// hash and nothing else. Only a y key press confirms, and only once the
// dialog has been open confirmDelay by the model's clock and the body has
// been seen to its last line: enter, clicks, held-key repeats and keys typed
// ahead never do.

const (
	// confirmDelay is how long the approve dialog must be open before y
	// counts, so a y typed straight after a (or buffered with it) cannot
	// approve what the operator has not seen.
	confirmDelay = 400 * time.Millisecond
	// replayEvery paces autoplay at four events a second.
	replayEvery = 250 * time.Millisecond

	modalMinW = 36
	modalMinH = 8

	// deskLaunchFailed is the skip reason for a claim whose run never
	// started.
	deskLaunchFailed = "launch-failed"
)

// shaRe is what a hash the dialog may approve looks like.
var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type modalKind int

const (
	modalStep modalKind = iota + 1
	modalEvent
	modalSwitcher
	modalConfirm
)

// deskModal is one open dialog. The model keeps a stack of them; esc closes
// the top one.
type deskModal struct {
	kind modalKind
	run  deskRunKey
	step string // modalStep: the step's id
	ev   int    // modalEvent: the index into the run's events
	cur  int    // modalStep: the selected row among the step's events
	vp   viewport.Model
	list list.Model // modalSwitcher

	// modalConfirm.
	item     runview.QueueItem // the frozen copy; nothing replaces it
	openedAt time.Time
	seenEnd  bool // the body's last line has been on screen
	busy     bool // a claim is in flight
	note     string
}

// deskVerbs is what the queue verbs need from the desk. *desk.Desk has these
// methods; tests stand in for Launch, which starts a detached supervisor.
type deskVerbs interface {
	Claim(name, wantSHA string) (*desk.Claimed, error)
	Launch(c *desk.Claimed) (int, error)
	Abandon(name, reason string) error
	Skip(name, reason string) error
	Unskip(name string) error
}

// deskReplay is a replay of one run: the state after its first i of n
// events. n is the event count when the replay began; events that arrive
// during it are counted, not shown.
type deskReplay struct {
	key     deskRunKey
	i, n    int
	playing bool
	gen     int // the autoplay tick that is current; older ticks are dropped
	view    *deskRun
}

type (
	// deskApproveMsg is how a background approval ended.
	deskApproveMsg struct {
		name, sha string
		pid       int
		err       error
		abandoned bool
	}
	// deskVerbMsg is how a skip or unskip ended.
	deskVerbMsg struct {
		verb, name string
		err        error
	}
	deskReplayTickMsg struct{ gen int }
)

// verbsOf is the first source's desk, as queue verbs.
func verbsOf(sources []runview.Source) deskVerbs {
	for _, src := range sources {
		if p, ok := src.(runview.DeskProvider); ok && p.Desk() != nil {
			return p.Desk()
		}
	}
	return nil
}

func deskModelHooks() deskHooks {
	return deskHooks{
		open:     func(m deskModel) (deskModel, tea.Cmd) { return m.openFocused() },
		verb:     func(m deskModel, v string) (deskModel, tea.Cmd) { return m.queueVerb(v) },
		switcher: func(m deskModel) (deskModel, tea.Cmd) { return m.openSwitcher(), nil },
		replay:   func(m deskModel) (deskModel, tea.Cmd) { return m.toggleReplay(), nil },
	}
}

// --- the modal stack ---

func (m deskModel) topModal() *deskModal {
	if len(m.modals) == 0 {
		return nil
	}
	return &m.modals[len(m.modals)-1]
}

func (m *deskModel) pushModal(md deskModal) {
	md.vp = viewport.New()
	m.modals = append(slices.Clone(m.modals), md)
}

func (m *deskModel) popModal() {
	m.modals = slices.Clone(m.modals[:len(m.modals)-1])
}

// modalRect is where the top dialog sits: centred, at most 80% of the window
// each way and at least 36×8, never past the window.
func (m deskModel) modalRect() (x, y, w, h int) {
	w = min(max(m.width*8/10, modalMinW), m.width)
	h = min(max(m.height*8/10, modalMinH), m.height)
	return (m.width - w) / 2, (m.height - h) / 2, w, h
}

// modalFootH is the rows a dialog keeps under its scrolling body.
func modalFootH(md *deskModal) int {
	if md.kind == modalConfirm {
		return 2
	}
	return 1
}

// modalBodySize is the size of the dialog's scrolling body.
func (m deskModel) modalBodySize(md *deskModal) (w, h int) {
	_, _, mw, mh := m.modalRect()
	return max(mw-4, 1), max(mh-2-modalFootH(md), 1)
}

// syncModal lays the top dialog out at the current size and content. It runs
// in Update, so View only reads and the approve dialog learns when its last
// line has been on screen.
func (m *deskModel) syncModal() {
	md := m.topModal()
	if md == nil {
		return
	}
	w, h := m.modalBodySize(md)
	if md.kind == modalSwitcher {
		md.list.SetSize(w, h)
		return
	}
	lines, _ := m.modalLines(md, w)
	md.vp.SetWidth(w)
	md.vp.SetHeight(h)
	md.vp.SetContentLines(lines)
	md.vp.SetYOffset(md.vp.YOffset()) // clamp after a resize or new content
	if md.kind == modalConfirm && m.layout().kind != layoutSmall && md.vp.AtBottom() {
		md.seenEnd = true
	}
}

// modalLines is the dialog's body at width w, and for a step dialog the body
// line each of its rows starts on.
func (m deskModel) modalLines(md *deskModal, w int) ([]string, []int) {
	switch md.kind {
	case modalConfirm:
		return m.confirmLines(md, w), nil
	case modalStep:
		return m.stepLines(md, w)
	case modalEvent:
		return m.eventLines(md, w), nil
	}
	return nil, nil
}

// wrapText wraps s, already safe and styled, to w columns.
func wrapText(s string, w int) []string {
	return strings.Split(ansi.Wrap(s, max(w, 1), ""), "\n")
}

// --- keys ---

// onModalKey handles a key while a dialog is open. Only ctrl+c gets past it.
func (m deskModel) onModalKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	md := m.topModal()
	switch md.kind {
	case modalConfirm:
		return m.onConfirmKey(md, k)
	case modalSwitcher:
		return m.onSwitcherKey(md, k)
	}
	switch {
	case binding.Matches(k, m.keys.Back, m.keys.Quit):
		m.popModal()
	case binding.Matches(k, m.keys.Up):
		m.modalMove(md, -1)
	case binding.Matches(k, m.keys.Down):
		m.modalMove(md, 1)
	case binding.Matches(k, m.keys.Top):
		md.cur = 0
		md.vp.GotoTop()
	case binding.Matches(k, m.keys.Bottom):
		md.vp.GotoBottom()
		if md.kind == modalStep {
			md.cur = max(len(m.stepEvents(md))-1, 0)
		}
	case binding.Matches(k, m.keys.Open):
		if md.kind == modalStep {
			return m.openStepEvent(md), nil
		}
	}
	return m, nil
}

// modalMove moves a step dialog's row cursor, or scrolls any other dialog.
func (m deskModel) modalMove(md *deskModal, delta int) {
	if md.kind != modalStep {
		if delta < 0 {
			md.vp.ScrollUp(-delta)
		} else {
			md.vp.ScrollDown(delta)
		}
		return
	}
	n := len(m.stepEvents(md))
	md.cur = min(max(md.cur+delta, 0), max(n-1, 0))
	w, h := m.modalBodySize(md)
	_, rowLine := m.stepLines(md, w)
	if md.cur < len(rowLine) {
		line := rowLine[md.cur]
		switch {
		case line < md.vp.YOffset():
			md.vp.SetYOffset(line)
		case line >= md.vp.YOffset()+h:
			md.vp.SetYOffset(line - h + 1)
		}
	}
}

func (m deskModel) onConfirmKey(md *deskModal, k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case binding.Matches(k, m.keys.Confirm):
		return m.confirm(md, k)
	case binding.Matches(k, m.keys.Cancel, m.keys.Back, m.keys.Quit):
		if md.busy {
			md.note = "starting; wait for the result"
			return m, nil
		}
		m.popModal()
		return m, nil
	case binding.Matches(k, m.keys.Up):
		md.vp.ScrollUp(1)
	case binding.Matches(k, m.keys.Down):
		md.vp.ScrollDown(1)
	case k.String() == "pgup":
		md.vp.PageUp()
	case k.String() == "pgdown", k.String() == "space":
		md.vp.PageDown()
	case binding.Matches(k, m.keys.Top):
		md.vp.GotoTop()
	case binding.Matches(k, m.keys.Bottom):
		md.vp.GotoBottom()
	case binding.Matches(k, m.keys.Open):
		md.note = "enter does not approve; y does"
	}
	return m, nil
}

// confirm is a y in the approve dialog. It approves only when every binding
// rule holds; otherwise it says which one did not.
func (m deskModel) confirm(md *deskModal, k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case k.IsRepeat || k.Mod != 0 || k.String() != "y":
		return m, nil // a held key, or y with a modifier, never approves
	case md.busy:
		return m, nil
	case m.layout().kind == layoutSmall:
		return m, nil // the dialog is not on screen
	case m.now().Sub(md.openedAt) < confirmDelay:
		md.note = "too soon: y counts once the dialog has been open a moment"
		return m, nil
	case !md.seenEnd:
		md.note = "read it to the end first (↓ or G)"
		return m, nil
	}
	it := md.item
	if why := cannotApprove(it); why != "" {
		md.note = why
		return m, nil
	}
	if it.TTY && it.Kind == string(desk.KindScript) {
		// A TTY item runs in this terminal, after the dashboard lets go of
		// it; the caller claims it with this hash.
		m.action = DeskRun{Name: it.Name, SHA: it.SHA256}
		return m, tea.Quit
	}
	if m.verbs == nil {
		md.note = "no desk to run it on"
		return m, nil
	}
	md.busy, md.note = true, "starting…"
	return m, approveCmd(m.verbs, it.Name, it.SHA256)
}

// cannotApprove says why it cannot be approved, or "".
func cannotApprove(it runview.QueueItem) string {
	switch {
	case it.Refusal != "":
		return "cannot approve: " + capSafe(it.Refusal, 240)
	case it.State != string(desk.StateWaiting):
		return "cannot approve: it is " + capSafe(it.State, 16) + ", not waiting"
	case !shaRe.MatchString(it.SHA256):
		return "cannot approve: it has no full recorded hash"
	}
	return ""
}

// approveCmd claims name with exactly sha and launches it. Anything that
// fails after the claim and before the supervisor starts gives the claim back
// as skipped:launch-failed. A Launch that returns a pid with its error
// started the supervisor, which owns the item from then on.
func approveCmd(v deskVerbs, name, sha string) tea.Cmd {
	return func() tea.Msg {
		c, err := v.Claim(name, sha)
		if err != nil {
			return deskApproveMsg{name: name, sha: sha, err: err}
		}
		var pid int
		if c == nil || c.SHA256 != sha {
			err = fmt.Errorf("desk: the claim for %s does not carry the approved hash", name)
		} else {
			pid, err = v.Launch(c)
		}
		if err == nil || pid != 0 {
			return deskApproveMsg{name: name, sha: sha, pid: pid, err: err}
		}
		aerr := v.Abandon(name, deskLaunchFailed)
		return deskApproveMsg{name: name, sha: sha, err: errors.Join(err, aerr), abandoned: aerr == nil}
	}
}

// onApprove applies a background approval's result.
func (m deskModel) onApprove(msg deskApproveMsg) (tea.Model, tea.Cmd) {
	if md := m.topModal(); md != nil && md.kind == modalConfirm && md.item.Name == msg.name {
		m.popModal()
	}
	st := m.styles
	name := capSafe(msg.name, 48)
	switch {
	case msg.err == nil:
		m.footer = st.OK.Render(fmt.Sprintf("%s started %s in the background (pid %d)", m.glyph.Done, name, msg.pid))
	case msg.pid != 0:
		m.footer = st.Warn.Render(fmt.Sprintf("! started %s (pid %d), but: %s", name, msg.pid, capSafe(msg.err.Error(), 200)))
	case errors.Is(msg.err, desk.ErrChanged):
		m.footer = st.Warn.Render("! " + name + " changed since you opened it")
		m.reopen = msg.name
	case errors.Is(msg.err, desk.ErrClaimed):
		m.footer = st.Warn.Render("! " + name + ": another desk claimed it")
	case errors.Is(msg.err, desk.ErrRefused):
		m.footer = st.Danger.Render(m.glyph.Failed + " " + capSafe(msg.err.Error(), 240))
	default:
		text := m.glyph.Failed + " could not run " + name + ": " + capSafe(msg.err.Error(), 200)
		if msg.abandoned {
			text += " · moved to skipped: " + deskLaunchFailed
		}
		m.footer = st.Danger.Render(text)
	}
	return m.relist()
}

// relist starts a list now, unless one is running.
func (m deskModel) relist() (tea.Model, tea.Cmd) {
	if m.listBusy {
		return m, nil
	}
	m.listBusy = true
	return m, m.listCmd()
}

// recheckConfirm closes the approve dialog when the refreshed queue no longer
// lists its item as it was frozen, and re-opens one asked for after a claim
// found the item changed.
func (m *deskModel) recheckConfirm() {
	if md := m.topModal(); md != nil && md.kind == modalConfirm && !md.busy {
		it, ok := m.queueItem(md.item.Name)
		if !ok || it.SHA256 != md.item.SHA256 || it.State != md.item.State || it.Refusal != md.item.Refusal {
			m.popModal()
			m.footer = m.styles.Warn.Render("! " + capSafe(md.item.Name, 48) + " changed — re-review")
		}
	}
	if name := m.reopen; name != "" {
		m.reopen = ""
		if it, ok := m.queueItem(name); ok && cannotApprove(it) == "" && m.topModal() == nil {
			m.openConfirm(it)
			m.footer = m.styles.Warn.Render("! " + capSafe(name, 48) + " changed since you opened it; this is the new content")
		}
	}
}

func (m deskModel) queueItem(name string) (runview.QueueItem, bool) {
	for _, it := range m.items {
		if it.Name == name {
			return it, true
		}
	}
	return runview.QueueItem{}, false
}

// --- queue verbs ---

// selectedItem is the queue row under the cursor.
func (m deskModel) selectedItem() (runview.QueueItem, bool) {
	items := m.queueOrder()
	if len(items) == 0 {
		return runview.QueueItem{}, false
	}
	return items[min(max(m.cur[viewQueue], 0), len(items)-1)], true
}

func (m deskModel) queueVerb(verb string) (deskModel, tea.Cmd) {
	st := m.styles
	it, ok := m.selectedItem()
	if !ok {
		m.footer = st.Muted.Render("queue empty")
		return m, nil
	}
	name := capSafe(it.Name, 48)
	switch verb {
	case "approve":
		if why := cannotApprove(it); why != "" {
			m.footer = st.Danger.Render(m.glyph.Failed + " " + name + ": " + why)
			return m, nil
		}
		if m.layout().kind == layoutSmall {
			return m, nil
		}
		m.openConfirm(it)
		return m, nil
	case "skip":
		if it.State != string(desk.StateWaiting) && it.State != string(desk.StateLost) {
			m.footer = st.Muted.Render("only a waiting or lost item can be skipped")
			return m, nil
		}
	case "unskip":
		if it.State != string(desk.StateSkipped) {
			m.footer = st.Muted.Render("only a skipped item can be unskipped")
			return m, nil
		}
		if it.SkipReason != desk.SkipOperator {
			m.footer = st.Warn.Render("! " + name + ": " + skipHint(it.SkipReason))
			return m, nil
		}
	}
	if m.verbs == nil {
		m.footer = st.Muted.Render("no desk to act on")
		return m, nil
	}
	v, raw := m.verbs, it.Name
	return m, func() tea.Msg {
		var err error
		if verb == "skip" {
			err = v.Skip(raw, desk.SkipOperator)
		} else {
			err = v.Unskip(raw)
		}
		return deskVerbMsg{verb: verb, name: raw, err: err}
	}
}

// skipHint is why an item skipped for reason cannot be unskipped.
func skipHint(reason string) string {
	switch reason {
	case desk.SkipChanged:
		return "changed — re-queue to run it"
	case deskLaunchFailed:
		return "launch failed — re-queue to run it"
	case desk.SkipLost:
		return "lost — re-queue to run it"
	}
	return "skipped (" + capSafe(reason, 64) + ") — only an operator skip can be undone; re-queue to run it"
}

func (m deskModel) onVerb(msg deskVerbMsg) (tea.Model, tea.Cmd) {
	st := m.styles
	name := capSafe(msg.name, 48)
	switch {
	case msg.err != nil:
		m.footer = st.Danger.Render(m.glyph.Failed + " " + msg.verb + " " + name + ": " + capSafe(msg.err.Error(), 200))
	case msg.verb == "skip":
		m.footer = st.Muted.Render(m.glyph.Skipped + " skipped " + name + " · u to undo")
	default:
		m.footer = st.Muted.Render(m.glyph.Pending + " unskipped " + name + ": waiting again")
	}
	return m.relist()
}

// --- the approve dialog ---

// openConfirm opens the approve dialog on a frozen copy of it.
func (m *deskModel) openConfirm(it runview.QueueItem) {
	it.Body = slices.Clone(it.Body)
	it.Marks = slices.Clone(it.Marks)
	if it.Exit != nil {
		rc := *it.Exit
		it.Exit = &rc
	}
	m.pushModal(deskModal{kind: modalConfirm, item: it, openedAt: m.now()})
}

func (m deskModel) confirmTitle(md *deskModal) string {
	where := "in the background"
	if md.item.TTY && md.item.Kind == string(desk.KindScript) {
		where = "in this terminal"
	}
	return "Run " + capSafe(md.item.Name, 64) + " now — " + where
}

// confirmLines is the approve dialog's body: what the item is, its counts,
// and every line of it, numbered and wrapped, with the lines that hold a rune
// worth a second look marked.
func (m deskModel) confirmLines(md *deskModal, w int) []string {
	st := m.styles
	it := md.item
	var out []string
	add := func(s string) { out = append(out, wrapText(s, w)...) }

	kind := st.Muted.Render("kind ") + st.Fg.Render(capSafe(it.Kind, 16))
	if it.TTY {
		kind += st.Muted.Render(" · tty")
	}
	add(kind)
	add(st.Muted.Render("WHAT ") + st.Fg.Render(capSafe(it.What, 400)))
	add(st.Muted.Render("WHY  ") + st.Fg.Render(capSafe(it.Why, 400)))
	add(st.Muted.Render("sha256 ") + st.Fg.Render(capSafe(it.SHA256, 64)))

	var esc, inv, non, marked int
	for _, mk := range it.Marks {
		esc += mk.Escaped
		inv += mk.Invisible
		non += mk.NonASCII
		if mk.Any() {
			marked++
		}
	}
	count := func(n int, word string) string {
		s := fmt.Sprintf("%d %s", n, word)
		if n > 0 {
			return st.Warn.Render(s)
		}
		return st.Muted.Render(s)
	}
	sep := st.Muted.Render(" · ")
	stats := st.Fg.Render(fmt.Sprintf("%d lines", len(it.Body))) + sep + st.Fg.Render(fmt.Sprintf("%d bytes", it.Bytes)) + sep +
		count(esc, "escaped runes") + sep + count(inv, "invisible") + sep + count(non, "non-ASCII")
	if marked > 0 {
		stats += sep + st.Warn.Render(fmt.Sprintf("! marks %d lines", marked))
	}
	add(stats)
	if it.Kind == string(desk.KindBatch) {
		add(st.Warn.Render("! hash covers the manifest's lines, not the scripts it calls"))
	}
	out = append(out, "")

	numW := len(strconv.Itoa(len(it.Body)))
	textW := max(w-numW-2, 1)
	for i, line := range it.Body {
		mark := " "
		if i < len(it.Marks) && it.Marks[i].Any() {
			mark = st.Warn.Render("!")
		}
		// The source escaped the body already; escaping again is a no-op
		// on its output and covers a source that did not.
		for j, part := range wrapText(m.revealInvisible(termsafe.SafeLine(line)), textW) {
			num := fmt.Sprintf("%*d", numW, i+1)
			if j > 0 {
				num, mark = strings.Repeat(" ", numW), " "
			}
			out = append(out, st.Muted.Render(num)+mark+st.Dim.Render("│")+part)
		}
	}
	return out
}

// revealInvisible draws a body line, already escaped, undimmed, with each
// rune that is drawn yet looks like nothing spelled out as \uXXXX.
func (m deskModel) revealInvisible(line string) string {
	st := m.styles
	var b, plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			b.WriteString(st.Fg.Render(plain.String()))
			plain.Reset()
		}
	}
	for _, r := range line {
		if termsafe.IsInvisibleRune(r) {
			flush()
			b.WriteString(st.Warn.Render(fmt.Sprintf(`\u%04X`, r)))
			continue
		}
		plain.WriteRune(r)
	}
	flush()
	return b.String()
}

// --- the step and event dialogs ---

// openFocused is enter, or a second click: it opens the selected step or
// event of the focused view.
func (m deskModel) openFocused() (deskModel, tea.Cmd) {
	e := m.sel()
	switch m.focusedView() {
	case viewFlow:
		if e == nil || len(e.state.Steps) == 0 {
			return m, nil
		}
		s := e.state.Steps[min(max(m.cur[viewFlow], 0), len(e.state.Steps)-1)]
		m.pushModal(deskModal{kind: modalStep, run: e.key, step: s.ID})
	case viewEvents:
		if e == nil {
			return m, nil
		}
		idx := m.eventIndexes(e)
		if len(idx) == 0 {
			return m, nil
		}
		m.pushModal(deskModal{kind: modalEvent, run: e.key, ev: idx[min(max(m.cur[viewEvents], 0), len(idx)-1)]})
	case viewQueue:
		m.footer = m.styles.Muted.Render("a reviews the item for approval")
	}
	return m, nil
}

// modalRun is the run a dialog shows, as the dashboard draws it (the replay's
// state while replay is on).
func (m deskModel) modalRun(md *deskModal) *deskRun {
	if e := m.sel(); e != nil && e.key == md.run {
		return e
	}
	return m.runs[md.run]
}

// stepEvents is the indexes of the step dialog's events.
func (m deskModel) stepEvents(md *deskModal) []int {
	e := m.modalRun(md)
	if e == nil {
		return nil
	}
	var out []int
	for i, ev := range e.events {
		if ev.Step == md.step {
			out = append(out, i)
		}
	}
	return out
}

func (m deskModel) openStepEvent(md *deskModal) deskModel {
	evs := m.stepEvents(md)
	if len(evs) == 0 {
		return m
	}
	run, idx := md.run, evs[min(max(md.cur, 0), len(evs)-1)]
	m.pushModal(deskModal{kind: modalEvent, run: run, ev: idx})
	return m
}

// stepLines is the step dialog: the step's state, attempts, rejects and
// duration, then its events, one row each. It is rebuilt on every update, so
// it follows a live run.
func (m deskModel) stepLines(md *deskModal, w int) ([]string, []int) {
	st := m.styles
	e := m.modalRun(md)
	var s *runview.StepState
	if e != nil {
		for i := range e.state.Steps {
			if e.state.Steps[i].ID == md.step {
				s = &e.state.Steps[i]
			}
		}
	}
	if s == nil {
		return []string{st.Muted.Render("this step is no longer in the run")}, nil
	}
	now := m.now()
	g, word, sty := m.stepLook(e, *s)
	var out []string
	add := func(t string) { out = append(out, wrapText(t, w)...) }
	add(sty.Render(g+" "+word) + "  " + st.Header.Render(capSafe(s.ID, 96)))
	if s.Note != "" {
		add(st.Muted.Render(capSafe(s.Note, 400)))
	}
	rej := fmt.Sprintf("%d", s.Rejects)
	if mx := e.spec.Gate.MaxRejects; mx > 0 {
		rej = fmt.Sprintf("%d/%d", s.Rejects, mx)
	}
	add(st.Muted.Render("attempt ") + st.Fg.Render(strconv.Itoa(s.Attempt)) + st.Muted.Render(" · rejects ") + st.Fg.Render(rej) +
		st.Muted.Render(" · duration ") + st.Fg.Render(ageShort(m.stepDur(e, *s, now))))
	if s.Resumed {
		add(st.Warn.Render(m.glyph.Resumed + " resumed after a restart"))
	}
	if !s.Start.IsZero() {
		t := st.Muted.Render("started ") + st.Fg.Render(s.Start.UTC().Format("15:04:05"))
		if !s.End.IsZero() {
			t += st.Muted.Render(" · ended ") + st.Fg.Render(s.End.UTC().Format("15:04:05"))
		}
		add(t)
	}
	evs := m.stepEvents(md)
	out = append(out, "", st.Header.Render(fmt.Sprintf("events (%d)", len(evs))))
	if len(evs) == 0 {
		return append(out, st.Muted.Render("none yet")), nil
	}
	rowLine := make([]int, len(evs))
	cur := min(max(md.cur, 0), len(evs)-1)
	for r, i := range evs {
		ev := e.events[i]
		marker := " "
		if r == cur {
			marker = ">"
		}
		when := "#" + strconv.Itoa(ev.Seq)
		if !ev.Time.IsZero() {
			when = ev.Time.UTC().Format("15:04:05")
		}
		tag := eventTag(e.spec, ev, m.glyph.Pending)
		rowLine[r] = len(out)
		out = append(out, ansi.Truncate(marker+st.Muted.Render(padTo(when, 8))+" "+m.tagStyle(tag).Render(padTo(tag, 5))+" "+
			st.Fg.Render(capSafe(ev.Name, 48))+" "+st.Muted.Render(eventFields(e.spec, ev)), w, "…"))
	}
	return out, rowLine
}

// eventLines is the event dialog: every field, wrapped.
func (m deskModel) eventLines(md *deskModal, w int) []string {
	st := m.styles
	e := m.modalRun(md)
	if e == nil || md.ev < 0 || md.ev >= len(e.events) {
		return []string{st.Muted.Render("this event is not in the run as shown")}
	}
	ev := e.events[md.ev]
	var out []string
	add := func(t string) { out = append(out, wrapText(t, w)...) }
	add(st.Muted.Render("event ") + st.Header.Render(capSafe(ev.Name, 256)))
	if ev.Step != "" {
		add(st.Muted.Render("step ") + st.Fg.Render(capSafe(ev.Step, 256)))
	}
	when := st.Muted.Render("seq ") + st.Fg.Render(strconv.Itoa(ev.Seq))
	if !ev.Time.IsZero() {
		when += st.Muted.Render(" · time ") + st.Fg.Render(ev.Time.UTC().Format(time.RFC3339))
	}
	add(when)
	out = append(out, "")
	for _, f := range ev.Fields {
		add(st.Accent.Render(capSafe(f.Key, 96)+":") + " " + st.Fg.Render(capSafe(f.Value, 1024)))
	}
	if len(ev.Fields) == 0 {
		add(st.Muted.Render("no fields"))
	}
	return out
}

// --- the run switcher ---

type switchItem struct {
	key        deskRunKey
	text       string // the row, styled
	filterText string
}

func (i switchItem) FilterValue() string { return i.filterText }

type switchDelegate struct{}

func (d switchDelegate) Height() int                         { return 1 }
func (d switchDelegate) Spacing() int                        { return 0 }
func (d switchDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d switchDelegate) Render(w io.Writer, l list.Model, index int, item list.Item) {
	si, ok := item.(switchItem)
	if !ok {
		return
	}
	marker := "  "
	if index == l.Index() {
		marker = "> "
	}
	// w is the list's in-memory buffer; Render has no error to return.
	_, _ = fmt.Fprint(w, ansi.Truncate(marker+si.text, l.Width(), "…"))
}

// openSwitcher opens the run switcher: every run, in list order, filterable.
func (m deskModel) openSwitcher() deskModel {
	now := m.now()
	st := m.styles
	items := make([]list.Item, 0, len(m.order))
	sel := 0
	for i, k := range m.order {
		e := m.runs[k]
		g, word, sty := m.runLook(e, now)
		name := capSafe(k.name, 96)
		row := sty.Render(g+" "+padTo(word, 8)) + " " + st.Fg.Render(name) + "  " + st.Muted.Render(m.runAge(e, now)+"  "+capSafe(k.source, 32))
		items = append(items, switchItem{key: k, text: row, filterText: name + " " + word + " " + capSafe(k.source, 32)})
		if k == m.selected {
			sel = i
		}
	}
	l := list.New(items, switchDelegate{}, 0, 0)
	ls := m.theme.List()
	ls.TitleBar = lipgloss.NewStyle()
	l.Styles = ls
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	// A steady cursor: a blinking one re-arms a timer for as long as the
	// filter is open.
	fs := l.FilterInput.Styles()
	fs.Cursor.Blink = false
	fs.Focused.Prompt, fs.Focused.Text = st.Accent, st.Fg
	fs.Blurred.Prompt, fs.Blurred.Text = st.Muted, st.Fg
	l.FilterInput.SetStyles(fs)
	l.Select(sel)
	m.pushModal(deskModal{kind: modalSwitcher, list: l})
	m.syncModal()
	return m
}

func (m deskModel) onSwitcherKey(md *deskModal, k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if md.list.FilterState() == list.Filtering {
		var cmd tea.Cmd
		md.list, cmd = md.list.Update(k)
		return m, cmd
	}
	switch {
	case binding.Matches(k, m.keys.Open):
		return m.pickSwitch(md)
	case binding.Matches(k, m.keys.Back):
		if md.list.FilterState() != list.Unfiltered {
			md.list.ResetFilter()
			return m, nil
		}
		m.popModal()
		return m, nil
	case binding.Matches(k, m.keys.Quit):
		m.popModal()
		return m, nil
	}
	var cmd tea.Cmd
	md.list, cmd = md.list.Update(k)
	return m, cmd
}

// pickSwitch selects the switcher's run and closes it.
func (m deskModel) pickSwitch(md *deskModal) (tea.Model, tea.Cmd) {
	si, ok := md.list.SelectedItem().(switchItem)
	m.popModal()
	if !ok || m.runs[si.key] == nil {
		return m, nil
	}
	if si.key != m.selected || !m.hasSel {
		m.selectKey(si.key)
	}
	return m, m.loadCmd(m.selLive())
}

// --- mouse ---

// onModalClick: a click selects, a second click on the same row opens, and a
// click outside closes a read dialog. Nothing a click does approves, and a
// click outside leaves the approve dialog open.
func (m deskModel) onModalClick(ms tea.Mouse) (tea.Model, tea.Cmd) {
	md := m.topModal()
	x, y, w, h := m.modalRect()
	inside := ms.X >= x && ms.X < x+w && ms.Y >= y && ms.Y < y+h
	if ms.Button != tea.MouseLeft {
		return m, nil
	}
	if !inside {
		if md.kind != modalConfirm {
			m.popModal()
		}
		return m, nil
	}
	line := ms.Y - y - 1 // below the top border
	switch md.kind {
	case modalStep:
		bw, _ := m.modalBodySize(md)
		_, rowLine := m.stepLines(md, bw)
		at := line + md.vp.YOffset()
		for r, l := range rowLine {
			if l != at {
				continue
			}
			if r == md.cur {
				return m.openStepEvent(md), nil
			}
			md.cur = r
		}
	case modalSwitcher:
		// The filter bar is the list's first line; rows follow it.
		p := md.list.Paginator
		i := p.Page*p.PerPage + line - 1
		if line < 1 || i < 0 || i >= len(md.list.VisibleItems()) {
			return m, nil
		}
		if i == md.list.Index() {
			return m.pickSwitch(md)
		}
		md.list.Select(i)
	}
	return m, nil
}

func (m deskModel) onModalWheel(ms tea.Mouse) deskModel {
	md := m.topModal()
	up := ms.Button == tea.MouseWheelUp
	if ms.Button != tea.MouseWheelUp && ms.Button != tea.MouseWheelDown {
		return m
	}
	if md.kind == modalSwitcher {
		if up {
			md.list.CursorUp()
		} else {
			md.list.CursorDown()
		}
		return m
	}
	if up {
		md.vp.ScrollUp(3)
	} else {
		md.vp.ScrollDown(3)
	}
	return m
}

// --- drawing ---

// overlayModal draws the top dialog over the screen's lines.
func (m deskModel) overlayModal(lines []string) []string {
	md := m.topModal()
	if md == nil {
		return lines
	}
	x, y, w, h := m.modalRect()
	box := strings.Split(m.modalView(md, w, h), "\n")
	for i, b := range box {
		row := y + i
		if row >= len(lines) {
			break
		}
		bg := lines[row]
		left := ansi.Truncate(bg, x, "")
		left += strings.Repeat(" ", max(x-ansi.StringWidth(left), 0))
		lines[row] = left + b + ansi.TruncateLeft(bg, x+w, "")
	}
	return lines
}

func (m deskModel) modalView(md *deskModal, w, h int) string {
	st := m.styles
	bw, bh := m.modalBodySize(md)
	var body []string
	title, strip := "", ""
	switch md.kind {
	case modalSwitcher:
		body = strings.Split(md.list.View(), "\n")
		title, strip = "switch run", strconv.Itoa(len(md.list.VisibleItems()))
	default:
		body = strings.Split(md.vp.View(), "\n")
	}
	for len(body) < bh {
		body = append(body, "")
	}
	body = body[:bh]
	var foot []string
	switch md.kind {
	case modalConfirm:
		title = m.confirmTitle(md)
		hint := ""
		if more := md.vp.TotalLineCount() - md.vp.YOffset() - bh; more > 0 {
			hint = st.Warn.Render(fmt.Sprintf("%d more lines ↓", more))
		} else {
			hint = st.Muted.Render("end of item")
		}
		if md.note != "" {
			hint += st.Muted.Render(" · ") + st.Warn.Render(md.note)
		}
		foot = []string{hint, st.Accent.Render("y") + st.Muted.Render(" approve · ") + st.Accent.Render("n/esc") + st.Muted.Render(" cancel")}
	case modalStep:
		title = "step " + capSafe(md.step, 48)
		strip = capSafe(md.run.name, 32)
		foot = []string{st.Muted.Render("enter opens an event · esc closes")}
	case modalEvent:
		title = "event"
		strip = capSafe(md.run.name, 32)
		foot = []string{st.Muted.Render("esc closes")}
	case modalSwitcher:
		foot = []string{st.Muted.Render("enter picks · / filters · esc closes")}
	}
	for i := range foot {
		foot[i] = ansi.Truncate(foot[i], bw, "…")
	}
	return Panel(st, w, title, strip, append(body, foot...))
}

// --- replay ---

// toggleReplay starts a replay of the selected run at its first event, or
// ends the one that is on.
func (m deskModel) toggleReplay() deskModel {
	if m.replay != nil {
		m.replay = nil
		return m
	}
	e := m.selLive()
	if e == nil || e.folder == nil || e.folder.Len() == 0 {
		m.footer = m.styles.Muted.Render("nothing to replay: the run has no events")
		return m
	}
	m.replay = &deskReplay{key: e.key, n: e.folder.Len()}
	m.syncReplay()
	return m
}

// onReplayKey handles the replay keys; ok is false for any other key.
func (m deskModel) onReplayKey(k tea.KeyPressMsg) (deskModel, tea.Cmd, bool) {
	r := *m.replay
	switch {
	case binding.Matches(k, m.keys.StepBack):
		r.i, r.playing = max(r.i-1, 0), false
	case binding.Matches(k, m.keys.StepFw):
		r.i, r.playing = min(r.i+1, r.n), false
	case binding.Matches(k, m.keys.Top):
		r.i, r.playing = 0, false
	case binding.Matches(k, m.keys.Bottom):
		r.i, r.playing = r.n, false
	case binding.Matches(k, m.keys.Autoplay):
		r.playing = !r.playing
		if r.playing && r.i >= r.n {
			r.i = 0
		}
		r.gen++
		m.replay = &r
		m.syncReplay()
		if r.playing {
			return m, m.replayTick(r.gen), true
		}
		return m, nil, true
	case binding.Matches(k, m.keys.Back), binding.Matches(k, m.keys.Replay):
		m.replay = nil
		return m, nil, true
	default:
		return m, nil, false
	}
	r.gen++
	m.replay = &r
	m.syncReplay()
	return m, nil, true
}

func (m deskModel) onReplayTick(msg deskReplayTickMsg) (tea.Model, tea.Cmd) {
	if m.replay == nil || !m.replay.playing || msg.gen != m.replay.gen {
		return m, nil
	}
	r := *m.replay
	r.i = min(r.i+1, r.n)
	if r.i >= r.n {
		r.playing = false
	}
	m.replay = &r
	m.syncReplay()
	if r.playing {
		return m, m.replayTick(r.gen)
	}
	return m, nil
}

// syncReplay rebuilds the replayed run's view: the state after its first i
// events, as Folder.At serves it.
func (m *deskModel) syncReplay() {
	if m.replay == nil {
		return
	}
	e := m.runs[m.replay.key]
	if e == nil || m.replay.key != m.selected {
		m.replay = nil
		return
	}
	r := *m.replay
	r.i = min(max(r.i, 0), r.n)
	v := *e
	v.events = e.events[:min(r.i, len(e.events))]
	v.state = e.folder.At(r.i, m.now())
	v.live = ""
	r.view = &v
	m.replay = &r
}

// replayChip is the header's replay mark.
func (m deskModel) replayChip() string {
	r := m.replay
	if r == nil {
		return ""
	}
	order := "by event"
	if e := m.runs[r.key]; e != nil && slices.ContainsFunc(e.events, func(ev runview.Event) bool { return !ev.Time.IsZero() }) {
		order = "by time"
	}
	newer := 0
	if e := m.runs[r.key]; e != nil {
		newer = max(e.folder.Len()-r.n, 0)
	}
	chip := fmt.Sprintf("REPLAY %d/%d · +%d new · %s", r.i, r.n, newer, order)
	if r.playing {
		chip = m.glyph.Running + " " + chip
	}
	return chip
}
