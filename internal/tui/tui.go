package tui

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// ActionKind is the deferred jump the TUI selected. Jumps that need the tty
// (attach/sesh connect) can't run while Bubble Tea owns the terminal, so the
// TUI records the intent and quits; the caller performs it afterward. Mutations
// (kill/rename) happen in-TUI and need no deferral.
type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionAttach
	ActionPick
	ActionLast
)

// Action is what Run returns for the caller to execute post-teardown.
type Action struct {
	Kind   ActionKind
	Target string
}

type mode int

const (
	menuMode mode = iota
	pickMode
	sessionsMode
	windowsMode
	treeMode
	cheatMode
	formMode
)

type opKind int

const (
	opNone opKind = iota
	opKill
	opKillOthers
	opRename
)

type model struct {
	ctx     context.Context
	client  *tmux.Client
	glyph   glyphSet
	noIcons bool

	width, height int
	mode          mode
	title         string

	l    list.Model
	tree viewport.Model

	form          *huh.Form
	pendingOp     opKind
	pendingTarget string

	// status is a transient one-line result of the last mutation (kill/rename),
	// shown in the footer — green on success, red on failure. Cleared when the
	// user starts the next action.
	status string

	action Action
}

// Run drives the TUI and returns the deferred Action (if any). The caller
// executes Action after Run returns, when the terminal is free again.
func Run(ctx context.Context, client *tmux.Client, noIcons bool) (Action, error) {
	m := newModel(ctx, client, noIcons)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return Action{}, err
	}
	if fm, ok := final.(model); ok {
		return fm.action, nil
	}
	return Action{}, nil
}

func newModel(ctx context.Context, client *tmux.Client, noIcons bool) model {
	g := pickGlyphs(noIcons)
	l := list.New(nil, itemDelegate{g: g}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetFilteringEnabled(true)

	m := model{
		ctx:     ctx,
		client:  client,
		glyph:   g,
		noIcons: noIcons,
		l:       l,
		tree:    viewport.New(0, 0),
		mode:    menuMode,
		title:   "menu",
	}
	m.l.SetItems(m.menuItems())
	return m
}

func (m model) Init() tea.Cmd { return nil }

func (m model) menuItems() []list.Item {
	return []list.Item{
		menuItem{"Pick", "connect or smart-create (sesh)", func(g glyphSet) string { return g.Pick }},
		menuItem{"Sessions", "attach · rename · kill", func(g glyphSet) string { return g.Session }},
		menuItem{"Windows", "jump to any window, any session", func(g glyphSet) string { return g.Window }},
		menuItem{"Tree", "the whole layout at a glance", func(g glyphSet) string { return g.Tree }},
		menuItem{"Last", "back to the last session", func(g glyphSet) string { return g.Last }},
		menuItem{"Cheatsheet", "tmux terms + the keys that matter", func(g glyphSet) string { return g.Cheat }},
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.KeyMsg:
		if t.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		m.applySize()
	}

	switch m.mode {
	case formMode:
		return m.updateForm(msg)
	case treeMode, cheatMode:
		return m.updateTree(msg)
	default:
		return m.updateList(msg)
	}
}

func (m *model) applySize() {
	narrow := m.width < 60
	m.l.SetDelegate(itemDelegate{g: m.glyph, narrow: narrow})
	body := m.height - 4
	if body < 3 {
		body = 3
	}
	m.l.SetSize(m.width, body)
	m.tree.Width = m.width
	m.tree.Height = body
}

func (m model) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.l, cmd = m.l.Update(msg)
		return m, cmd
	}

	// While filtering, the list owns every key.
	if m.l.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.l, cmd = m.l.Update(msg)
		return m, cmd
	}

	key := km.String()
	switch key {
	case "q", "esc":
		if m.mode == menuMode {
			return m, tea.Quit
		}
		m.toMenu()
		return m, nil
	case "enter":
		return m.activate(m.l.Index())
	}

	// Number-key select (thumb mode) — jump straight to that row and act.
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		if idx := int(key[0] - '1'); idx < len(m.l.Items()) {
			m.l.Select(idx)
			return m.activate(idx)
		}
	}

	// Session-screen action keys.
	if m.mode == sessionsMode {
		if it, ok := m.l.SelectedItem().(sessionItem); ok {
			switch key {
			case "k":
				return m.startConfirm(opKill, it.s.Name)
			case "K":
				return m.startConfirm(opKillOthers, it.s.Name)
			case "r":
				return m.startRename(it.s.Name)
			}
		}
	}

	var cmd tea.Cmd
	m.l, cmd = m.l.Update(msg)
	return m, cmd
}

