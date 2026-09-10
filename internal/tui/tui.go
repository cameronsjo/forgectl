package tui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// errStatus renders a footer error. Every error surfaced here can carry text
// forgectl never composed — a tmux session or window name, a sesh candidate, an
// exec diagnostic quoting one — so it goes through termsafe.SafeLine before any
// styling. Escape sequences in a name would otherwise repaint the TUI's chrome.
func errStatus(prefix string, err error, s theme.Styles) string {
	return s.Danger.Render(termsafe.SafeLine("✗ " + prefix + err.Error()))
}

// ActionKind is the deferred jump the TUI selected. Jumps that need the tty
// (attach/sesh connect) can't run while Bubble Tea owns the terminal, so the
// TUI records the intent and quits; the caller performs it afterward. Mutations
// (kill/rename) happen in-TUI and need no deferral.
type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionAttachSession
	ActionAttachWindow
	ActionPick
	ActionLast
	// ActionRunVerb is a hub-selected command with a complete argv (no
	// missing positional). The caller re-enters the same argv dispatch
	// pipeline a typed invocation takes (execute.go's runAction), printing
	// "$ forgectl <argv>" to stderr first.
	ActionRunVerb
	// ActionShowInvocation is a hub-selected leaf that NEEDS an argument the
	// hub cannot supply (e.g. pr's <ref>). The caller prints
	// "$ forgectl <argv>" — with the placeholder still in it — to stderr and
	// runs nothing, so the operator has the line to complete by hand.
	ActionShowInvocation
)

// Action is what Run returns for the caller to execute post-teardown.
//
// Attach carries a typed, generation-qualified identity rather than a target
// string. That matters here more than anywhere else in the program: an Action
// crosses the whole Bubble Tea teardown before it is dispatched, which is
// unbounded time for the server to restart or the object to move. The
// identity's generation is what lets the dispatch prove the object it is about
// to act on is still the one the operator selected.
//
// Pick is the exception, and deliberately so: it is a sesh candidate name
// handed to `sesh connect`, not a tmux target, so there is no tmux object to
// qualify.
//
// Argv is ActionRunVerb/ActionShowInvocation's payload: registry-derived
// command/subcommand identifiers (ActionRunVerb) or a Use-line placeholder
// text split on whitespace (ActionShowInvocation) — either way tokens this
// program composed, never user input, so nothing here needs shell quoting.
type Action struct {
	Kind    ActionKind
	Pick    string
	Session tmux.SessionIdentity
	Window  tmux.WindowIdentity
	Argv    []string
}

// HubEntry is one row of the hub's top screen, or one row of the flattened
// "all commands" screen the hub's extension-tier aggregate row opens into.
// Leaves is empty for a module with no runnable subverbs (e.g. doctor) —
// selecting such a row runs it directly instead of drilling in.
type HubEntry struct {
	Name   string
	Short  string
	Core   bool
	Leaves []HubLeaf
}

// HubLeaf is one runnable verb inside a HubEntry's drill-down list. NeedsArgs
// mirrors internal/cli's parentTakesArg predicate: true when Use carries a
// placeholder ("pr <ref>") the hub cannot fill in, in which case selecting
// the leaf shows the invocation rather than running it.
type HubLeaf struct {
	Name      string
	Short     string
	Use       string
	NeedsArgs bool
}

// hubAllCommandsPrefix identifies the synthetic aggregate HubEntry buildHub
// appends for the extension tier. Its Leaves are flattened one level deeper
// than a normal entry's — each Name is already a complete argv joined by
// spaces ("docker prune", or "doctor" for a leafless module) — so leaf
// selection there builds Argv by splitting the leaf's own Name instead of
// pairing it with the enclosing entry's Name.
const hubAllCommandsPrefix = "all commands"

func isAllCommandsEntry(name string) bool {
	return strings.HasPrefix(name, hubAllCommandsPrefix)
}

