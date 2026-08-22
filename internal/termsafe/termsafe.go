// Package termsafe holds primitives for writing untrusted text to a terminal.
// SafeLine and QuotePath are the human-output boundary: they visibly quote
// unsafe and non-graphic runes rather than deleting or replacing them, so the
// operator sees that something was there. JSONEncoder is the machine-output
// boundary, and is value-preserving instead — a --json document must hand back
// the stored bytes exactly.
//
// This package deliberately exports no weaker text primitive. An earlier
// Sanitize mapped non-tab Cc controls and Unicode Bidi_Control characters to
// spaces, which was both lossy (the operator could not tell a space from a
// suppressed control) and short: U+2028, U+2029, U+200B, U+00AD and U+2060 are
// none of those classes and passed through it untouched (#281). Every sink that
// used it now takes SafeLine or QuotePath.
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
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// IsUnsafeTerminalRune reports whether r is a Cc control or a Unicode
// Bidi_Control formatting character. Contextual renderers may visibly quote
// additional runes, but must not broaden this shared classification.
func IsUnsafeTerminalRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control)
}

// SafeLine turns arbitrary text into one inert physical terminal line. Go's
// graphic quoting escapes C0/C1 controls, DEL, tabs/newlines, and Unicode
// format characters (including bidi overrides) while retaining ordinary
// printable Unicode. The surrounding quotes are removed for sentence values.
func SafeLine(s string) string {
	var safe strings.Builder
	for _, r := range s {
		if !IsUnsafeTerminalRune(r) && unicode.IsGraphic(r) {
			safe.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRuneToGraphic(r)
		if len(quoted) >= 2 {
			safe.WriteString(quoted[1 : len(quoted)-1])
		} else {
			safe.WriteString(quoted)
		}
	}
	return safe.String()
}

// QuoteText visibly quotes an untrusted text field without allowing it to
// contribute terminal controls. Unlike applying %q after SafeLine, it escapes
// each original rune exactly once, so a newline is shown as \n rather than
// the more confusing \\n.
func QuoteText(text string) string {
	return strconv.QuoteToGraphic(text)
}

// QuotePath is QuoteText named for filesystem sinks, where the surrounding
// quotes also keep spaces and path boundaries legible.
func QuotePath(path string) string {
	return QuoteText(path)
}

// QuotePathIfUnsafe returns path verbatim when quoting would have changed
// nothing but the surrounding quotes, and the full QuotePath escaping
// otherwise.
//
// It exists for a sink whose output is BOTH rendered to a terminal and a
// documented machine-parseable field — `forgectl pr list` field 3, which
// `pr teardown` is fed. Unconditional quoting there would rewrite every
// ordinary row and break callers parsing it; printing raw would let a planted
// breadcrumb filename drive the reader's terminal. Quoting only the paths that
// need it keeps both properties, and the test for "needs it" is the escaping
// itself rather than a second, drift-prone predicate.
//
// Prefer plain QuotePath on any sink that is human-only.
func QuotePathIfUnsafe(path string) string {
	if quoted := QuotePath(path); quoted != `"`+path+`"` {
		return quoted
	}
	return path
}

type safeError struct {
	message string
	cause   error
}

func (e safeError) Error() string { return e.message }
func (e safeError) Unwrap() error { return e.cause }

// Error converts a nested filesystem/config error into terminal-safe text
// while preserving its unwrap chain for errors.Is/errors.As disposition.
// Known filesystem errors are reconstructed from individually escaped fields
// so a raw path can never be reinserted by their native Error method.
func Error(err error) error {
	if err == nil {
		return nil
	}
	message := SafeLine(err.Error())
	if linkErr, ok := err.(*os.LinkError); ok {
		message = fmt.Sprintf("%s %s %s: %s", SafeLine(linkErr.Op), QuotePath(linkErr.Old), QuotePath(linkErr.New), SafeLine(linkErr.Err.Error()))
	} else if pathErr, ok := err.(*os.PathError); ok {
		message = fmt.Sprintf("%s %s: %s", SafeLine(pathErr.Op), QuotePath(pathErr.Path), SafeLine(pathErr.Err.Error()))
	}
	return safeError{message: message, cause: err}
}
