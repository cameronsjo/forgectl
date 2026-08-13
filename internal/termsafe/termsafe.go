// Package termsafe holds primitives for writing untrusted text to a terminal.
// Sanitize maps non-tab Cc controls and Unicode Bidi_Control formatting
// characters to spaces; tab and every other rune pass through.
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

// IsUnsafeTerminalRune reports whether r is a Cc control or a Unicode
// Bidi_Control formatting character. Contextual renderers may visibly quote
// additional runes, but must not broaden this shared classification.
func IsUnsafeTerminalRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control)
}

// Sanitize maps non-tab Cc controls and Unicode Bidi_Control formatting
// characters to spaces; tab and every other rune pass through.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || !IsUnsafeTerminalRune(r) {
			return r
		}
		return ' '
	}, s)
}
