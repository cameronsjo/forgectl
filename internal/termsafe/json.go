package termsafe

import (
	"encoding/json"
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"
)

// JSONEncoder returns a *json.Encoder whose output reaches out through a filter
// that rewrites every terminal-unsafe character as a \uXXXX JSON escape.
// SetIndent and SetEscapeHTML behave exactly as on json.NewEncoder(out).
//
// This is value-preserving encoding, not sanitization, and that distinction is
// the whole point. A --json document is a machine contract that must hand back
// the operator's exact stored bytes, so the text renderer's approach — visibly
// quoting unsafe runes — would corrupt it. A \uXXXX escape instead decodes back
// to the very same rune, so the round trip is unchanged and only the terminal
// rendering differs.
//
// The preservation guarantee is scoped to VALID UTF-8, and that boundary is
// encoding/json's, not this filter's: the stdlib encoder substitutes U+FFFD for
// invalid bytes in a Go string, so a Unix path holding an unpaired 0xFF does not
// survive a --json round trip and never did. This filter matches that behaviour
// rather than diverging from it (escapeJSONUnsafe repairs the same way, which is
// what makes an overlong encoding of an unsafe rune fail closed instead of being
// reassembled downstream). Reversible encoding of arbitrary bytes would mean a
// different wire shape for every path field — a contract change, not a fix — so
// the claim is narrowed here instead of the behaviour being quietly overstated.
// TestJSONEncoder_InvalidUTF8IsReplacedNotPreserved pins it.
//
// encoding/json escapes every byte below 0x20 inside a string, which is why C0
// controls were never the gap. Above 0x20 it escapes only '"', '\\', U+2028 and
// U+2029 — so DEL, the C1 controls (including CSI and OSC), bidi formatting,
// and other non-graphic format characters such as U+FEFF, U+2060, and Unicode
// tag characters all reached a terminal literally. They were byte-faithful and
// not visibly inert; this filter makes them both while leaving ordinary graphic
// Unicode unchanged.
func JSONEncoder(out io.Writer) *json.Encoder {
	// termsafe:allow-raw-json wrapped here by escapingWriter before terminal output
	return json.NewEncoder(&escapingWriter{out: out})
}

// jsonEscapeFloor is where this filter's responsibility starts. Below it the
// encoder's own escaping is not merely sufficient but REQUIRED to leave alone:
// tab, newline and carriage return are Cc, and they are also JSON's structural
// whitespace, so escaping them would rewrite SetIndent's layout and the newline
// that terminates every document. Inside a string the encoder has already
// escaped them, so nothing below the floor can reach a terminal raw either way.
//
// That split is the one place this filter delegates, and delegating silently is
// how the C1 gap opened in the first place — so it is asserted, not assumed:
// TestEncoderEscapesEverythingBelowTheFloor checks that encoding/json really
// does escape every sub-floor rune IsUnsafeTerminalRune names.
const jsonEscapeFloor = 0x7F

// needsJSONEscape matches the text boundary's policy for controls and invisible
// format characters. JSON narrows that shared policy only by delegating the
// sub-floor runes encoding/json already escapes; graphic Unicode remains raw.
func needsJSONEscape(r rune) bool {
	return r >= jsonEscapeFloor && (IsUnsafeTerminalRune(r) || unicode.In(r, unicode.Cf))
}

// escapingWriter rewrites terminal-unsafe characters in a UTF-8 JSON byte
// stream as \uXXXX escapes.
//
// Rewriting the serialized document rather than each value is safe because none
// of these characters is structural above the floor: in a valid JSON document
// the only bytes outside a string literal are structure and ASCII whitespace,
// so every occurrence this filter can find is inside a string, where the escape
// is exactly equivalent. It is therefore a JSON filter, not a general one — do
// not point it at arbitrary bytes.
//
// A character split across two Write calls is handled by holding the incomplete
// trailing sequence back until the next Write completes it. Held bytes are
// released by the following Write whether or not they complete into something
// escapable, so the only lossy case is a stream that ends mid-character —
// truncated, invalid UTF-8, which encoding/json never produces: it substitutes
// U+FFFD for invalid input and terminates every document with a newline.
type escapingWriter struct {
	out  io.Writer
	held []byte
}

