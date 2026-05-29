package tui

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// ASCII glyphs keep test output deterministic — no Nerd Font codepoints.
var testGlyphs = asciiGlyphs

func TestMenuItemRender(t *testing.T) {
	item := menuItem{
		label: "Sessions",
		desc:  "attach · rename · kill",
		glyph: func(g glyphSet) string { return g.Session },
	}

	t.Run("selected contains label", func(t *testing.T) {
		got := item.render(0, true, false, testGlyphs)
		if !strings.Contains(got, "Sessions") {
			t.Errorf("selected render missing label: %q", got)
		}
	})
	t.Run("unselected wide contains description", func(t *testing.T) {
		got := item.render(0, false, false, testGlyphs)
		if !strings.Contains(got, "Sessions") {
			t.Errorf("wide render missing label: %q", got)
		}
		if !strings.Contains(got, "attach") {
			t.Errorf("wide render missing description: %q", got)
		}
	})
	t.Run("narrow omits description", func(t *testing.T) {
		got := item.render(0, false, true, testGlyphs)
		if strings.Contains(got, "attach · rename · kill") {
			t.Errorf("narrow render should omit description: %q", got)
		}
	})
	t.Run("selected and unselected differ", func(t *testing.T) {
		sel := item.render(0, true, false, testGlyphs)
		unsel := item.render(0, false, false, testGlyphs)
		if sel == unsel {
			t.Error("selected and unselected renders are identical")
		}
	})
}

func TestPickItemRender(t *testing.T) {
	item := pickItem("myproject")

	t.Run("contains item name", func(t *testing.T) {
		got := item.render(0, false, false, testGlyphs)
		if !strings.Contains(got, "myproject") {
			t.Errorf("pick render missing name: %q", got)
		}
	})
	t.Run("selected and unselected differ", func(t *testing.T) {
		sel := item.render(0, true, false, testGlyphs)
		unsel := item.render(0, false, false, testGlyphs)
		if sel == unsel {
			t.Error("selected and unselected renders are identical")
		}
	})
}

func TestWindowItemRender(t *testing.T) {
	item := windowItem{w: tmux.Window{
		Session: "main",
		Name:    "editor",
		Active:  true,
		Panes:   2,
		Target:  "main:editor",
	}}

	t.Run("contains session and window name", func(t *testing.T) {
		got := item.render(0, false, false, testGlyphs)
		if !strings.Contains(got, "main") {
			t.Errorf("missing session name: %q", got)
		}
		if !strings.Contains(got, "editor") {
			t.Errorf("missing window name: %q", got)
		}
	})
	t.Run("wide includes pane count", func(t *testing.T) {
		got := item.render(0, false, false, testGlyphs)
		if !strings.Contains(got, "2") {
			t.Errorf("wide render missing pane count: %q", got)
		}
	})
	t.Run("narrow omits pane count", func(t *testing.T) {
		got := item.render(0, false, true, testGlyphs)
		if strings.Contains(got, "panes") {
			t.Errorf("narrow render should omit pane count label: %q", got)
		}
	})
}
