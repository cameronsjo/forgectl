package termsafe

import (
	"fmt"
	"strings"
)

// maxNamedKills caps how many session names the kill-others prompt spells out;
// the rest are counted, so the prompt stays on one screen with dozens of
// sessions.
const maxNamedKills = 6

// KillOthersPrompt names the sessions a kill-others confirm is about to kill,
// so the operator confirms the actual set rather than "ALL sessions". Names are
// quoted through QuoteTextMax, which shows control characters as escapes
// instead of sending them to the terminal. It lives here because both
// `tmux kill --others` (internal/cli) and the hub (internal/tui) need it and
// cli imports tui.
func KillOthersPrompt(keep string, doomed []string) string {
	shown := doomed
	if len(shown) > maxNamedKills {
		shown = shown[:maxNamedKills]
	}
	names := make([]string, len(shown))
	for i, n := range shown {
		names[i] = QuoteTextMax(n, 40)
	}
	list := strings.Join(names, ", ")
	if extra := len(doomed) - len(shown); extra > 0 {
		list += fmt.Sprintf(", and %d more", extra)
	}
	noun := "sessions"
	if len(doomed) == 1 {
		noun = "session"
	}
	return fmt.Sprintf("Keep %q and kill the other %d %s (%s)?", keep, len(doomed), noun, list)
}
