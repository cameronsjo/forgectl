package tui

// Test plan for keybinds.go
//
// KeybindSheet (Classification: pure render)
//   [x] Happy: a row with a Comment renders the comment, not the raw action
//   [x] Happy: a row with no Comment renders the raw action
//   [x] Happy: every trigger appears in the rendered output
//   [x] Happy: --no-icons swaps in the ASCII glyph set

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/ghostty"
)

func TestKeybindSheet_CommentPreferredOverAction(t *testing.T) {
	rows := []ghostty.Keybind{
		{Trigger: "super+ctrl+t", Action: `text:\x00c`, Comment: "New tmux window"},
	}
	out := KeybindSheet(rows, false)
	if !strings.Contains(out, "New tmux window") {
		t.Errorf("expected comment in output, got: %s", out)
	}
	if strings.Contains(out, `text:\x00c`) {
		t.Errorf("raw action should not appear when a comment is present, got: %s", out)
	}
}

func TestKeybindSheet_NoCommentRendersRawAction(t *testing.T) {
	rows := []ghostty.Keybind{
		{Trigger: "escape", Action: "end_search"},
	}
	out := KeybindSheet(rows, false)
	if !strings.Contains(out, "end_search") {
		t.Errorf("expected raw action in output, got: %s", out)
	}
}

func TestKeybindSheet_EveryTriggerAppears(t *testing.T) {
	rows := []ghostty.Keybind{
		{Trigger: "super+t", Action: "new_tab"},
		{Trigger: "super+w", Action: "close_surface"},
		{Trigger: "super+ctrl+=", Action: "equalize_splits"},
	}
	out := KeybindSheet(rows, false)
	for _, row := range rows {
		if !strings.Contains(out, row.Trigger) {
			t.Errorf("expected trigger %q in output, got: %s", row.Trigger, out)
		}
	}
}

func TestKeybindSheet_NoIcons_UsesASCIIGlyph(t *testing.T) {
	rows := []ghostty.Keybind{{Trigger: "escape", Action: "end_search"}}
	out := KeybindSheet(rows, true)
	if !strings.HasPrefix(out, asciiGlyphs.Cheat) {
		t.Errorf("expected output to start with ascii cheat glyph %q, got: %s", asciiGlyphs.Cheat, out)
	}
}