// activate handles enter / number select per screen.
func (m model) activate(index int) (tea.Model, tea.Cmd) {
	switch m.mode {
	case menuMode:
		switch index {
		case 0:
			m.enterPick()
		case 1:
			m.enterSessions()
		case 2:
			m.enterWindows()
		case 3:
			m.enterTree()
		case 4:
			m.action = Action{Kind: ActionLast}
			return m, tea.Quit
		case 5:
			m.enterCheat()
		}
		return m, nil
	case pickMode:
		if it, ok := m.l.SelectedItem().(pickItem); ok {
			m.action = Action{Kind: ActionPick, Target: string(it)}
			return m, tea.Quit
		}
	case sessionsMode:
		if it, ok := m.l.SelectedItem().(sessionItem); ok {
			m.action = Action{Kind: ActionAttach, Target: it.s.Name}
			return m, tea.Quit
		}
	case windowsMode:
		if it, ok := m.l.SelectedItem().(windowItem); ok {
			m.action = Action{Kind: ActionAttach, Target: it.w.Target}
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) updateTree(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "q", "esc", "backspace":
			m.toMenu()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.tree, cmd = m.tree.Update(msg)
	return m, cmd
}

func (m model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	fm, cmd := m.form.Update(msg)
	if f, ok := fm.(*huh.Form); ok {
		m.form = f
	}
	switch m.form.State {
	case huh.StateCompleted:
		m.applyPending()
		m.form = nil
		m.enterSessions()
		return m, nil
	case huh.StateAborted:
		m.form = nil
		m.enterSessions()
		return m, nil
	}
	return m, cmd
}

// --- screen transitions (synchronous loads; tmux calls are local + fast) ---

func (m *model) toMenu() {
	m.status = ""
	m.title = "menu"
	m.setList(m.menuItems())
	m.mode = menuMode
}

func (m *model) enterPick() {
	names, err := m.client.SeshList(m.ctx)
	if err != nil {
		slog.Error("Failed to load sesh sessions.", "error", err)
		m.status = styleDanger.Render("✗ sesh: " + err.Error())
	}
	items := make([]list.Item, 0, len(names))
	for _, n := range names {
		items = append(items, pickItem(n))
	}
	m.title = "pick"
	m.setList(items)
	m.mode = pickMode
}

func (m *model) enterSessions() {
	sessions, err := m.client.ListSessions(m.ctx)
	if err != nil {
		slog.Error("Failed to load sessions.", "error", err)
		m.status = styleDanger.Render("✗ tmux: " + err.Error())
	}
	items := make([]list.Item, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, sessionItem{s})
	}
	m.title = "sessions"
	m.setList(items)
	m.mode = sessionsMode
}

func (m *model) enterWindows() {
	windows, err := m.client.ListWindows(m.ctx)
	if err != nil {
		slog.Error("Failed to load windows.", "error", err)
		m.status = styleDanger.Render("✗ tmux: " + err.Error())
	}
	items := make([]list.Item, 0, len(windows))
	for _, w := range windows {
		items = append(items, windowItem{w})
	}
	m.title = "windows"
	m.setList(items)
	m.mode = windowsMode
}

func (m *model) enterTree() {
	out, err := m.client.Tree(m.ctx, !m.noIcons)
	if err != nil {
		slog.Error("Failed to load tree.", "error", err)
		m.status = styleDanger.Render("✗ tmux: " + err.Error())
	}
	m.tree.SetContent(out)
	m.tree.GotoTop()
	m.title = "tree"
	m.mode = treeMode
}

func (m *model) enterCheat() {
	m.tree.SetContent(Cheatsheet(m.noIcons))
	m.tree.GotoTop()
	m.title = "cheatsheet"
	m.mode = cheatMode
}

func (m *model) setList(items []list.Item) {
	m.l.ResetFilter()
	m.l.SetItems(items)
	m.l.Select(0)
}

func (m model) startConfirm(op opKind, target string) (tea.Model, tea.Cmd) {
	m.status = ""
	prompt := fmt.Sprintf("Kill session %q?", target)
	if op == opKillOthers {
		prompt = fmt.Sprintf("Kill ALL sessions except %q?", target)
	}
	m.pendingOp = op
	m.pendingTarget = target
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Key("ok").Title(prompt).Affirmative("Yes").Negative("No"),
	)).WithWidth(m.formWidth()).WithShowHelp(false)
	m.mode = formMode
	return m, m.form.Init()
}

