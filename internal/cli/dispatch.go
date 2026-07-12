package cli

import (
	"strings"

	"github.com/cameronsjo/forgectl/internal/forgive"
)

// argvForgiveness is one module's pre-Cobra argv surface: the spellings that
// converge on its canonical name, and the resolver that canonicalizes the
// verb that follows.
type argvForgiveness struct {
	name     string
	tokens   map[string]bool
	resolver *forgive.Resolver
}

// argvModules derives the forgiveness table from the module registry: only
// manifests declaring ArgvTokens participate. That is tmux alone today —
// extending argv forgiveness to every module is a flagged opt-in follow-on
// (ADR-0005), so the table stays behavior-identical to the old hardcoded
// tmux path.
func argvModules() []argvForgiveness {
	var out []argvForgiveness
	for _, m := range allModules() {
		if len(m.ArgvTokens) == 0 {
			continue
		}
		tokens := map[string]bool{m.Name: true}
		for _, t := range m.ArgvTokens {
			tokens[t] = true
		}
		out = append(out, argvForgiveness{
			name:     m.Name,
			tokens:   tokens,
			resolver: forgive.NewResolver(m.SubAliases),
		})
	}
	return out
}

// normalizeArgs rewrites a fat-fingered or iOS-autocorrected module verb to
// its canonical form before Cobra parses, so "LS." and "Kill," resolve and
// "rm" maps to kill. Only the module token and the verb that follows it are
// touched. Flags pass through untouched (so "--bogus" stays a strict flag
// error), and an unrecognized verb is left as-is — that's the signal the M5
// TUI fallthrough keys off.
func normalizeArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)

	mods := argvModules()
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
		for _, fm := range mods {
			if !fm.tokens[mod] {
				continue
			}
			out[i] = fm.name // converge every spelling/alias on the canonical module
			for j := i + 1; j < len(out); j++ {
				if out[j] == "--" {
					break // stop at end-of-flags; don't rewrite positional args
				}
				if isFlag(out[j]) {
					continue
				}
				if canon, known := fm.resolver.Canonical(out[j]); known {
					out[j] = canon
				}
				break
			}
			break
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
