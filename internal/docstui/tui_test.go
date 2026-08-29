package docstui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cameronsjo/forgectl/internal/docs"
	forgexec "github.com/cameronsjo/forgectl/internal/exec"
)

func testModel(t *testing.T) model {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"README.md": "# Home\n\n[Next](next.md)\n\n[Site](https://example.com)\n",
		"next.md":   "# Next\n\nBody\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := docs.NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	reloadC := make(chan string)
	m := newModel(context.Background(), docs.NewStore(idx), reloadC, &forgexec.FakeRunner{}, false)
	m.width, m.height = 100, 30
	m.applySize()
	// Select Home regardless of recency ordering.
	for _, doc := range idx.List() {
		if doc.Title == "Home" {
			m.load(doc, "", false)
		}
	}
	return m
}

func TestModel_AdaptiveLayoutAndNavigation(t *testing.T) {
	m := testModel(t)
	if !strings.Contains(m.View(), "Home") {
		t.Fatalf("wide view lacks current document: %q", m.View())
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m = updated.(model)
	if m.width >= narrowWidth || m.reader.Width != 58 {
		t.Fatalf("narrow sizing: width=%d reader=%d", m.width, m.reader.Width)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(model)
	if m.focus != 1 {
		t.Fatalf("focus = %d, want reader", m.focus)
	}
}

func TestModel_InternalLinkHistoryAndExternalConfirmation(t *testing.T) {
	m := testModel(t)
	var internal, external docs.TerminalLink
	for _, item := range m.linksList.Items() {
		link := item.(linkItem).link
		if strings.HasPrefix(link.Target, "http") {
			external = link
		} else {
			internal = link
		}
	}
	updated, _ := m.follow(internal)
	m = updated.(model)
	if m.current.Title != "Next" || len(m.history) != 1 {
		t.Fatalf("internal navigation: current=%q history=%d", m.current.Title, len(m.history))
	}
	m.goBack()
	if m.current.Title != "Home" {
		t.Fatalf("back returned to %q", m.current.Title)
	}
	updated, _ = m.follow(external)
	m = updated.(model)
	if m.pending == nil || !strings.Contains(m.status, "system browser") {
		t.Fatalf("external link did not require confirmation: pending=%v status=%q", m.pending, m.status)
	}
	updated, _ = m.confirmExternal(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = updated.(model)
	if m.pending != nil {
		t.Fatal("declined external link remained pending")
	}
}
