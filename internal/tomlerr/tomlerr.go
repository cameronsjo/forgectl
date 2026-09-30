// Package tomlerr renders BurntSushi/toml decode errors as allowlisted text:
// a position, a capped key and a fixed hint, never any part of the decoder's
// own message (#687, #738). Every TOML decode site imports it, so the
// allowlist has one home. Like termsafe it is low-level by design: it imports
// nothing from internal/ but termsafe, so config, workflow and bless can all
// depend on it without a cycle.
package tomlerr

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

// tomlHints is the ALLOWLIST Scrub renders from, in match order.
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

// Scrub rewords a BurntSushi/toml decode error from structured fields and a
// fixed hint, never from its message text (#687, #738).
//
// A ParseError's message is not safe to render in any part: the lexer quotes
// the leading bare word of an invalid unquoted value (`token = ghp_…` fails
// with `found "ghp"`), and an out-of-range number is printed bare in full
// (`log_level = 98765…` echoes every digit). So the rendering is an
// allowlist: the line, the column, the last key (capped), and the tomlHints
// entry the message matches.
//
// A decoder type mismatch is not a ParseError but a plain error whose text
// names the full key path, uncapped; see scrubDecodeError. Anything else (an
// os error from DecodeFile, a nil) passes through unchanged. The original
// error stays on the chain for errors.As.
func Scrub(err error) error {
	var pe toml.ParseError
	if err == nil {
		return nil
	}
	if !errors.As(err, &pe) {
		return scrubDecodeError(err)
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

// decoderPrefix opens every message toml v1.6.0's decoder builds with
// fmt.Errorf (decode.go's MetaData.e and its fixed-text siblings).
const decoderPrefix = "toml: "

// tomlValueKinds maps decode.go's fmtType (a %T of the decoded TOML value)
// to a TOML type name. A type not listed is left out of the rendering.
var tomlValueKinds = map[string]string{
	"string": "string", "int64": "integer", "float64": "float", "bool": "boolean",
	"time.Time": "datetime", "toml.LocalDate": "datetime", "toml.LocalTime": "datetime", "toml.LocalDatetime": "datetime",
	"[]any": "array", "[]map[string]any": "array", "map[string]any": "table",
}

// destinationKinds maps decode.go's destination names (badtype's fixed words)
// to the same vocabulary. A Go type name (a struct, a named type) is not
// listed and is left out.
var destinationKinds = map[string]string{
	"string": "string", "integer": "integer", "float": "float", "boolean": "boolean",
	"slice": "array", "array": "array", "map": "table", "table": "table",
}

// scrubDecodeError rebuilds a toml decoder error from a fixed template. The
// decoder writes `toml: [line N ](last key "<path>"): <message>` with the
// full key path %q-quoted, so a 400-rune key in a user-keyed map
// (launch.defaults.env, docs.root_kinds) echoes in full. The rendering keeps
// the line, the key capped through QuoteArgMax, and a hint chosen from fixed
// text; the types named are allowlisted words. A message that does not open with the
// decoder's prefix is not the decoder's and passes through unchanged.
func scrubDecodeError(err error) error {
	msg := err.Error()
	rest, ok := strings.CutPrefix(msg, decoderPrefix)
	if !ok {
		return err
	}
	var b strings.Builder
	b.WriteString("toml:")
	if after, found := strings.CutPrefix(rest, "line "); found {
		digits := 0
		for digits < len(after) && digits < 9 && after[digits] >= '0' && after[digits] <= '9' {
			digits++
		}
		if digits > 0 && strings.HasPrefix(after[digits:], " ") {
			b.WriteString(" line ")
			b.WriteString(after[:digits])
			rest = after[digits+1:]
		}
	}
	if after, found := strings.CutPrefix(rest, "(last key "); found {
		if quoted, qerr := strconv.QuotedPrefix(after); qerr == nil {
			if tail, closed := strings.CutPrefix(after[len(quoted):], "): "); closed {
				if key, uerr := strconv.Unquote(quoted); uerr == nil {
					b.WriteString(" (last key ")
					b.WriteString(termsafe.QuoteArgMax(key, 0))
					b.WriteString(")")
					rest = tail
				}
			}
		}
	}
	b.WriteString(": ")
	b.WriteString(decodeHintFor(rest))
	return termsafe.Categorical(b.String(), err)
}

// decodeHintFor words a decoder message from fixed text and the allowlisted
// type names it carries.
func decodeHintFor(msg string) string {
	if body, ok := strings.CutPrefix(msg, "incompatible types: TOML value has type "); ok {
		found, want, _ := strings.Cut(body, "; destination has type ")
		return wrongType(tomlValueKinds[found], destinationKinds[want])
	}
	if body, ok := strings.CutPrefix(msg, "type mismatch for "); ok {
		i := strings.LastIndex(body, ": expected table but found ")
		if i >= 0 {
			return wrongType(tomlValueKinds[body[i+len(": expected table but found "):]], "table")
		}
		return wrongType("", "")
	}
	if strings.HasPrefix(msg, "expected array length ") {
		return "wrong array length"
	}
	return "decode error"
}

func wrongType(found, want string) string {
	switch {
	case found != "" && want != "":
		return "wrong value type: found " + found + ", want " + want
	case want != "":
		return "wrong value type: want " + want
	case found != "":
		return "wrong value type: found " + found
	}
	return "wrong value type"
}

// maxKeysShown bounds Keys: a file with hundreds of stray keys cannot flood
// the error.
const maxKeysShown = 5

// Keys renders a decoder's undecoded-key list for an "unknown key(s)" error:
// each key quoted and capped through QuoteArgMax, at most maxKeysShown of
// them, then an ellipsis (#706, #738). toml.Key.String escapes only C0
// controls inside quoted pieces and leaves a bare piece of any length as is,
// so its output is not a safe echo on its own.
func Keys(keys []toml.Key) string {
	shown := make([]string, 0, maxKeysShown+1)
	for i, k := range keys {
		if i == maxKeysShown {
			shown = append(shown, "…")
			break
		}
		shown = append(shown, termsafe.QuoteArgMax(k.String(), 0))
	}
	return strings.Join(shown, ", ")
}
