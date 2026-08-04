// Package termsafe holds the primitives for writing untrusted text to a
// terminal. Text forgectl renders that came from config, a database, or
// another process's output SHOULD pass through here, so a hostile string
// cannot clear the line, forge a posture, or set a color the operator did not
// ask for.
//
// "Should", not "does": this is what the package is for, not an invariant the
// codebase currently upholds everywhere. `launch which` and `config` still
// render config-derived values raw (#250). Reading this as "already handled"
// is how the next print path ships without calling Sanitize.
//
// The coverage is the Cc category only — C0, C1, and DEL. Cf (format)
// characters, bidirectional overrides among them, are outside
// unicode.IsControl and pass through; sanitize_fuzz_test.go pins that gap
// deliberately (#244), so read the guarantee as "no control byte", not
// "safe text".
//
// Named termsafe rather than term because golang.org/x/term is already
// imported unqualified as `term` in internal/cli and internal/launch — the two
// packages that depend on this one. Sharing the name would make goimports
// resolve a new `term.Sanitize` call to x/term and fail to compile in a
// package that visibly has one.
//
// Leaf package by design: it imports nothing from internal/, so every layer
// (cli, launch, …) can depend on it without a cycle.
package termsafe

import (
	"strings"
	"unicode"
)

// Sanitize replaces control bytes (everything unicode.IsControl except
// tab) with spaces so untrusted content renders inert in the terminal.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return ' '
	}, s)
}
