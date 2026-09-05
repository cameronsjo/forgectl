package tui

// Test plan for keybinds.go
//
// KeybindSheet (Classification: pure render)
//   [x] Happy: a row with a Comment renders the comment, not the raw action
//   [x] Happy: a row with no Comment renders the raw action
//   [x] Happy: every trigger appears in the rendered output
//   [x] Happy: --no-icons swaps in the ASCII glyph set
//   [x] Unhappy: a hostile trigger/action/comment carrying C0/DEL bytes
//       renders with the control bytes stripped (forgectl#7 fold, mirrors
//       forgectl#162's original terminal hardening)
//
// sanitizeControlBytes (Classification: pure sanitizer)
//   [x] Happy: every C0 byte and DEL is stripped, visible content survives
//   [x] Happy: printable Unicode passes through untouched

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/ghostty"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
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

// TestKeybindSheet_SanitizesHostileBytes pins the forgectl#7 fold: a
// Trigger/Action/Comment carrying an ESC-based cursor-control sequence (or
// any other C0/DEL byte) must not reach the rendered sheet, since all three
// fields come from ghostty's own `+list-keybinds` process echoing the
// user's config file verbatim — a crafted config entry is untrusted input
// at this render boundary the same way a hostile PR title is (forgectl#162).
func TestKeybindSheet_SanitizesHostileBytes(t *testing.T) {
	hostile := "safe\x1b[2K\x1b[Gtrigger\x00\x01\x07\x7fend"
	rows := []ghostty.Keybind{
		{Trigger: hostile, Action: hostile},
		{Trigger: "escape", Action: hostile, Comment: hostile},
	}
	// Rendered with icons OFF, matching every other inertness assertion in this
	// package. Nerd Font glyphs live in the Unicode private use area, which is
	// not graphic, so an icon render trips the shared contract on forgectl's own
	// glyph table rather than on anything the hostile payload contributed. The
	// glyph set is orthogonal to sanitization, and
	// TestKeybindSheet_NoIcons_UsesASCIIGlyph covers the switch itself.
	out := KeybindSheet(rows, true)

	// The shared contract rather than a local byte list. This test used to scan
	// for 0x00/0x01/0x07/0x1b/0x7f by hand, which was a second, subtly narrower
	// notion of "safe" than the one every other output boundary asserts — the
	// exact drift termsafetest exists to prevent. AssertInert also covers the
	// C1 and bidi classes the hand-written list never mentioned.
	//
	// It permits the appearance-only SGR forgectl's own styles emit, which is
	// why this became necessary at the charm v2 migration: lipgloss v2 renders
	// colour unconditionally, so the sheet's own headings now carry ESC. The
	// hostile payload's cursor-control sequences are still rejected.
	termsafetest.AssertInert(t, "keybind sheet", out)
	for _, want := range []string{"safe", "trigger", "end", "escape"} {
		if !strings.Contains(out, want) {
			t.Errorf("KeybindSheet dropped visible content %q: got %q", want, out)
		}
	}
}

// TestSanitizeControlBytes_StripsAllC0AndDEL mirrors
// internal/cli's TestSanitizeCell_StripsAllC0AndDEL (pr_prs_test.go) for
// this package's independent copy of the sanitizer.
func TestSanitizeControlBytes_StripsAllC0AndDEL(t *testing.T) {
	hostile := "safe\x1b[2K\x1b[Gtitle\x00\x01\x07\x7fend\tmore\nlines\rhere"
	got := sanitizeControlBytes(hostile)

	for i := 0; i < 0x20; i++ {
		if strings.ContainsRune(got, rune(i)) {
			t.Errorf("sanitizeControlBytes output still contains C0 byte 0x%02x: %q", i, got)
		}
	}
	if strings.ContainsRune(got, 0x7f) {
		t.Errorf("sanitizeControlBytes output still contains DEL: %q", got)
	}
	for _, want := range []string{"safe", "title", "end", "more", "lines", "here"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitizeControlBytes dropped visible content %q: got %q", want, got)
		}
	}

	unicodeTitle := "café émoji \U0001F600 done"
	if got := sanitizeControlBytes(unicodeTitle); got != unicodeTitle {
		t.Errorf("sanitizeControlBytes must not touch printable Unicode: got %q, want %q", got, unicodeTitle)
	}
}
