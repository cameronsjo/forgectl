package cli

import (
	"strings"

	"github.com/cameronsjo/forgectl/internal/forgive"
)

// tmuxModuleTokens are the accepted spellings of the tmux module (matched after
// normalization, so "Tmux." also lands here).
var tmuxModuleTokens = map[string]bool{"tmux": true, "tm": true}

// normalizeArgs rewrites a fat-fingered or iOS-autocorrected tmux verb to its
// canonical form before Cobra parses, so "LS." and "Kill," resolve and "rm"
// maps to kill. Only the module token and the verb that follows it are
// touched. Flags pass through untouched (so "--bogus" stays a strict flag
// error), and an unrecognized verb is left as-is — that's the signal the M5
// TUI fallthrough keys off.
func normalizeArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)

	for i, tok := range out {
		if tok == "--" {
			break // POSIX end-of-flags: everything after is positional, leave it
		}
		if isFlag(tok) {
			continue
		}
		// First non-flag token is the module. Canonicalize its spelling, then
		// normalize the verb that follows. (A bare verb with no module prefix —
		// `forgectl LS.` — isn't matched here; it falls through to shouldLaunchTUI
		// and opens the menu, which is the intended thumb-mode behavior.)
		mod := forgive.Normalize(tok)
		if tmuxModuleTokens[mod] {
			out[i] = "tmux" // converge every spelling/alias on the canonical module
			for j := i + 1; j < len(out); j++ {
				if out[j] == "--" {
					break // stop at end-of-flags; don't rewrite positional args
				}
				if isFlag(out[j]) {
					continue
				}
				if canon, known := forgive.Canonical(out[j]); known {
					out[j] = canon
				}
				break
			}
		}
		break
	}
	return out
}

// isFlag reports whether tok is a flag. The bare "-" (last-session shorthand)
// and "--" (end-of-flags sentinel) are NOT flags — both are handled explicitly
// by the caller.
func isFlag(tok string) bool {
	return tok != "-" && tok != "--" && strings.HasPrefix(tok, "-")
}
