package config

import (
	"errors"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// tomlHint maps one family of BurntSushi/toml ParseError messages to fixed
// text. match is tested against the message; hint is what the operator sees.
type tomlHint struct {
	match func(msg string) bool
	hint  string
}

func tomlPrefix(ps ...string) func(string) bool {
	return func(msg string) bool {
		for _, p := range ps {
			if strings.HasPrefix(msg, p) {
				return true
			}
		}
		return false
	}
}

func tomlContains(ss ...string) func(string) bool {
	return func(msg string) bool {
		for _, s := range ss {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
}

// tomlHints is the ALLOWLIST scrubTOMLError renders from, in match order.
// Each entry names a message family of toml v1.6.0's lexer, parser and
// decoder (lex.go, parse.go, error.go, decode.go's parseErr). The message is
// only ever matched against, never rendered: the lexer quotes the text it
// choked on, and errParseRange/errUnsafeFloat print an out-of-range number
// bare, so any rendering of it is a value echo (#687). A message no entry
// matches reads "syntax error".
var tomlHints = []tomlHint{
	{tomlContains("is out of range for", "is out of the safe"), "number out of range"},
	{tomlPrefix("invalid datetime"), "invalid datetime"},
	{tomlPrefix("invalid duration"), "invalid duration"},
	{tomlPrefix("invalid float", "Invalid float", "Invalid integer", "floats must start", "not a binary number", "not a hexadecimal number",
		"not an octal number", "cannot use sign with non-decimal", "expected a digit"), "invalid number"},
	{tomlPrefix(`unexpected EOF; expected '"`, `unexpected EOF; expected "'`, "strings cannot contain newlines"), "unterminated string"},
	{tomlPrefix(`unexpected "''''''"`, `unexpected '""""""'`), "too many quotes in a multi-line string"},
	{tomlPrefix("invalid escape", "Escaped character", "expected two hexadecimal digits", "expected four hexadecimal digits",
		"expected eight hexadecimal digits"), "invalid escape"},
	{tomlPrefix("unexpected EOF; expected value", "expected value but found"), "expected a value"},
	{tomlPrefix("expected '.' or '='", "unexpected EOF; expected key separator"), "expected '=' after a key"},
	{tomlPrefix("unexpected '=': key name appears blank", "unexpected '.': keys cannot start", "unexpected '='", "unexpected '.'"), "invalid key"},
	{tomlContains("table name", "table array name"), "invalid table header"},
	{tomlContains("has already been defined", "was already created", "is not a table"), "duplicate or conflicting key"},
	{tomlPrefix("expected a comma (',') or array terminator"), "invalid array"},
	{tomlPrefix("expected a comma or an inline table terminator", "newlines not allowed within inline tables"), "invalid inline table"},
	{tomlPrefix("unexpected comma"), "unexpected comma"},
	{tomlPrefix("expected a top-level item to end"), "unexpected text after a value"},
	{tomlPrefix("TOML files cannot contain control characters"), "control character in file"},
	{tomlPrefix("invalid UTF-8"), "invalid UTF-8"},
	{tomlPrefix("[theme.colors]:"), "invalid [theme.colors] entry: a colour takes a hex string or a {dark, light} table"},
	{func(msg string) bool { return msg == "unexpected EOF" }, "unexpected end of file"},
}

// tomlHintFor returns the fixed hint for a ParseError message.
func tomlHintFor(msg string) string {
	for _, h := range tomlHints {
		if h.match(msg) {
			return h.hint
		}
	}
	return "syntax error"
}

// scrubTOMLError rewords a toml.ParseError from structured fields and a fixed
// hint, never from its message text (#687).
//
// The message is not safe to render in any part: the lexer quotes the leading
// bare word of an invalid unquoted value (`token = ghp_…` fails with
// `found "ghp"`), and an out-of-range number is printed bare in full
// (`log_level = 98765…` echoes every digit). So the rendering is an
// allowlist: the line, the column, the last key (capped), and the tomlHints
// entry the message matches. That text reaches stderr through the loader's
// warning, the parse gate, `forgectl config` and `doctor`. Anything that is
// not a ParseError (decode.go's type mismatches name the key and the two
// types, never the value) passes through unchanged. The original error stays
// on the chain for errors.As.
func scrubTOMLError(err error) error {
	var pe toml.ParseError
	if err == nil || !errors.As(err, &pe) {
		return err
	}
	// Position is set by every ParseError constructor in toml v1.6.0; the
	// deprecated ParseError.Line is not a fallback worth keeping (parse.go
	// fills it with the token length on some paths).
	var b strings.Builder
	b.WriteString("toml: line ")
	b.WriteString(strconv.Itoa(pe.Position.Line))
	if pe.Position.Col > 0 {
		b.WriteString(", column ")
		b.WriteString(strconv.Itoa(pe.Position.Col))
	}
	if pe.LastKey != "" {
		b.WriteString(" (last key ")
		b.WriteString(termsafe.QuoteArgMax(pe.LastKey, 0))
		b.WriteString(")")
	}
	b.WriteString(": ")
	b.WriteString(tomlHintFor(pe.Message))
	return termsafe.Categorical(b.String(), err)
}
