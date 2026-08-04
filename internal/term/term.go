// Package term holds the primitives for writing untrusted text to a terminal.
// Anything forgectl renders that came from config, a database, or another
// process's output passes through here first, so a hostile string cannot
// clear the line, forge a posture, or set a color the operator did not ask for.
//
// The coverage is the Cc category only — C0, C1, and DEL. Cf (format)
// characters, bidirectional overrides among them, are outside
// unicode.IsControl and pass through; sanitize_fuzz_test.go pins that gap
// deliberately, so read the guarantee as "no control byte", not "safe text".
//
// Leaf package by design: it imports nothing from internal/, so every layer
// (cli, launch, …) can depend on it without a cycle.
package term

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
