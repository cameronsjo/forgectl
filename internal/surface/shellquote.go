package surface

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnquotable reports a word that cannot be quoted identically across the
// shells a terminal manager might be running.
var ErrUnquotable = errors.New("surface: word cannot be safely quoted for a shell")

// Quote renders one word so a shell reproduces it verbatim.
//
// The strategy is single quotes and nothing else: inside them, POSIX sh gives
// every character its literal value, so spaces, `$`, `;`, backticks, globs,
// newlines, and a leading `=` all survive without a per-character table anyone
// could get wrong.
//
// It refuses two characters rather than escaping them, and that refusal is the
// interesting part. forgectl does not know which shell will run this. tmux
// executes through /bin/sh, but cmux and herdr *type the command into whatever
// shell the operator is already using* — which may be fish. A POSIX shell
// embeds a quote by closing the run, emitting an escaped quote, and reopening
// it; fish has no such form and instead treats a backslash before a quote or a
// backslash as an escape inside single quotes. So a word containing
// `'` or `\` is the one case where a single encoding cannot mean the same
// thing everywhere, and guessing wrong turns a quoted argument into shell
// syntax. Every other character is literal in all of them.
//
// In practice nothing forgectl puts in a bootstrap contains either: the socket
// path is a name this process generated under a directory it created, and the
// nonce is hex. The refusal exists for the one caller-shaped input — the
// absolute path of the forgectl binary itself.
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
// manager might hand it to.
func quotable(word string) error {
	for i, r := range word {
		switch {
		case r == '\'':
			return fmt.Errorf("%w: contains a single quote at byte %d, which POSIX and fish "+
				"escape differently inside single quotes", ErrUnquotable, i)
		case r == '\\':
			return fmt.Errorf("%w: contains a backslash at byte %d, which fish treats as an "+
				"escape inside single quotes and POSIX does not", ErrUnquotable, i)
		case r == 0:
			return fmt.Errorf("%w: contains a NUL at byte %d", ErrUnquotable, i)
		}
	}
	return nil
}
