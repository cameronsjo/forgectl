// Package docstui provides the interactive terminal reader for forgectl docs.
package docstui

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/docs"
	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const narrowWidth = 80

var (
	accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#B0B9F9")).Bold(true)
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#cc6666"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#b5bd68"))
)

type docItem struct{ doc docs.Doc }

func (i docItem) Title() string { return termsafe.SafeLine(i.doc.Title) }
func (i docItem) Description() string {
	return termsafe.SafeLine(i.doc.RootLabel + "/" + i.doc.RelPath)
}
func (i docItem) FilterValue() string {
	return i.doc.Title + " " + i.doc.RootLabel + "/" + i.doc.RelPath
}

type linkItem struct{ link docs.TerminalLink }

func (i linkItem) Title() string       { return termsafe.SafeLine(i.link.Text) }
func (i linkItem) Description() string { return termsafe.SafeLine(i.link.Target) }
func (i linkItem) FilterValue() string { return i.link.Text + " " + i.link.Target }

type reloadMsg struct{}
type openedMsg struct{ err error }

type location struct {
	doc    docs.Doc
	offset int
}

type model struct {
	ctx      context.Context
	store    *docs.Store
	reloadC  <-chan string
	runner   forgexec.Runner
	graphics bool

	width, height int
	focus         int
	docsList      list.Model
	linksList     list.Model
	reader        viewport.Model
	linkMode      bool
	pending       *docs.TerminalLink
	status        string

	current    docs.Doc
	history    []location
	currentIDs []uint32
	allIDs     []uint32
}

// Run owns the alternate-screen reader until the user quits or ctx is
// cancelled. The watcher and broker are the same proven reload path as the web
// reader; only the subscriber is a Bubble Tea command instead of SSE.
func Run(ctx context.Context, idx *docs.Index, runner forgexec.Runner, mode docs.GraphicsMode, in io.Reader, out io.Writer) error {
	store := docs.NewStore(idx)
	broker := docs.NewBroker()
	reloadC, unsubscribe := broker.Subscribe()
	defer unsubscribe()
	defer broker.Close()

	watcher, err := docs.NewWatcher(store, broker)
	if err != nil {
		return fmt.Errorf("start docs live reload: %w", err)
	}
	defer watcher.Close()
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go watcher.Run(watchCtx)

	m := newModel(ctx, store, reloadC, runner, docs.KittyGraphicsEnabled(mode, os.Getenv))
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	final, err := p.Run()
	if fm, ok := final.(model); ok && len(fm.allIDs) > 0 {
		_, _ = io.WriteString(out, docs.KittyCleanupSequence(fm.allIDs))
	}
	if err != nil {
		return fmt.Errorf("run docs reader: %w", err)
	}
	return nil
}

func newModel(ctx context.Context, store *docs.Store, reloadC <-chan string, runner forgexec.Runner, graphics bool) model {
	docsList := list.New(docItems(store.Current()), list.NewDefaultDelegate(), 0, 0)
	docsList.Title = "Documents"
	docsList.SetShowHelp(false)
	linksList := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	linksList.Title = "Links"
	linksList.SetShowHelp(false)
	m := model{
		ctx: ctx, store: store, reloadC: reloadC, runner: runner, graphics: graphics,
		docsList: docsList, linksList: linksList, reader: viewport.New(0, 0),
	}
	if item, ok := docsList.SelectedItem().(docItem); ok {
		m.load(item.doc, "", false)
	}
	return m
}

func docItems(idx *docs.Index) []list.Item {
	listed := idx.List()
	items := make([]list.Item, 0, len(listed))
	for _, doc := range listed {
		items = append(items, docItem{doc: doc})
	}
	return items
}

func (m model) Init() tea.Cmd { return waitReload(m.reloadC) }