// RunOptions configures Run. Hub is the full ordered row set buildHub
// produced (cli.buildHub); StartInTmux skips the hub and opens directly in
// the tmux jumper (menuMode) — bare `forgectl tmux`'s behavior — with the hub
// still one esc away.
type RunOptions struct {
	Hub         []HubEntry
	StartInTmux bool
	NoIcons     bool
	Theme       theme.Theme
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
	hubMode
	leavesMode
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

	// theme is the resolved palette; styles is its Styles() cached on the
	// model so a hot render path (item delegate, footer) never recomputes it
	// per row. Both are rebuilt together on a tea.BackgroundColorMsg.
	theme  theme.Theme
	styles theme.Styles

	width, height int
	mode          mode
	title         string

	l    list.Model
	tree viewport.Model

	form      *huh.Form
	pendingOp opKind
	// pendingSession is the identity the confirmed operation acts on. The
	// confirmation prompt renders pendingSession.Name; the command targets its
	// native id, revalidated at dispatch. Holding a name here instead is how a
	// "yes" to "Kill session X?" could land on a different X.
	pendingSession tmux.SessionIdentity

	// status is a transient one-line result of the last mutation (kill/rename),
	// shown in the footer — green on success, red on failure. Cleared when the
	// user starts the next action.
	status string

	action Action

	// hub is the full ordered row set from RunOptions.Hub — hubMode's list.
	hub []HubEntry
	// leaves is the drill-down list currently shown in leavesMode, and
	// leavesParent is the enclosing HubEntry's Name — leafArgv/usageLine need
	// it to build the right Argv (or detect the flattened "all commands"
	// screen, where a leaf's own Name is already the full argv).
	leaves       []HubLeaf
	leavesParent string
}

// Run drives the TUI and returns the deferred Action (if any). The caller
// executes Action after Run returns, when the terminal is free again.
func Run(ctx context.Context, client *tmux.Client, opts RunOptions) (Action, error) {
	m := newModel(ctx, client, opts)
	// Bubble Tea v2 dropped WithAltScreen: the alt screen is a property of the
	// View the model returns each frame, not a program-construction option.
	// View() sets AltScreen instead.
	p := tea.NewProgram(m, tea.WithContext(ctx))
	final, err := p.Run()
	if err != nil {
		return Action{}, err
	}
	if fm, ok := final.(model); ok {
		return fm.action, nil
	}
	return Action{}, nil
}

func newModel(ctx context.Context, client *tmux.Client, opts RunOptions) model {
	g := pickGlyphs(opts.NoIcons)
	st := opts.Theme.Styles()
	l := list.New(nil, itemDelegate{g: g, styles: st}, 0, 0)
	l.Styles = opts.Theme.List()
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetFilteringEnabled(true)

	m := model{
		ctx:     ctx,
		client:  client,
		glyph:   g,
		noIcons: opts.NoIcons,
		theme:   opts.Theme,
		styles:  st,
		l:       l,
		tree:    viewport.New(),
		mode:    hubMode,
		title:   "hub",
		hub:     opts.Hub,
	}
	if opts.StartInTmux {
		m.title = "menu"
		m.mode = menuMode
		m.l.SetItems(m.menuItems())
		return m
	}
	m.setList(hubItems(m.hub))
	return m
}

// hubItems renders entries as list rows.
func hubItems(entries []HubEntry) []list.Item {
	items := make([]list.Item, 0, len(entries))
	for _, e := range entries {
		items = append(items, hubItem{entry: e})
	}
	return items
}

// hubLeafItems renders leaves as list rows.
func hubLeafItems(leaves []HubLeaf) []list.Item {
	items := make([]list.Item, 0, len(leaves))
	for _, l := range leaves {
		items = append(items, leafItem{leaf: l})
	}
	return items
}

