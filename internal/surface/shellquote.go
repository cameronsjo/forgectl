package surface

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// ErrUnquotable reports a word that cannot be quoted identically across the
// shells a terminal manager might be running.
var ErrUnquotable = errors.New("surface: word cannot be safely quoted for a shell")

// Quote renders one word so a shell reproduces it verbatim.
//
// The strategy is single quotes and nothing else: inside them, POSIX sh gives
// every character its literal value, so spaces, `$`, `;`, backticks, globs, and
// a leading `=` all survive without a per-character table anyone could get
// wrong.
//
// What makes this more than three lines is the refusal set, because forgectl
// does not know which shell will run the result. tmux executes through
// /bin/sh, but cmux and herdr *type the command into whatever shell the
// operator is already using*. Four byte classes cannot mean one thing across
// the shells that reach:
//
//   - A single quote. A POSIX shell embeds one by closing the quoted run,
//     emitting an escaped quote, and reopening; fish has no such form and
//     instead treats a backslash before a quote or a backslash as an escape
//     inside single quotes. One encoding cannot satisfy both.
//   - A backslash, for the same reason from the other side.
//   - A `!`. csh and tcsh perform history expansion *inside* single quotes,
//     and do it non-interactively: `tcsh -c "printf %s 'a!b'"` fails with
//     "Event not found". bash and zsh do not, even interactively. Both csh and
//     tcsh ship on every macOS.
//   - Anything termsafe calls an unsafe terminal rune — the C0 controls and
//     the Unicode bidi overrides. These are refused for a different reason
//     than the others: the bootstrap is *typed*, so the manager never parses
//     the quotes. A newline submits the line and executes the remainder as a
//     fresh command however well it is quoted, and an ESC opens a sequence to
//     the terminal emulator. Quoting is simply not the layer that defends
//     against a keystroke.
//
// Everything else is literal in sh, bash, zsh, fish, and csh alike.
//
// In practice nothing forgectl puts in a bootstrap contains any of them: the
// socket path is a name this process generated under a directory it created,
// and the nonce is hex. The refusal exists for the one caller-shaped input —
// the absolute path of the forgectl binary itself.
func Quote(word string) (string, error) {
	if err := quotable(word); err != nil {
		return "", err
	}
	if word == "" {
		return "''", nil
	}
	return "'" + word + "'", nil
}

// QuoteCommand renders a whole command line. It is not a convenience wrapper:
// a caller that quoted words individually and joined them itself would be one
// forgotten separator away from concatenating two arguments into one.
func QuoteCommand(words []string) (string, error) {
	if len(words) == 0 {
		return "", fmt.Errorf("%w: empty command", ErrUnquotable)
	}
	quoted := make([]string, 0, len(words))
	for i, word := range words {
		q, err := Quote(word)
		if err != nil {
			return "", fmt.Errorf("word %d: %w", i, err)
		}
		quoted = append(quoted, q)
	}
	return strings.Join(quoted, " "), nil
}

// quotable reports why a word cannot be encoded identically in every shell a
// manager might hand it to, or cannot survive being typed at all.
func quotable(word string) error {
	for i, r := range word {
		switch {
		case r == '\'':
			return fmt.Errorf("%w: contains a single quote at byte %d, which POSIX and fish "+
				"escape differently inside single quotes", ErrUnquotable, i)
		case r == '\\':
			return fmt.Errorf("%w: contains a backslash at byte %d, which fish treats as an "+
				"escape inside single quotes and POSIX does not", ErrUnquotable, i)
		case r == '!':
			return fmt.Errorf("%w: contains a '!' at byte %d, which csh and tcsh expand as "+
				"history even inside single quotes", ErrUnquotable, i)
		case termsafe.IsUnsafeTerminalRune(r):
			// Covers NUL, the C0 controls, and the bidi overrides. A newline
			// here would submit the typed line and run the remainder as a new
			// command; quoting cannot prevent that, because the manager types
			// rather than parses.
			return fmt.Errorf("%w: contains a control character at byte %d, which a terminal "+
				"manager would type rather than quote", ErrUnquotable, i)
		}
	}
	return nil
}