func waitReload(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-ch; !ok {
			return nil
		}
		return reloadMsg{}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.applySize()
		if m.current.AbsPath != "" {
			m.load(m.current, "", false)
		}
		return m, nil
	case reloadMsg:
		m.reload()
		return m, waitReload(m.reloadC)
	case openedMsg:
		if msg.err != nil {
			m.status = errorStyle.Render(termsafe.SafeLine("open link: " + msg.err.Error()))
		} else {
			m.status = okStyle.Render("opened external link")
		}
		return m, nil
	case tea.KeyMsg:
		if m.pending != nil {
			return m.confirmExternal(msg)
		}
		if m.linkMode {
			return m.updateLinks(msg)
		}
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab":
			m.focus = (m.focus + 1) % 2
			return m, nil
		case "l":
			m.openLinks()
			return m, nil
		case "b":
			m.goBack()
			return m, nil
		case "?":
			m.status = "tab panes · / filter · enter open · l links · b back · q quit"
			return m, nil
		case "enter":
			if m.focus == 0 {
				if item, ok := m.docsList.SelectedItem().(docItem); ok {
					m.load(item.doc, "", true)
					if m.width < narrowWidth {
						m.focus = 1
					}
				}
				return m, nil
			}
		}
	}

	var cmd tea.Cmd
	if m.focus == 0 {
		m.docsList, cmd = m.docsList.Update(msg)
	} else {
		m.reader, cmd = m.reader.Update(msg)
	}
	return m, cmd
}

func (m *model) applySize() {
	bodyHeight := max(1, m.height-4)
	if m.width < narrowWidth {
		m.docsList.SetSize(max(20, m.width-2), bodyHeight)
		m.reader.Width = max(20, m.width-2)
		m.reader.Height = bodyHeight
		return
	}
	left := max(28, m.width/3)
	m.docsList.SetSize(left-2, bodyHeight)
	m.reader.Width = max(20, m.width-left-3)
	m.reader.Height = bodyHeight
}

func (m *model) load(doc docs.Doc, anchor string, push bool) {
	if push && m.current.AbsPath != "" && m.current.AbsPath != doc.AbsPath {
		m.history = append(m.history, location{doc: m.current, offset: m.reader.YOffset})
	}
	raw, err := os.ReadFile(doc.AbsPath)
	if err != nil {
		m.status = errorStyle.Render(termsafe.SafeLine(err.Error()))
		return
	}
	root, ok := rootFor(m.store.Current(), doc.RootLabel)
	if !ok {
		m.status = errorStyle.Render("document root disappeared")
		return
	}
	page, err := docs.RenderTerminal(raw, doc, root, max(20, m.reader.Width), m.graphics)
	if err != nil {
		m.status = errorStyle.Render(termsafe.SafeLine(err.Error()))
		return
	}
	if len(m.currentIDs) > 0 {
		page.Content = docs.KittyCleanupSequence(m.currentIDs) + page.Content
	}
	m.current = doc
	m.currentIDs = page.ImageIDs
	m.allIDs = append(m.allIDs, page.ImageIDs...)
	m.reader.SetContent(page.Content)
	m.reader.GotoTop()
	if anchor != "" {
		m.reader.SetYOffset(findAnchorLine(page.Content, anchor))
	}
	items := make([]list.Item, 0, len(page.Links))
	for _, link := range page.Links {
		items = append(items, linkItem{link: link})
	}
	m.linksList.SetItems(items)
	m.status = ""
}

func rootFor(idx *docs.Index, label string) (docs.Root, bool) {
	for _, root := range idx.Roots() {
		if root.Label == label {
			return root, true
		}
	}
	return docs.Root{}, false
}

func (m *model) reload() {
	idx := m.store.Current()
	selected := m.current.AbsPath
	m.docsList.SetItems(docItems(idx))
	if selected == "" {
		return
	}
	if doc, ok := idx.FindByAbsPath(selected); ok {
		offset := m.reader.YOffset
		m.load(doc, "", false)
		m.reader.SetYOffset(offset)
		m.status = okStyle.Render("reloaded")
		return
	}
	if item, ok := m.docsList.SelectedItem().(docItem); ok {
		m.load(item.doc, "", false)
		m.status = errorStyle.Render("current document was removed")
	}
}