func (m model) startRename(target string) (tea.Model, tea.Cmd) {
	m.status = ""
	m.pendingOp = opRename
	m.pendingTarget = target
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("name").Title(fmt.Sprintf("Rename %q to:", target)),
	)).WithWidth(m.formWidth()).WithShowHelp(false)
	m.mode = formMode
	return m, m.form.Init()
}

func (m *model) applyPending() {
	switch m.pendingOp {
	case opKill:
		if m.form.GetBool("ok") {
			m.setStatus(m.client.KillSession(m.ctx, m.pendingTarget), "killed "+m.pendingTarget)
		}
	case opKillOthers:
		if m.form.GetBool("ok") {
			m.setStatus(m.client.KillOthers(m.ctx, m.pendingTarget), "kept only "+m.pendingTarget)
		}
	case opRename:
		if name := m.form.GetString("name"); name != "" {
			m.setStatus(m.client.RenameSession(m.ctx, m.pendingTarget, name), "renamed "+m.pendingTarget+" → "+name)
		}
	}
	m.pendingOp = opNone
}

// setStatus records a transient footer message: the error (red) if non-nil,
// otherwise the success text (green).
func (m *model) setStatus(err error, ok string) {
	if err != nil {
		m.status = styleDanger.Render("✗ " + err.Error())
		return
	}
	m.status = styleOK.Render("✓ " + ok)
}

func (m model) formWidth() int {
	w := m.width - 4
	if w > 50 {
		w = 50
	}
	if w < 20 {
		w = 20
	}
	return w
}

func (m model) View() string {
	header := m.headerView()
	var body string
	switch m.mode {
	case treeMode, cheatMode:
		body = m.tree.View()
	case formMode:
		body = m.form.View()
	default:
		body = m.l.View()
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, m.footerView())
}

func (m model) headerView() string {
	brand := styleHeader.Render(m.glyph.Forge + "  " + meta.AppName)
	return brand + styleMuted.Render("  ·  "+m.title)
}

func (m model) footerView() string {
	narrow := m.width < 60
	var hint string
	switch m.mode {
	case menuMode:
		hint = "1-6 / enter select · q/esc quit"
	case sessionsMode:
		if narrow {
			hint = "↑↓ · enter attach · k kill · r rename · q/esc back"
		} else {
			hint = "↑↓ move · 1-9 jump · enter attach · k kill · K kill-others · r rename · / filter · q/esc back"
		}
	case pickMode, windowsMode:
		hint = "↑↓ · 1-9 · enter select · / filter · q/esc back"
	case treeMode, cheatMode:
		hint = "↑↓ scroll · q/esc back"
	case formMode:
		hint = "enter confirm · esc cancel"
	}
	if m.status != "" {
		return m.status + "\n" + styleMuted.Render(hint)
	}
	return styleMuted.Render(hint)
}
