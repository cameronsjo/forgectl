package launch

import (
	"os"

	"golang.org/x/term"
)

// IsInteractiveTTY reports whether both stdin and stdout are terminals.
//
// `forgectl launch` no longer consults it — the launcher drops straight into
// the resolved profile with no prompt. Its live use is the clean-room review's
// human approval gate (internal/pr Client.isTTY), which must stage rather than
// post when there is no terminal to approve on.
func IsInteractiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