func (m *model) openLinks() {
	if len(m.linksList.Items()) == 0 {
		m.status = mutedStyle.Render("this document has no links")
		return
	}
	m.linkMode = true
	m.linksList.SetSize(max(20, m.width-4), max(6, m.height-4))
}

func (m model) updateLinks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		m.linkMode = false
		return m, nil
	case "enter":
		item, ok := m.linksList.SelectedItem().(linkItem)
		if !ok {
			return m, nil
		}
		m.linkMode = false
		return m.follow(item.link)
	}
	var cmd tea.Cmd
	m.linksList, cmd = m.linksList.Update(msg)
	return m, cmd
}

func (m model) follow(link docs.TerminalLink) (tea.Model, tea.Cmd) {
	u, err := url.Parse(link.Target)
	if err != nil {
		m.status = errorStyle.Render("invalid link")
		return m, nil
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		m.pending = &link
		m.status = fmt.Sprintf("Open %s in the system browser? y/n", termsafe.SafeLine(link.Target))
		return m, nil
	}
	if u.Scheme != "" || u.Host != "" {
		m.status = errorStyle.Render("unsupported link scheme")
		return m, nil
	}
	if u.Path == "" {
		m.load(m.current, u.Fragment, false)
		return m, nil
	}
	candidate := filepath.Join(filepath.Dir(m.current.AbsPath), filepath.FromSlash(u.Path))
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		m.status = errorStyle.Render("linked document is unavailable")
		return m, nil
	}
	doc, ok := m.store.Current().FindByAbsPath(filepath.Clean(resolved))
	if !ok {
		m.status = errorStyle.Render("linked document is outside the index")
		return m, nil
	}
	m.load(doc, u.Fragment, true)
	return m, nil
}

func (m model) confirmExternal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		target := m.pending.Target
		m.pending = nil
		m.status = "opening external link…"
		return m, func() tea.Msg {
			return openedMsg{err: docs.OpenBrowser(m.ctx, m.runner, target)}
		}
	case "n", "N", "esc":
		m.pending = nil
		m.status = mutedStyle.Render("link not opened")
	}
	return m, nil
}

func (m *model) goBack() {
	if len(m.history) == 0 {
		m.status = mutedStyle.Render("no previous document")
		return
	}
	last := m.history[len(m.history)-1]
	m.history = m.history[:len(m.history)-1]
	m.load(last.doc, "", false)
	m.reader.SetYOffset(last.offset)
}

func findAnchorLine(content, anchor string) int {
	want := strings.ReplaceAll(strings.ToLower(anchor), "-", " ")
	for lineNo, line := range strings.Split(ansi.Strip(content), "\n") {
		normalized := strings.ToLower(strings.TrimSpace(line))
		if normalized == want || strings.Contains(normalized, want) {
			return lineNo
		}
	}
	return 0
}

func (m model) View() string {
	header := accentStyle.Render("◆ forgectl docs")
	if m.current.Title != "" {
		header += mutedStyle.Render("  ·  " + termsafe.SafeLine(m.current.Title))
	}
	footer := mutedStyle.Render("tab panes · / filter · enter open · l links · b back · ? help · q quit")
	if m.status != "" {
		footer = m.status + "\n" + footer
	}
	if m.pending != nil {
		footer = m.status
	}
	var body string
	if m.linkMode {
		body = m.linksList.View()
	} else if m.width < narrowWidth {
		if m.focus == 0 {
			body = m.docsList.View()
		} else {
			body = m.reader.View()
		}
	} else {
		body = lipgloss.JoinHorizontal(lipgloss.Top,
			lipgloss.NewStyle().Width(m.docsList.Width()).Render(m.docsList.View()),
			mutedStyle.Render("│ "),
			m.reader.View(),
		)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}
