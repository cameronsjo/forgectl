package tui

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// TestTUITextIsCapped is #934 for the TUI: a footer error, a session row and
// a window row each draw text forgectl did not compose, so each is escaped
// AND bounded. One 100k-rune value cannot flood the screen, and a cut value
// keeps its head and says it was cut.
//
// Mutations that turn it red: print errStatus's text, a session name, a
// session path, or a window name through termsafe.SafeLine instead of the
// capped form.
func TestTUITextIsCapped(t *testing.T) {
	long := func(head string) string { return head + strings.Repeat("x\u202e", 50_000) }
	s := theme.Default().Styles()
	g := pickGlyphs(true)
	for name, tc := range map[string]struct {
		got, head string
		limit     int
	}{
		"errStatus":    {errStatus("", errors.New(long("ERR")), s), "ERR", statusMaxRunes},
		"session name": {sessionItem{s: tmux.Session{Name: long("SESS"), Windows: 1}}.render(0, false, true, g, s), "SESS", nameMaxRunes},
		"session path": {sessionItem{s: tmux.Session{Name: "s", Windows: 1, Path: long("/PATH/")}}.render(0, false, false, g, s), "/PATH/", termsafe.PathEchoMaxRunes * 7},
		"window name":  {windowItem{w: tmux.Window{Session: "s", Name: long("WIN")}}.render(0, false, true, g, s), "WIN", nameMaxRunes},
	} {
		if n := utf8.RuneCountInString(tc.got); n > tc.limit+256 {
			t.Errorf("%s: rendered %d runes; want it capped near %d", name, n, tc.limit)
		}
		if !strings.Contains(tc.got, tc.head) {
			t.Errorf("%s: rendered %.120q; want its head %q kept", name, tc.got, tc.head)
		}
	}
}
