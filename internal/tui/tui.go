package tui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/paginator"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// errStatus renders a footer error. Every error surfaced here can carry text
// forgectl never composed — a tmux session or window name, a sesh candidate, an
// exec diagnostic quoting one — so it goes through termsafe.SafeLineMax
// (statusMaxRunes) before any styling. Escape sequences in a name would
// otherwise repaint the TUI's chrome.
func errStatus(prefix string, err error, s theme.Styles) string {
	return s.Danger.Render(termsafe.SafeLineMax("✗ "+prefix+err.Error(), statusMaxRunes))
}

// unreadableStatus is the footer note for a listing that could not read some
// of tmux's rows (forgectl#815), or "" when it read them all — so a screen
// with a silently missing session or window does not read as a smaller
// server. The note is forgectl's own text, but it goes through SafeLineMax like
// every other footer.
func unreadableStatus(u tmux.UnreadableRows, s theme.Styles) string {
	note := u.Note()
	if note == "" {
		return ""
	}
	return s.Warn.Render(termsafe.SafeLineMax("! "+note, statusMaxRunes))
}

// noteUnreadable adds unreadableStatus to the footer. It appends rather than
// replaces: a screen reload right after a kill or rename must keep that
// mutation's result visible beside the note.
func (m *model) noteUnreadable(u tmux.UnreadableRows) {
	note := unreadableStatus(u, m.styles)
	if note == "" {
		return
	}
	if m.status != "" {
		m.status += "  "
	}
	m.status += note
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
// command/subcommand identifiers, plus — for a picker choice — exactly one
// operator-typed or candidate argument that PickerArgv validated (ActionRunVerb),
// or a Use-line placeholder text split on whitespace (ActionShowInvocation).
// It is dispatched as argv, never through a shell; echo it with DisplayArgv,
// which quotes the operator's element rather than trusting it.
type Action struct {
	Kind    ActionKind
	Pick    string
	Session tmux.SessionIdentity
	Window  tmux.WindowIdentity
	Argv    []string
}

// HubEntry is one row of the hub's top screen. There are four shapes:
//
//   - a module row (Argv nil, Members nil): Leaves is its drill-down list, empty for a
//     module with no runnable subverbs (e.g. doctor), which runs directly. Use
//     is the module's own Use line; when it names exactly one required
//     argument (pr <ref>), enter opens the argument picker in place instead,
//     and the picker's last row drills into Leaves;
//   - a direct-command row (Argv set, the "recent" section): enter runs Argv,
//     or opens the picker when NeedsArgs;
//   - an area row (Members set): enter opens Members, a list of module rows
//     ("repos" holds status, review, branch, …);
//   - a Heading row: a section divider ("── recent ──") that the cursor skips
//     and enter ignores.
//
// Key is the row's jump key on the top screen, 1-9, or 0 for none. It is
// fixed when the hub is built, not taken from the row's position, so a key
// keeps its meaning as the recent rows above it change (forgectl#1074). A
// negative Key numbers the row by position instead, as every other list does;
// area lists and the search-all list use it.
type HubEntry struct {
	Name      string
	Short     string
	Core      bool
	Leaves    []HubLeaf
	Use       string
	Argv      []string
	NeedsArgs bool
	Heading   bool
	Key       int
	Members   []HubEntry
	// NoPicker keeps the argument picker off this row even when its Use
	// names one positional the picker could supply: the argument fills
	// another CLI's subcommand slot (internal/cli's hub-no-picker
	// annotation), so the row prints its invocation instead.
	NoPicker bool
}

// HubLeaf is one runnable verb inside a HubEntry's drill-down list. NeedsArgs
// mirrors internal/cli's parentTakesArg predicate: true when Use carries a
// placeholder ("pr <ref>") the hub cannot fill in, in which case selecting
// the leaf shows the invocation rather than running it.
//
// A leaf with Leaves is a nested group (pr findings): selecting it opens
// those leaves as the next drill-down level rather than running it (#916).
//
// Self marks the synthetic bare-command leaf a NeedsArgs module or group
// contributes for itself (pr's own "pr <ref>"): running it invokes the path
// alone. It is a flag, not a name match, so a real subcommand that happens to
// share its parent's name still runs as parent+child (#948).
type HubLeaf struct {
	Name      string
	Short     string
	Use       string
	NeedsArgs bool
	Self      bool
	Leaves    []HubLeaf
	// NoPicker is HubEntry.NoPicker for a leaf.
	NoPicker bool
}

// RunOptions configures Run. Hub is the full ordered row set buildHub
// produced (cli.buildHub); StartInTmux skips the hub and opens directly in
// the tmux jumper (menuMode) — bare `forgectl tmux`'s behavior — with the hub
// still one esc away.
//
// Header is the hub's status line and ArgSources feeds the argument picker,
// keyed by the space-joined argv before the argument ("projects clone").
// BuildArgv turns a picker choice into argv against the live command tree;
// nil falls back to the tree-blind PickerArgv.
type RunOptions struct {
	Hub         []HubEntry
	Header      HubHeader
	ArgSources  map[string]ArgSource
	BuildArgv   ArgvBuilder
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
	// areaMode lists one hub area's module rows (HubEntry.Members).
	areaMode
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

	// filterGen numbers the query as typed: it advances whenever a message
	// changes the filter text, and tagFilterRuns stamps each filter run
	// with it, so updateList can tell the newest result from an older one.
	filterGen int
	// hub is the full ordered row set from RunOptions.Hub — hubMode's list.
	hub []HubEntry
	// nameCol is the hub lists' name column (hubNameColumn), recomputed
	// when the list's items or size change.
	nameCol int
	// hubFlat is true while hubMode shows every command in one list, which
	// it does while a filter is open, so "/" searches every command and not
	// only the rows on the top screen.
	hubFlat bool
	// searchFrom is the top-screen row the search-all list was opened from,
	// where the cursor returns when the search is cancelled.
	searchFrom string
	// area is the open area in areaMode, and the area a leavesMode list was
	// entered from (esc returns there), or nil.
	area *HubEntry
	// leaves is the drill-down list currently shown in leavesMode, and
	// leavesPath is the argv that reaches it: the enclosing HubEntry's Name,
	// then each nested group opened below it (pr, findings) — leafArgv and
	// usageLine need it to build the right Argv. leavesUp holds the list each
	// opened group was chosen from, so esc climbs one level at a time.
	leaves     []HubLeaf
	leavesPath []string
	leavesUp   [][]HubLeaf

	// header is the hub's status line; argSources feeds picker candidates.
	header     HubHeader
	argSources map[string]ArgSource
	buildArgv  ArgvBuilder
	// picker is the open argument picker, or nil. While it is open it owns
	// the keyboard (updatePicker).
	picker *argPicker
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
	// Page numbers, not dots: the current dot differs only by color, which a
	// monochrome terminal or a screen reader does not show.
	l.Paginator.Type = paginator.Arabic
	l.SetFilteringEnabled(true)
	l.Filter = rankFilter

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

		header:     opts.Header,
		argSources: opts.ArgSources,
		buildArgv:  opts.BuildArgv,
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
		menuItem{menuPick, "Pick", "connect or smart-create (sesh)", func(g glyphSet) string { return g.Pick }},
		menuItem{menuSessions, "Sessions", "attach · rename · kill", func(g glyphSet) string { return g.Session }},
		menuItem{menuWindows, "Windows", "jump to any window, any session", func(g glyphSet) string { return g.Window }},
		menuItem{menuTree, "Tree", "the whole layout at a glance", func(g glyphSet) string { return g.Tree }},
		menuItem{menuLast, "Last", "back to the last session", func(g glyphSet) string { return g.Last }},
		menuItem{menuCheat, "Cheatsheet", "tmux terms + the keys that matter", func(g glyphSet) string { return g.Cheat }},
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

	if m.tooSmall() {
		switch t := msg.(type) {
		case tea.KeyPressMsg:
			if s := t.String(); s == "q" || s == "esc" {
				return m, tea.Quit
			}
			return m, nil
		case tea.PasteMsg:
			return m, nil
		}
	}

	if m.picker != nil {
		return m.updatePicker(msg)
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
	// The hub screens draw exactly a header, the list, the detail lines, and
	// a footer (plus a status line when one is set); the other screens keep
	// two lines of slack.
	chrome := 4
	if m.hubScreen() {
		chrome = 2
		if m.status != "" {
			chrome++
		}
	}
	body := m.height - chrome - m.extraChromeLines()
	if body < 3 {
		body = 3
	}
	// bubbles sizes a page by reserving the pager's height as it is now: two
	// lines when the list is already paged, one when it is not. A list first
	// sized while paged (every list is, at height 0) then stays paged at a
	// height that fits it on one page. Reset to one page, size, and size
	// again so the second pass reserves what the first pass decided.
	m.l.Paginator.SetTotalPages(1)
	m.l.SetSize(m.width, body)
	m.l.SetSize(m.width, body)
	m.tree.SetWidth(m.width)
	m.tree.SetHeight(body)
	m.fitTopScreen()
	m.refreshDelegate()
}

// fitTopScreen keeps the top screen's areas on screen when the terminal is
// short (forgectl#1074 review): when every row does not fit, the recent rows
// and their divider go first, since each repeats a command an area holds.
func (m *model) fitTopScreen() {
	// While the argument picker is open the list behind it is not what the
	// user acts on; trimming it would drop the recent row the picker was
	// opened from, and closing the picker would land the cursor elsewhere.
	if m.mode != hubMode || m.hubFlat || m.picker != nil || m.l.FilterState() != list.Unfiltered {
		return
	}
	// The list spends a line on its filter title and one on pagination
	// (bubbles reserves it even on a single page), so rows is its height
	// less those two.
	entries := topEntries(m.hub, m.l.Height()-2)
	if len(entries) == len(m.l.Items()) {
		return
	}
	name := m.selectedName()
	m.l.SetItems(hubItems(entries))
	m.l.Paginator.SetTotalPages(1)
	m.l.SetSize(m.l.Width(), m.l.Height())
	m.l.SetSize(m.l.Width(), m.l.Height())
	m.l.Select(0)
	m.selectRow(name)
}

// topEntries fits hub into rows lines: when all of it does not fit, the
// recent rows (Argv set) and the divider over them go; when the rest still
// does not fit, the dividers go too, leaving the keyed rows. rows <= 0 (no
// size yet) keeps everything.
func topEntries(hub []HubEntry, rows int) []HubEntry {
	if rows <= 0 || len(hub) <= rows {
		return hub
	}
	out := make([]HubEntry, 0, len(hub))
	for i, e := range hub {
		if e.Argv != nil || (e.Heading && i+1 < len(hub) && hub[i+1].Argv != nil) {
			continue
		}
		out = append(out, e)
	}
	if len(out) <= rows {
		return out
	}
	keyed := out[:0:0]
	for _, e := range out {
		if !e.Heading {
			keyed = append(keyed, e)
		}
	}
	return keyed
}

// refreshDelegate rebuilds the row delegate for the list's current items and
// size: the narrow flag and the hub name column.
func (m *model) refreshDelegate() {
	m.nameCol = hubNameColumn(m.l.Items(), m.width)
	m.l.SetDelegate(itemDelegate{g: m.glyph, narrow: m.width < 60, col: m.nameCol, styles: m.styles})
}

// tooSmall reports whether the terminal is below the smallest size the
// screens draw in. View then shows only a notice, and Update takes no key but
// quit, so nothing acts on a screen the user cannot see.
func (m model) tooSmall() bool {
	return m.width > 0 && m.height > 0 && (m.width < hubMinWidth || m.height < hubMinHeight)
}

// extraChromeLines is what the hub screens draw below the list beyond the
// shared header and footer: the detail lines ("$ forgectl …" and the
// description the row could not fit), and the picker box while one is open.
func (m model) extraChromeLines() int {
	if !m.hubScreen() {
		return 0
	}
	if m.picker != nil {
		return 1 + pickerLines()
	}
	return m.detailLines()
}

// hubDetailLines is the detail block's height: the "$ forgectl …" line and
// up to two lines of description. Below hubDetailMinHeight rows the block is
// the "$" line alone, so the list keeps room for more than one row.
const (
	hubDetailLines     = 3
	hubDetailMinHeight = 20
)

func (m model) detailLines() int {
	if m.height < hubDetailMinHeight {
		return 1
	}
	return hubDetailLines
}

// hubScreen reports whether the current screen is one of the hub's lists.
func (m model) hubScreen() bool {
	return m.mode == hubMode || m.mode == areaMode || m.mode == leavesMode
}

// skipHeading moves the hub cursor off a section divider, continuing in the
// direction the last key moved it (up for an upward key), and back the other
// way at the list's edge.
func (m *model) skipHeading(up bool) {
	isHeading := func() bool {
		it, ok := m.l.SelectedItem().(hubItem)
		return ok && it.entry.Heading
	}
	if !isHeading() {
		return
	}
	move := func(up bool) {
		if up {
			m.l.CursorUp()
		} else {
			m.l.CursorDown()
		}
	}
	before := m.l.Index()
	move(up)
	if isHeading() || m.l.Index() == before {
		m.l.Select(before)
		move(!up)
	}
}

// updateList runs msg through the list and stamps the filter runs it starts
// with the query's generation. Every message that can change the query (a key,
// a paste) goes through here, so a result from an older query can always be
// told from the current one: updateListMsg drops it (forgectl#1102).
func (m model) updateList(msg tea.Msg) (tea.Model, tea.Cmd) {
	query := m.l.FilterInput.Value()
	out, cmd := m.updateListMsg(msg)
	next, ok := out.(model)
	if !ok {
		return out, cmd
	}
	if next.l.FilterInput.Value() != query {
		next.filterGen++
	}
	return next, tagFilterRuns(cmd, next.filterGen)
}

func (m model) updateListMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	km, ok := msg.(tea.KeyPressMsg)
	if !ok {
		if run, ok := msg.(filterRun); ok {
			// Each change to the query starts its own filter run and the runs
			// finish in any order: the result for "p" can land after the one
			// for "pr" and replace it. Only the newest query's result is
			// current; an older one is dropped (forgectl#1102).
			if run.gen != m.filterGen {
				return m, nil
			}
			msg = run.msg
		}
		var cmd tea.Cmd
		m.l, cmd = m.l.Update(msg)
		if _, matches := msg.(list.FilterMatchesMsg); matches && m.hubScreen() {
			// Filter results arrive after the key that asked for them, and
			// bubbles counts pages from the previous result set until the
			// list is sized again; size it now so the page number is this
			// query's.
			m.applySize()
		}
		return m, cmd
	}
	// The hub screens size the list by filter state (applySize), and the
	// list sizes its filter input by the prompt (SetSize), so a key that
	// opens or closes a filter, or sets a prompt, re-sizes the list once it
	// is handled.
	before, prompt := m.l.FilterState(), m.l.FilterInput.Prompt
	out, cmd := m.updateListKey(km)
	if next, ok := out.(model); ok && next.hubScreen() &&
		(next.l.FilterState() != before || next.l.FilterInput.Prompt != prompt) {
		next.applySize()
		return next, cmd
	}
	return out, cmd
}

// updateListKey is updateList for a key press.
func (m model) updateListKey(km tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	msg := tea.Msg(km)

	// While filtering, the list owns every key.
	if m.l.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.l, cmd = m.l.Update(msg)
		m.unflattenHub()
		return m, cmd
	}

	key := km.String()
	if key == "/" && m.l.FilterState() == list.Unfiltered {
		// The list is shared by every screen, so each "/" sets the prompt
		// for the screen it opens on: the hub's top screen searches every
		// command, an area or subcommand list names what it filters, and
		// every other screen keeps the plain prompt.
		m.l.FilterInput.Prompt = filterPrompt
		switch {
		case m.mode == hubMode:
			m.flattenHub()
			m.l.FilterInput.Prompt = fitPrompt(m.width, hubSearchPrompt, "Search: ")
		case m.hubScreen():
			m.l.FilterInput.Prompt = fitPrompt(m.width, "Filter "+fitWords(m.title, hubFilterTitleMax)+": ", filterPrompt)
		}
	}
	if key == "esc" && m.l.FilterState() == list.FilterApplied && m.hubScreen() {
		// esc clears an applied filter before it backs out of the screen,
		// and leaves the cursor on the row it was on.
		name := m.selectedName()
		m.l.ResetFilter()
		m.unflattenHub()
		m.selectRow(name)
		return m, nil
	}
	switch key {
	case "q", "esc":
		switch m.mode {
		case hubMode:
			return m, tea.Quit
		case areaMode:
			m.toHub()
			return m, nil
		case leavesMode:
			if len(m.leavesUp) > 0 {
				// Inside a nested group: back to the list it was opened from.
				m.leaveGroup()
				return m, nil
			}
			if m.area != nil {
				m.backToArea()
				return m, nil
			}
			m.toHub()
			return m, nil
		case menuMode:
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
		return m.activate()
	}

	// Number-key select (thumb mode). The hub's top screen carries fixed
	// keys, and a key opens its row: every keyed row is a pinned module or an
	// area, so it opens a list or the argument picker and never runs a
	// command. Every other list numbers its rows by position. In the hub's
	// deeper lists (an area, a module's subcommands, the search-all list) a
	// digit only moves the cursor, so a command runs only on enter; the tmux
	// screens keep acting on a digit.
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' && m.mode == hubMode && !m.hubFlat {
		for i, it := range m.l.VisibleItems() {
			if hi, ok := it.(hubItem); ok && hi.entry.Key == int(key[0]-'0') {
				m.l.Select(i)
				return m.activate()
			}
		}
		return m, nil
	}
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		if idx := int(key[0] - '1'); idx < len(m.l.VisibleItems()) {
			m.l.Select(idx)
			if it, ok := m.l.SelectedItem().(hubItem); ok && it.entry.Heading {
				// A divider's number is not a row: land on the next real
				// row and wait for enter rather than run something the
				// operator did not number.
				m.skipHeading(false)
				return m, nil
			}
			if m.hubScreen() {
				return m, nil
			}
			return m.activate()
		}
		return m, nil
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
	m.unflattenHub()
	if m.mode == hubMode {
		switch key {
		case "up", "k", "ctrl+p", "pgup", "left", "h", "home", "g":
			m.skipHeading(true)
		default:
			m.skipHeading(false)
		}
	}
	return m, cmd
}