func (w *escapingWriter) Write(p []byte) (int, error) {
	buf := p
	if len(w.held) > 0 {
		buf = append(append(make([]byte, 0, len(w.held)+len(p)), w.held...), p...)
		w.held = w.held[:0]
	}
	if hold := incompleteSuffixLen(buf); hold > 0 {
		w.held = append(w.held[:0], buf[len(buf)-hold:]...)
		buf = buf[:len(buf)-hold]
	}
	if len(buf) == 0 {
		return len(p), nil
	}
	if _, err := w.out.Write(escapeJSONUnsafe(buf)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// incompleteSuffixLen reports how many trailing bytes of b begin a UTF-8
// character whose remaining bytes have not arrived yet — the bytes a filter
// must hold rather than emit.
//
// The condition is "incomplete character", not "prefix of a character we would
// escape", and the difference is a data-loss bug rather than a missed escape.
// Once invalid UTF-8 is repaired to U+FFFD, emitting a half-arrived SAFE
// character destroys it: the leading bytes are repaired on this Write and the
// trailing bytes on the next, so a split turns café into two replacement
// characters. Holding every incomplete character subsumes the escapable-prefix
// rule — every such prefix is itself an incomplete character — so one condition
// covers both jobs and there is no second table to drift.
func incompleteSuffixLen(b []byte) int {
	for k := 1; k < utf8.UTFMax && k <= len(b); k++ {
		start := b[len(b)-k]
		if !utf8.RuneStart(start) {
			continue // a continuation byte; keep walking back for its lead
		}
		if utf8.FullRune(b[len(b)-k:]) {
			return 0
		}
		return k
	}
	return 0
}

// escapeJSONUnsafe returns doc with each terminal-unsafe character replaced by
// its \uXXXX escape, or doc itself when there is nothing to rewrite.
//
// Invalid UTF-8 is replaced with U+FFFD rather than copied through, which is
// what encoding/json already does for a Go string. The difference matters for
// bytes that reach the encoder without that treatment — a json.RawMessage is
// checked for JSON syntax but not for UTF-8 validity — because an overlong
// encoding of an unsafe character would otherwise pass a rune-level predicate
// as several meaningless bytes and be reassembled by a lenient decoder. Failing
// closed on invalid input costs nothing on a well-formed document, where this
// branch never runs.
func escapeJSONUnsafe(doc []byte) []byte {
	if !mayContainUnsafe(doc) {
		return doc
	}
	escaped := make([]byte, 0, len(doc))
	for i := 0; i < len(doc); {
		r, size := utf8.DecodeRune(doc[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			escaped = utf8.AppendRune(escaped, utf8.RuneError)
		case needsJSONEscape(r):
			escaped = appendJSONEscape(escaped, r)
		default:
			escaped = append(escaped, doc[i:i+size]...)
		}
		i += size
	}
	return escaped
}

// appendJSONEscape writes r as one \uXXXX escape, or as the surrogate pair JSON
// requires above the BMP. Every rune needsJSONEscape accepts is in the BMP
// today; the pair branch keeps that from being a silent assumption.
func appendJSONEscape(dst []byte, r rune) []byte {
	if r > 0xFFFF {
		high, low := utf16Pair(r)
		return append(dst, fmt.Sprintf(`\u%04x\u%04x`, high, low)...)
	}
	return append(dst, fmt.Sprintf(`\u%04x`, r)...)
}

func utf16Pair(r rune) (high, low rune) {
	r -= 0x10000
	return 0xD800 + (r >> 10), 0xDC00 + (r & 0x3FF)
}

// mayContainUnsafe is a prefilter so an all-ASCII document — the overwhelmingly
// common one — is returned without a rune-by-rune pass.
//
// It triggers on any non-ASCII byte rather than on an enumerated set of lead
// bytes, for two reasons. The slow path does two jobs and a lead-byte table can
// predict only one: it escapes unsafe runes, and it repairs invalid UTF-8, and
// invalid input has no lead byte to enumerate — an overlong encoding of RLO
// starts 0xF0, which begins no escapable rune, so a table-derived prefilter
// hands exactly those bytes through. And applying the predicate directly to an
// ASCII byte is exact, since a byte below utf8.RuneSelf IS its own rune, which
// leaves no table to build at init and none to drift.
func mayContainUnsafe(doc []byte) bool {
	for _, b := range doc {
		if b >= utf8.RuneSelf || needsJSONEscape(rune(b)) {
			return true
		}
	}
	return false
}