// Init requests the terminal's background colour only where probing it is
// safe (theme.ShouldProbe): both stdin and stdout must be a real TTY, and
// TERM must not be a multiplexer that swallows the OSC 11 response. Elsewhere
// the theme stays pinned to whatever th.Mode() already resolved (dark by
// default), and no probe command goes out at all.
func (m model) Init() tea.Cmd {
	env := theme.Env{
		StdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		StdoutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		Term:      os.Getenv("TERM"),
		NoColor:   os.Getenv("NO_COLOR") != "",
	}
	if theme.ShouldProbe(m.theme.Mode(), env) {
		return tea.RequestBackgroundColor
	}
	return nil
}

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
	// tea.KeyPressMsg, never tea.KeyMsg: in v2 KeyMsg is an interface that both
	// a press and a RELEASE satisfy. On a terminal speaking the Kitty keyboard
	// protocol every key would then be handled twice — one "q" would quit from
	// a subscreen and again from the menu.
	case tea.KeyPressMsg:
		if t.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = t.Width, t.Height
		m.applySize()
	case tea.BackgroundColorMsg:
		// Bubble Tea v2 delivers this once, early — the one reply to Init's
		// RequestBackgroundColor. Rebuild everything derived from the theme so
		// list chrome, rows, and (on the next form) huh forms all agree.
		//
		// Guarded on ModeAuto even though Init only asks under ModeAuto: an
		// answer nobody asked for must not silently override a configured
		// mode. Bubble Tea can deliver this unsolicited, and a terminal is
		// free to volunteer it — so the decision to accept lives with the
		// setting, not with whether a message happened to arrive.
		if m.theme.Mode() == theme.ModeAuto {
			m.theme = m.theme.WithDark(t.IsDark())
			m.styles = m.theme.Styles()
			m.l.Styles = m.theme.List()
			// The cheatsheet is the one screen whose content is already
			// rendered into a viewport and can be rebuilt from nothing but
			// styles, so it is the one that must be. Nothing orders this
			// message against a keypress: the terminal answers when it
			// answers, and a slow reply arriving after `6` would otherwise
			// leave dark-theme text on a light terminal until the user backed
			// out and reopened. treeMode's content came from tmux and cannot
			// be regenerated without re-running the query.
			if m.mode == cheatMode {
				m.tree.SetContent(Cheatsheet(m.noIcons, m.styles))
			}
			m.applySize()
		}
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
	m.l.SetDelegate(itemDelegate{g: m.glyph, narrow: narrow, styles: m.styles})
	body := m.height - 4
	if body < 3 {
		body = 3
	}
	m.l.SetSize(m.width, body)
	m.tree.SetWidth(m.width)
	m.tree.SetHeight(body)
}

