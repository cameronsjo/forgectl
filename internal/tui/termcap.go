package tui

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
