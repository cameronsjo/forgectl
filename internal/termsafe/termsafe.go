// Package termsafe holds the primitives for writing untrusted text to a
// terminal. Text forgectl renders that came from config, a database, or
// another process's output SHOULD pass through here, so a hostile string
// cannot clear the line, forge a posture, or set a color the operator did not
// ask for.
//
// Sanitize is the older compatibility primitive and covers the Cc category
// only. SafeLine and QuotePath are the stronger final-output boundary: they
// also escape tabs and Unicode format characters such as bidi overrides.
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

// SafeLine turns arbitrary text into one inert physical terminal line. Go's
// graphic quoting escapes C0/C1 controls, DEL, tabs/newlines, and Unicode
// format characters (including bidi overrides) while retaining ordinary
// printable Unicode. The surrounding quotes are removed for sentence values.
func SafeLine(s string) string {
	var safe strings.Builder
	for _, r := range s {
		if unicode.IsGraphic(r) {
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

// QuotePath is SafeLine with explicit quotes, so spaces and path boundaries
// remain legible without allowing the path to contribute terminal controls.
func QuotePath(path string) string {
	return strconv.QuoteToGraphic(path)
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
