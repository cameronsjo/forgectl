package tui

import (
	"errors"
	"fmt"
)

// Caps for the text the TUI draws that forgectl did not compose (#934). Each
// counts escaped output runes (termsafe.SafeLineMax), and a cut value ends in
// termsafe.TruncatedMarker. They match internal/cli's classes for the same
// values, so a name reads the same on the screen and in `forgectl tmux ls`.
const (
	// nameMaxRunes caps a tmux session or window name and a sesh candidate:
	// internal/cli's titleMaxRunes, which tmux ls uses for the same names.
	nameMaxRunes = 256

	// statusMaxRunes caps a footer or picker status line, which can quote an
	// error carrying names and paths: internal/cli's textMaxRunes.
	statusMaxRunes = 1280
)

// ErrDumbTerminal is why a full-screen view refuses under TERM=dumb (#1152):
// such a terminal cannot move the cursor or switch to the alternate screen,
// so the view would print as a stream of escape sequences.
var ErrDumbTerminal = errors.New("TERM=dumb cannot draw a full-screen view")

// DumbTerminal reports a TERM value that cannot draw a full-screen view.
func DumbTerminal(term string) bool { return term == "dumb" }

// DumbTerminalError is a full-screen view's refusal under TERM=dumb: what
// refused, ErrDumbTerminal, and the plain form to use instead.
func DumbTerminalError(what, plain string) error {
	return fmt.Errorf("%s: %w; use %s", what, ErrDumbTerminal, plain)
}