func (m model) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyPressMsg)
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
		switch m.mode {
		case hubMode:
			return m, tea.Quit
		case menuMode, leavesMode:
			// The hub is the quit level: a bare invoke's tmux jumper and any
			// module's leaf list both back out to the hub, not straight to
			// the shell (Architecture: "q/esc in hubMode quits; in menuMode
			// returns to the hub").
			m.toHub()
			return m, nil
		default:
			m.toMenu()
			return m, nil
		}
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
				return m.startConfirm(opKill, m.client.SessionIdentity(it.s))
			case "K":
				return m.startConfirm(opKillOthers, m.client.SessionIdentity(it.s))
			case "r":
				return m.startRename(m.client.SessionIdentity(it.s))
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
	case hubMode:
		if index < 0 || index >= len(m.hub) {
			return m, nil
		}
		entry := m.hub[index]
		if entry.Name == "tmux" {
			// tmux's hub row opens today's unchanged tmux jumper rather than
			// a drill-down list (Architecture: "opens today's menuMode
			// screen unchanged in content").
			m.title = "menu"
			m.setList(m.menuItems())
			m.mode = menuMode
			return m, nil
		}
		if len(entry.Leaves) == 0 {
			m.action = Action{Kind: ActionRunVerb, Argv: []string{entry.Name}}
			return m, tea.Quit
		}
		m.enterLeaves(entry)
		return m, nil
	case leavesMode:
		if index < 0 || index >= len(m.leaves) {
			return m, nil
		}
		leaf := m.leaves[index]
		if leaf.NeedsArgs {
			m.action = Action{Kind: ActionShowInvocation, Argv: strings.Fields(usageLine(m.leavesParent, leaf))}
			return m, tea.Quit
		}
		m.action = Action{Kind: ActionRunVerb, Argv: leafArgv(m.leavesParent, leaf)}
		return m, tea.Quit
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
			m.action = Action{Kind: ActionPick, Pick: string(it)}
			return m, tea.Quit
		}
	case sessionsMode:
		if it, ok := m.l.SelectedItem().(sessionItem); ok {
			m.action = Action{Kind: ActionAttachSession, Session: m.client.SessionIdentity(it.s)}
			return m, tea.Quit
		}
	case windowsMode:
		if it, ok := m.l.SelectedItem().(windowItem); ok {
			m.action = Action{Kind: ActionAttachWindow, Window: m.client.WindowIdentity(it.w)}
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) updateTree(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyPressMsg); ok {
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

// toHub returns to the hub's top screen — the quit level every other screen
// (menuMode, leavesMode) backs out to on q/esc.
func (m *model) toHub() {
	m.status = ""
	m.title = "hub"
	m.setList(hubItems(m.hub))
	m.mode = hubMode
}

// enterLeaves opens entry's drill-down list.
func (m *model) enterLeaves(entry HubEntry) {
	m.status = ""
	m.title = entry.Name
	m.leaves = entry.Leaves
	m.leavesParent = entry.Name
	m.setList(hubLeafItems(entry.Leaves))
	m.mode = leavesMode
}

// leafArgv builds the argv to run leaf, given the HubEntry.Name it was
// listed under. Inside the "all commands" aggregate, a leaf's own Name is
// already a complete argv joined by spaces (buildHub flattened it there);
// everywhere else the module name and the leaf name are the two tokens.
func leafArgv(entryName string, leaf HubLeaf) []string {
	if isAllCommandsEntry(entryName) {
		return strings.Fields(leaf.Name)
	}
	if leaf.Name == entryName {
		// The synthetic bare-module leaf a NeedsArgs module contributes
		// (e.g. pr's own "pr <ref>") — running it means invoking the module
		// alone, not module+module.
		return []string{entryName}
	}
	return []string{entryName, leaf.Name}
}

// usageLine returns the placeholder-carrying invocation text for a NeedsArgs
// leaf — "pr <ref>", or "projects clone <query>" for a subcommand whose own
// Use line (cobra convention) omits the parent's name.
func usageLine(entryName string, leaf HubLeaf) string {
	mod := entryName
	if isAllCommandsEntry(entryName) {
		if fields := strings.Fields(leaf.Name); len(fields) > 0 {
			mod = fields[0]
		}
	}
	if leaf.Use == mod || strings.HasPrefix(leaf.Use, mod+" ") {
		return leaf.Use
	}
	return mod + " " + leaf.Use
}

func (m *model) enterPick() {
	names, err := m.client.SeshList(m.ctx)
	if err != nil {
		slog.Error("Failed to load sesh sessions.", "error", err)
		m.status = errStatus("sesh: ", err, m.styles)
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
		m.status = errStatus("tmux: ", err, m.styles)
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
		m.status = errStatus("tmux: ", err, m.styles)
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
		m.status = errStatus("tmux: ", err, m.styles)
	}
	m.tree.SetContent(out)
	m.tree.GotoTop()
	m.title = "tree"
	m.mode = treeMode
}

func (m *model) enterCheat() {
	m.tree.SetContent(Cheatsheet(m.noIcons, m.styles))
	m.tree.GotoTop()
	m.title = "cheatsheet"
	m.mode = cheatMode
}

func (m *model) setList(items []list.Item) {
	m.l.ResetFilter()
	m.l.SetItems(items)
	m.l.Select(0)
}

func (m model) startConfirm(op opKind, session tmux.SessionIdentity) (tea.Model, tea.Cmd) {
	m.status = ""
	prompt := fmt.Sprintf("Kill session %q?", session.Name)
	if op == opKillOthers {
		prompt = fmt.Sprintf("Kill ALL sessions except %q?", session.Name)
	}
	m.pendingOp = op
	m.pendingSession = session
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Key("ok").Title(prompt).Affirmative("Yes").Negative("No"),
	)).WithWidth(m.formWidth()).WithShowHelp(false).WithKeyMap(keymap.Cancel()).
		WithTheme(m.theme.Huh())
	m.mode = formMode
	return m, m.form.Init()
}