// activate handles enter / number select per screen. It takes no index:
// every mode reads the filter-aware SelectedItem(), because a raw index is a
// position in the FILTERED list (#496).
func (m model) activate() (tea.Model, tea.Cmd) {
	switch m.mode {
	case hubMode, areaMode:
		// m.l.Index()/number-key raw indices are positions in the FILTERED
		// list, not m.hub — indexing m.hub directly runs the wrong row once a
		// filter narrows the visible set (charm.land/bubbles/v2 list.Index()
		// docs: "consider using GlobalIndex() instead" for exactly this).
		// SelectedItem() is filter-aware.
		it, ok := m.l.SelectedItem().(hubItem)
		if !ok {
			return m, nil
		}
		entry := it.entry
		if entry.Heading {
			return m, nil
		}
		if entry.Members != nil {
			m.openArea(entry)
			return m, nil
		}
		if m.mode == hubMode && m.hubFlat && entry.Argv == nil {
			// A command reached through the search-all list belongs to an
			// area; esc from its subcommands goes back there. A recent row
			// (Argv set) is never an area's member, whatever its name.
			m.area = m.areaOf(entry.Name)
		}
		if entry.Argv != nil {
			if !entry.NeedsArgs {
				m.action = Action{Kind: ActionRunVerb, Argv: append([]string(nil), entry.Argv...)}
				return m, tea.Quit
			}
			if m.openPicker(entry.Argv, entry.Use, entry.NoPicker, nil) {
				return m, nil
			}
			if !requiresArg(entry.Use) {
				m.action = Action{Kind: ActionRunVerb, Argv: append([]string(nil), entry.Argv...)}
				return m, tea.Quit
			}
			usage := append(append([]string(nil), entry.Argv[:len(entry.Argv)-1]...), strings.Fields(entry.Use)...)
			m.action = Action{Kind: ActionShowInvocation, Argv: usage}
			return m, tea.Quit
		}
		if entry.Name == "tmux" {
			// tmux's hub row opens today's unchanged tmux jumper rather than
			// a drill-down list (Architecture: "opens today's menuMode
			// screen unchanged in content").
			m.title = "menu"
			m.setList(m.menuItems())
			m.mode = menuMode
			m.applySize()
			return m, nil
		}
		if entry.Name == "status" {
			// status's hub row opens the cockpit (forgectl#13 lane 2), as
			// tmux's opens its jumper: the overview is a screen to stay on,
			// not a report to print and exit.
			m.action = Action{Kind: ActionRunVerb, Argv: statusCockpitArgv()}
			return m, tea.Quit
		}
		if moduleNeedsArg(entry) {
			// The module itself needs one argument (pr <ref>): ask for it
			// here, with its subcommands one row away in the picker.
			browse := entry
			m.openPicker([]string{entry.Name}, entry.Use, entry.NoPicker, &browse)
			return m, nil
		}
		if len(entry.Leaves) == 0 {
			m.action = Action{Kind: ActionRunVerb, Argv: []string{entry.Name}}
			return m, tea.Quit
		}
		m.enterLeaves(entry)
		return m, nil
	case leavesMode:
		it, ok := m.l.SelectedItem().(leafItem)
		if !ok {
			return m, nil
		}
		leaf := it.leaf
		if len(leaf.Leaves) > 0 {
			m.enterGroup(leaf)
			return m, nil
		}
		if leaf.NeedsArgs {
			if m.openPicker(leafArgv(m.leavesPath, leaf), leaf.Use, leaf.NoPicker, nil) {
				return m, nil
			}
			if !requiresArg(leaf.Use) {
				// Every placeholder is optional ("docs check [dir|file ...]"):
				// the bare command runs, as `forgectl menu` reports it can.
				m.action = Action{Kind: ActionRunVerb, Argv: leafArgv(m.leavesPath, leaf)}
				return m, tea.Quit
			}
			m.action = Action{Kind: ActionShowInvocation, Argv: strings.Fields(usageLine(m.leavesPath, leaf))}
			return m, tea.Quit
		}
		m.action = Action{Kind: ActionRunVerb, Argv: leafArgv(m.leavesPath, leaf)}
		return m, tea.Quit
	case menuMode:
		// Same filter-aware SelectedItem() as hubMode above (#496).
		it, ok := m.l.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}
		switch it.act {
		case menuPick:
			m.enterPick()
		case menuSessions:
			m.enterSessions()
		case menuWindows:
			m.enterWindows()
		case menuTree:
			m.enterTree()
		case menuLast:
			m.action = Action{Kind: ActionLast}
			return m, tea.Quit
		case menuCheat:
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
	m.applySize()
}

// toHub returns to the hub's top screen — the quit level every other screen
// (menuMode, leavesMode) backs out to on q/esc.
func (m *model) toHub() {
	back := ""
	switch {
	case m.mode == areaMode && m.area != nil:
		back = m.area.Name
	case m.mode == leavesMode && len(m.leavesPath) > 0:
		back = m.leavesPath[0]
	case m.mode == menuMode:
		back = "tmux"
	}
	m.status = ""
	m.title = "hub"
	m.area = nil
	m.hubFlat = false
	m.setList(hubItems(m.hub))
	m.mode = hubMode
	m.selectRow(back)
	m.applySize()
}

// openArea lists one area's module rows, numbered by position: an area's
// membership is fixed at build time, so its numbers are as stable as the top
// screen's keys.
func (m *model) openArea(area HubEntry) {
	members := make([]HubEntry, len(area.Members))
	for i, e := range area.Members {
		e.Key = -1
		members[i] = e
	}
	m.status = ""
	m.title = area.Name
	m.area = &area
	m.hubFlat = false
	m.setList(hubItems(members))
	m.mode = areaMode
	m.applySize()
}

// backToArea returns from a module's subcommands to the area it was opened
// from, with the cursor on that module.
func (m *model) backToArea() {
	module := m.leavesPath[0]
	m.openArea(*m.area)
	m.selectRow(module)
}

// areaOf is the hub area holding the module named name, or nil.
func (m model) areaOf(name string) *HubEntry {
	for _, e := range m.hub {
		for _, member := range e.Members {
			if member.Name == name {
				area := e
				return &area
			}
		}
	}
	return nil
}

// selectRow puts the cursor on the hub row named name, if the list has one.
func (m *model) selectRow(name string) {
	if name == "" {
		return
	}
	for i, it := range m.l.Items() {
		switch row := it.(type) {
		case hubItem:
			if !row.entry.Heading && row.entry.Name == name {
				m.l.Select(i)
				return
			}
		case leafItem:
			if row.leaf.Name == name {
				m.l.Select(i)
				return
			}
		}
	}
}

// flattenHub swaps the top screen for every command in one list: the first-
// run and pinned rows, then each area's modules, each followed by its
// subcommands, numbered by position. Recent
// rows are left out — each repeats a command the list already holds — as are
// dividers, area rows, and a second row of one name (the first-run init row
// and setup's init).
func (m *model) flattenHub() {
	var flat []HubEntry
	seen := map[string]bool{}
	add := func(e HubEntry) {
		if !seen[e.Name] {
			seen[e.Name] = true
			e.Key = -1
			flat = append(flat, e)
		}
	}
	// A subcommand joins as a direct-command row under its full path ("pr
	// findings list"), so "/" finds it and enter runs it, opens its picker,
	// or prints its usage exactly as a recent row would. A nested group adds
	// its own subcommands rather than itself; the synthetic self leaf repeats
	// its module's row and is skipped.
	var addLeaves func(path []string, leaves []HubLeaf)
	addLeaves = func(path []string, leaves []HubLeaf) {
		for _, l := range leaves {
			if l.Self {
				continue
			}
			argv := append(append([]string(nil), path...), l.Name)
			if len(l.Leaves) > 0 {
				addLeaves(argv, l.Leaves)
				continue
			}
			add(HubEntry{Name: strings.Join(argv, " "), Short: l.Short, Use: l.Use, Argv: argv, NeedsArgs: l.NeedsArgs, NoPicker: l.NoPicker})
		}
	}
	addModule := func(e HubEntry) {
		add(e)
		addLeaves([]string{e.Name}, e.Leaves)
	}
	for _, e := range m.hub {
		switch {
		case e.Heading, e.Argv != nil:
		case e.Members != nil:
			for _, member := range e.Members {
				addModule(member)
			}
		default:
			addModule(e)
		}
	}
	m.searchFrom = m.selectedName()
	m.hubFlat = true
	m.setList(hubItems(flat))
}

// unflattenHub restores the top screen once the search-all list has no
// filter left, with the cursor on the row the search started from.
func (m *model) unflattenHub() {
	if m.mode != hubMode || !m.hubFlat || m.l.FilterState() != list.Unfiltered {
		return
	}
	m.hubFlat = false
	m.setList(hubItems(m.hub))
	m.fitTopScreen()
	m.refreshDelegate()
	// Back on the row the search started from. Clearing an applied search
	// re-selects the found row afterwards when the top screen has it.
	m.selectRow(m.searchFrom)
}

// enterLeaves opens entry's drill-down list.
func (m *model) enterLeaves(entry HubEntry) {
	m.leavesPath = []string{entry.Name}
	m.leavesUp = nil
	m.showLeaves(entry.Leaves)
}

// enterGroup opens a nested group's leaves one level below the current list.
func (m *model) enterGroup(group HubLeaf) {
	m.leavesUp = append(m.leavesUp[:len(m.leavesUp):len(m.leavesUp)], m.leaves)
	m.leavesPath = append(append([]string(nil), m.leavesPath...), group.Name)
	m.showLeaves(group.Leaves)
}

// leaveGroup returns from a nested group to the list it was opened from,
// with the cursor back on the group's row.
func (m *model) leaveGroup() {
	last := len(m.leavesUp) - 1
	parent := m.leavesUp[last]
	group := m.leavesPath[len(m.leavesPath)-1]
	m.leavesUp = m.leavesUp[:last]
	m.leavesPath = m.leavesPath[:len(m.leavesPath)-1]
	m.showLeaves(parent)
	for i, l := range parent {
		if l.Name == group && !l.Self {
			m.l.Select(i)
			break
		}
	}
}

// showLeaves shows leaves as the drill-down list at m.leavesPath.
func (m *model) showLeaves(leaves []HubLeaf) {
	m.status = ""
	m.hubFlat = false
	m.title = strings.Join(m.leavesPath, " ")
	if m.area != nil {
		m.title = m.area.Name + " › " + m.title
	}
	m.leaves = leaves
	m.setList(hubLeafItems(leaves))
	m.mode = leavesMode
	m.applySize()
}

// leafArgv builds the argv to run leaf, given the path it was listed under
// (the module name, then any nested groups): the path, then the leaf name.
func leafArgv(path []string, leaf HubLeaf) []string {
	argv := append([]string(nil), path...)
	if leaf.Self {
		// The synthetic bare-command leaf a NeedsArgs module or group
		// contributes (e.g. pr's own "pr <ref>") — running it means invoking
		// the path alone, not module+module.
		return argv
	}
	return append(argv, leaf.Name)
}

// usageLine returns the placeholder-carrying invocation text for a NeedsArgs
// leaf — "pr <ref>", or "projects clone <query>" for a subcommand whose own
// Use line (cobra convention) begins with its own name, not its parents'.
func usageLine(path []string, leaf HubLeaf) string {
	argv := leafArgv(path, leaf)
	return strings.Join(append(argv[:len(argv)-1], leaf.Use), " ")
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
	sessions, unreadable, err := m.client.DisplaySessionListing(m.ctx)
	if err != nil {
		slog.Error("Failed to load sessions.", "error", err)
		m.status = errStatus("tmux: ", err, m.styles)
	} else {
		m.noteUnreadable(tmux.UnreadableRows{Sessions: unreadable})
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
	windows, unreadable, err := m.client.DisplayWindowListing(m.ctx)
	if err != nil {
		slog.Error("Failed to load windows.", "error", err)
		m.status = errStatus("tmux: ", err, m.styles)
	} else {
		m.noteUnreadable(tmux.UnreadableRows{Windows: unreadable})
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
	out, unreadable, err := m.client.TreeListing(m.ctx, !m.noIcons)
	if err != nil {
		slog.Error("Failed to load tree.", "error", err)
		m.status = errStatus("tmux: ", err, m.styles)
	} else {
		m.noteUnreadable(unreadable)
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
	m.refreshDelegate()
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
	m.status = m.styles.OK.Render(termsafe.SafeLineMax("✓ "+ok, statusMaxRunes))
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
	if m.tooSmall() {
		v := tea.NewView(m.tooSmallView())
		v.AltScreen = true
		return v
	}
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
	parts := []string{header, body}
	if m.hubScreen() {
		// The list's blank padding is dropped so the detail lines sit right
		// under its last row, next to the selection, not at the screen's foot.
		parts[1] = trimBlankTail(body)
		if m.picker != nil {
			parts = append(parts, m.pickerView(), m.pickerDollar())
		} else {
			parts = append(parts, m.detailView())
		}
	}
	parts = append(parts, m.footerView())
	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, parts...))
	v.AltScreen = true
	return v
}

// tooSmallView replaces every screen when the terminal is below the smallest
// size the layout fits, saying so rather than drawing a broken frame.
func (m model) tooSmallView() string {
	msg := fmt.Sprintf("%s needs %d×%d; this terminal is %d×%d. Enlarge it, or press q to quit.",
		meta.AppName, hubMinWidth, hubMinHeight, m.width, m.height)
	return m.styles.Fg.Render(ansi.Wordwrap(msg, m.width, ""))
}

// trimBlankTail drops the blank lines a list pads its rows with.
func trimBlankTail(s string) string {
	lines := strings.Split(s, "\n")
	for len(lines) > 1 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func (m model) headerView() string {
	brandText := m.glyph.Forge + "  " + meta.AppName
	brand := m.styles.Brand.Render(m.glyph.Forge) + "  " + m.styles.Header.Render(meta.AppName)
	rest := m.title
	if m.mode == hubMode {
		if line := m.header.Line(); line != "" {
			rest = line
		}
	}
	if m.width > 0 {
		room := m.width - ansi.StringWidth(brandText) - 5
		if room < 1 {
			return brand
		}
		rest = fitWords(rest, room)
	}
	return brand + m.styles.Muted.Render("  ·  "+rest)
}

// detailView is the block under a hub list: the "$ forgectl …" line for the
// selected row, then that row's description in full (up to two lines) when
// the row itself had to cut or drop it.
func (m model) detailView() string {
	first := m.selectedDollar()
	if m.l.FilterState() != list.Unfiltered && len(m.l.VisibleItems()) == 0 {
		first = m.noMatchLine()
	}
	if m.detailLines() == 1 {
		return first
	}
	lines := make([]string, hubDetailLines)
	lines[0] = first
	descLines := hubDetailLines - 1
	name, desc := m.selectedText()
	width := m.l.Width()
	if desc != "" && width > 0 {
		lead := leaderWidth
		if _, _, cut := hubRow(name, desc, lead, m.nameCol, width); cut {
			wrapped := strings.Split(ansi.Wordwrap(desc, width-lead, " "), "\n")
			if len(wrapped) > descLines {
				wrapped[descLines-1] = fitWords(strings.Join(wrapped[descLines-1:], " "), width-lead)
				wrapped = wrapped[:descLines]
			}
			for i, w := range wrapped {
				// Wordwrap leaves a word longer than the line whole; cut it.
				lines[1+i] = strings.Repeat(" ", lead) + m.styles.Muted.Render(fitLine(w, width-lead))
			}
		}
	}
	return strings.Join(lines, "\n")
}

// noMatchLine is the detail line when a filter matches nothing: what was
// searched, where, and how to get out. The filter text is the operator's own
// typing, so it is escaped and capped like any free text.
func (m model) noMatchLine() string {
	query := capSafe(m.l.FilterValue(), hubHeaderValueMax)
	msg := fmt.Sprintf("no command or subcommand is named like %q · forgectl menu lists descriptions", query)
	if m.mode != hubMode {
		msg = fmt.Sprintf("nothing in %s is named like %q · / on the hub searches every command", m.title, query)
	}
	return m.styles.Muted.Render(fitWords(msg, m.l.Width()))
}

// enterHint is the footer's enter hint for the selected row: "enter run"
// when enter runs a command, "enter open" when it opens a list, a screen, or
// the argument picker. It follows activate's branches.
func (m model) enterHint() string {
	if len(m.l.VisibleItems()) == 0 {
		return "" // nothing to act on; the no-match line says why
	}
	runs := false
	switch it := m.l.SelectedItem().(type) {
	case hubItem:
		e := it.entry
		switch {
		case e.Heading, e.Members != nil, e.Name == "tmux" && e.Argv == nil:
		case e.Argv != nil:
			if e.NeedsArgs {
				return argHint(e.Use, e.NoPicker)
			}
			runs = true
		case e.Name == "status":
			runs = true
		default:
			runs = !moduleNeedsArg(e) && len(e.Leaves) == 0
		}
	case leafItem:
		if len(it.leaf.Leaves) == 0 && it.leaf.NeedsArgs {
			return argHint(it.leaf.Use, it.leaf.NoPicker)
		}
		runs = len(it.leaf.Leaves) == 0
	}
	if runs {
		return "enter run"
	}
	return "enter open"
}

// argHint is enterHint for a row whose Use names placeholders, following
// activate's order: the picker opens when it can take the argument; failing
// that, a row whose placeholders are all optional runs bare; otherwise enter
// leaves the hub and prints the command with its placeholders, to finish by
// hand.
func argHint(use string, noPicker bool) string {
	if _, _, ok := pickerSpec(use); ok && !noPicker {
		return "enter open"
	}
	if !requiresArg(use) {
		return "enter run"
	}
	return "enter print command"
}

// hubQueryMin is the fewest cells a filter prompt leaves for the query.
const hubQueryMin = 8

// fitPrompt is the first of prompts that leaves hubQueryMin cells for the
// query at width, or the shortest one, "/ ", when none does. A width of 0
// (no size yet) takes the first.
func fitPrompt(width int, prompts ...string) string {
	for _, p := range prompts {
		if width <= 0 || ansi.StringWidth(p)+hubQueryMin <= width {
			return p
		}
	}
	return "/ "
}

// filterPrompt is the list's plain filter prompt, and hubFilterTitleMax caps
// the screen title an area or subcommand list's prompt names.
const (
	filterPrompt      = "Filter: "
	hubSearchPrompt   = "Search commands: "
	hubFilterTitleMax = 20
)

// selectedName is the selected hub or leaf row's name — its identity, which
// selectRow matches — or "" for a divider or no selection.
func (m model) selectedName() string {
	switch it := m.l.SelectedItem().(type) {
	case hubItem:
		if !it.entry.Heading {
			return it.entry.Name
		}
	case leafItem:
		return it.leaf.Name
	}
	return ""
}

// selectedText is the selected hub or leaf row's name and description, as
// its row draws them.
func (m model) selectedText() (name, desc string) {
	switch it := m.l.SelectedItem().(type) {
	case hubItem:
		if it.entry.Heading {
			return "", ""
		}
		return it.label(), it.entry.Short
	case leafItem:
		return it.leaf.Name, it.desc()
	}
	return "", ""
}

// selectedDollar is the "$ forgectl …" line for the hub row under the
// cursor (forgectl#730 item 3): the exact argv enter runs, or the Use-line
// placeholder form when the row still needs an argument or drills into
// subcommands. A divider row shows an empty line so the layout does not jump.
func (m model) selectedDollar() string {
	var argv []string
	exact := false
	switch it := m.l.SelectedItem().(type) {
	case hubItem:
		e := it.entry
		switch {
		case e.Heading:
			return ""
		case e.Members != nil:
			return m.styles.Muted.Render(fitLine(fmt.Sprintf("enter lists %s (%d commands)", e.Name, len(e.Members)), m.l.Width()))
		case e.Argv != nil && (!e.NeedsArgs || argHint(e.Use, e.NoPicker) == "enter run"):
			argv, exact = e.Argv, true
		case e.Argv != nil:
			argv = append(append([]string(nil), e.Argv[:len(e.Argv)-1]...), strings.Fields(e.Use)...)
		case e.Name == "tmux":
			argv, exact = []string{e.Name}, true
		case e.Name == "status":
			argv, exact = statusCockpitArgv(), true
		case moduleNeedsArg(e):
			argv = strings.Fields(e.Use)
		case len(e.Leaves) == 0:
			argv, exact = []string{e.Name}, true
		default:
			argv = []string{e.Name, "<subcommand>"}
		}
	case leafItem:
		switch {
		case len(it.leaf.Leaves) > 0:
			argv = append(leafArgv(m.leavesPath, it.leaf), "<subcommand>")
		case it.leaf.NeedsArgs && argHint(it.leaf.Use, it.leaf.NoPicker) != "enter run":
			argv = strings.Fields(usageLine(m.leavesPath, it.leaf))
		default:
			argv, exact = leafArgv(m.leavesPath, it.leaf), true
		}
	default:
		return ""
	}
	if exact {
		return m.styles.Fg.Render(fitLine(dollarLine(argv), m.l.Width()))
	}
	return m.styles.Muted.Render(fitLine("$ "+meta.AppName+" "+strings.Join(argv, " "), m.l.Width()))
}

// fitLine cuts s to width cells with a trailing "…"; a width of 0 (no size
// yet) leaves it whole.
func fitLine(s string, width int) string {
	if width <= 0 || ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// footerView is the key hints for the current screen, fitted to the width:
// hints drop from least to most important until the line fits, so it never
// ends mid-word (forgectl#1074).
func (m model) footerView() string {
	var hints []string
	var prio []int
	switch {
	case m.picker != nil:
		enter := "enter run"
		if row, ok := m.picker.current(); ok && row.kind == pickerRowBrowse {
			enter = "enter open"
		}
		hints = []string{enter, "tab edit", "↑↓ choose", "esc back"}
		prio = []int{0, 3, 2, 1}
	case m.l.FilterState() == list.Filtering:
		hints = []string{"type to filter", "enter done", "esc clear"}
		prio = []int{2, 0, 1}
	case m.l.FilterState() == list.FilterApplied && m.hubScreen():
		hints = []string{"↑↓ move", "1-9 jump", m.enterHint(), "esc clear filter"}
		prio = []int{3, 2, 0, 1}
	}
	if hints == nil {
		switch m.mode {
		case hubMode:
			hints = []string{"↑↓ move", "1-9 open", m.enterHint(), "/ search", "q/esc quit", "forgectl --help lists every command"}
			prio = []int{4, 2, 3, 0, 1, 5}
		case areaMode, leavesMode:
			hints = []string{"↑↓ move", "1-9 jump", m.enterHint(), "/ filter", "q/esc back"}
			prio = []int{4, 2, 3, 0, 1}
		case menuMode:
			hints = []string{"1-6 / enter select", "q/esc back"}
			prio = []int{0, 1}
		case sessionsMode:
			hints = []string{"↑↓ move", "1-9 jump", "enter attach", "k kill", "K kill-others", "r rename", "/ filter", "q/esc back"}
			prio = []int{2, 7, 3, 5, 0, 4, 6, 1}
		case pickMode, windowsMode:
			hints = []string{"↑↓ move", "1-9 jump", "enter select", "/ filter", "q/esc back"}
			prio = []int{2, 4, 0, 3, 1}
		case treeMode, cheatMode:
			hints = []string{"↑↓ scroll", "q/esc back"}
			prio = []int{1, 0}
		case formMode:
			hints = []string{"enter confirm", "esc cancel"}
			prio = []int{0, 1}
		}
	}
	width := m.width
	if width <= 0 {
		width = 1 << 16
	}
	hint := m.styles.Muted.Render(fitHints(width, hints, prio))
	if m.status != "" {
		return m.status + "\n" + hint
	}
	return hint
}