func (m model) startRename(session tmux.SessionIdentity) (tea.Model, tea.Cmd) {
	m.status = ""
	m.pendingOp = opRename
	m.pendingSession = session
	// %q is the text boundary here, not cosmetic quoting: session.Name comes
	// from tmux, and strconv-style quoting keeps terminal controls and bidi
	// overrides inert inside the huh prompt.
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Key("name").Title(fmt.Sprintf("Rename %q to:", session.Name)),
	)).WithWidth(m.formWidth()).WithShowHelp(false).WithKeyMap(keymap.Cancel()).
		WithTheme(m.theme.Huh())
	m.mode = formMode
	return m, m.form.Init()
}

// applyPending runs the confirmed mutation. Each client call revalidates
// m.pendingSession before issuing anything, so a session that was killed,
// renamed, or replaced by a server restart while the confirmation was on screen
// produces a refusal in the footer rather than a mutation somewhere else.
func (m *model) applyPending() {
	name := m.pendingSession.Name
	switch m.pendingOp {
	case opKill:
		if m.form.GetBool("ok") {
			m.setStatus(m.client.KillSession(m.ctx, m.pendingSession), "killed "+name)
		}
	case opKillOthers:
		if m.form.GetBool("ok") {
			m.setStatus(m.client.KillOthers(m.ctx, m.pendingSession), "kept only "+name)
		}
	case opRename:
		if newName := m.form.GetString("name"); newName != "" {
			m.setStatus(m.client.RenameSession(m.ctx, m.pendingSession, newName), "renamed "+name+" → "+newName)
		}
	}
	m.pendingOp = opNone
	m.pendingSession = tmux.SessionIdentity{}
}

// setStatus records a transient footer message: the error (red) if non-nil,
// otherwise the success text (green).
//
// The success text needs the same boundary as the error text, and for the same
// reason: every caller composes it around a session name ("killed <name>",
// "renamed <old> → <new>"), which is text forgectl never wrote. Neutralizing
// here rather than at the three call sites means a fourth mutation cannot
// reintroduce the gap.
func (m *model) setStatus(err error, ok string) {
	if err != nil {
		m.status = errStatus("", err, m.styles)
		return
	}
	m.status = m.styles.OK.Render(termsafe.SafeLine("✓ " + ok))
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

// View returns a tea.View rather than a string: Bubble Tea v2 made the alt
// screen a per-frame property of the view, which is where WithAltScreen went.
func (m model) View() tea.View {
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
	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, header, body, m.footerView()))
	v.AltScreen = true
	return v
}

func (m model) headerView() string {
	brand := m.styles.Brand.Render(m.glyph.Forge) + "  " + m.styles.Header.Render(meta.AppName)
	return brand + m.styles.Muted.Render("  ·  "+m.title)
}

func (m model) footerView() string {
	narrow := m.width < 60
	var hint string
	switch m.mode {
	case hubMode:
		hint = "↑↓ move · 1-9 jump · enter select · / filter · q/esc quit · forgectl --help lists every command"
	case menuMode:
		hint = "1-6 / enter select · q/esc back"
	case leavesMode:
		hint = "↑↓ · 1-9 · enter select · / filter · q/esc back"
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
		return m.status + "\n" + m.styles.Muted.Render(hint)
	}
	return m.styles.Muted.Render(hint)
}
