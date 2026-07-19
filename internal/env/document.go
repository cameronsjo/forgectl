// Package env is the domain layer for `forgectl env`: safe .env file
// management where secret values never cross the domain→CLI boundary as
// printable text (the CLI surface lands in a later commit; this package is
// pure and knows nothing of Cobra — the decoupling house pattern, see
// internal/net, internal/clean, internal/docker).
//
// document.go is the line-based parse/render model: a .env file is read
// into a Document of Lines, each either byte-verbatim (untouched) or
// re-rendered from its decoded fields (dirty, after Set). locate.go resolves
// and contains a --file path inside the invoking git repo, reusing
// internal/sandbox's audited containment primitive. write.go is the atomic,
// 0600-at-creation write path.
package env

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Kind classifies one logical Line of a Document.
type Kind int

const (
	// KindBlank is a whitespace-only line.
	KindBlank Kind = iota
	// KindComment is a line whose first non-whitespace character is '#'.
	KindComment
	// KindPair is a valid KEY=VALUE assignment (optionally `export`-prefixed).
	KindPair
	// KindMalformed is anything else — non-blank, non-comment, and either
	// missing an '=' or naming a key ValidKey rejects. Parse is lenient here
	// (it doesn't error) so `redact` can still mask a malformed line in its
	// entirety rather than leaving it unmasked.
	KindMalformed
)

// Line is one logical entry in a Document. For most Kinds it corresponds to
// exactly one physical source line; a KindPair whose value is a quoted
// string containing literal newlines (a PEM key, a cert, embedded JSON)
// spans multiple physical lines, all held in Raw.
type Line struct {
	Kind Kind
	// Raw holds the verbatim source line(s) this Line came from, terminator
	// stripped. Len > 1 for a multiline quoted KindPair, or for a
	// KindMalformed region spanning an unterminated quote through EOF.
	// Bytes emits Raw byte-for-byte for any Line that hasn't been touched
	// by Set.
	Raw []string
	// term holds each physical line's own terminator, parallel to Raw:
	// "\r\n", "\n", or "" only for a final unterminated physical line (EOF
	// with no trailing newline). Bytes and Redacted reproduce each
	// untouched line's OWN terminator from this rather than one style for
	// the whole document — the per-line fidelity a mixed CRLF/LF file
	// needs. Unexported: it's a rendering-fidelity detail, not part of the
	// Kind/Export/Key/Value/Quote/Inline surface callers reason about.
	term []string

	// Export, Key, Value, Quote, and Inline are populated only for KindPair
	// (and, partially, for error-tolerant masking of KindMalformed via Raw).
	Export bool
	Key    string
	// Value is the fully decoded logical value — may contain real newline
	// characters for a multiline quoted pair.
	Value string
	// Quote is 0 for a bare value, '"' or '\'' for a quoted one.
	Quote byte
	// Inline is the trailing text after the closing quote on its physical
	// line (typically a comment), verbatim including leading whitespace.
	// Only ever populated for a quoted pair — an unquoted value has no
	// comment boundary, so any trailing "#…" is absorbed into Value instead.
	Inline string

	// dirty is set by Set. A dirty Line is re-rendered from Export/Key/Value
	// via encode rather than emitted from Raw — Raw may be stale (or, for an
	// appended Line, empty) once dirty is true.
	dirty bool
}

// Document is a parsed .env file: an ordered list of Lines plus the two
// whole-file properties (dominant line-ending style, trailing-newline
// presence) needed to reproduce an untouched file byte-for-byte. Per-line
// terminator fidelity lives on each Line (see Line.term); crlf below is only
// the DEFAULT used for a new or fallback terminator, not what every line
// necessarily uses.
type Document struct {
	Lines []Line

	// crlf reports whether CRLF is the MAJORITY line terminator across the
	// file. It supplies the default terminator for a Set-appended line and
	// the fallback for any Line whose own term is unset — it is not applied
	// uniformly to every line; each untouched Line reproduces its own
	// recorded terminator instead (see Bytes).
	crlf bool
	// finalNL reports whether the source had a trailing newline after its
	// last line. Set-append always leaves this true (an appended pair line
	// always ends in a newline); an in-place Set never changes it.
	finalNL bool
}

// validKeyRE is the sole definition of a valid .env key name.
var validKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidKey reports whether key is a syntactically valid .env variable name:
// ^[A-Za-z_][A-Za-z0-9_]*$. This governs every display/mutate surface (keys,
// set, get); Parse's own assignment detection is more lenient (KindMalformed
// for anything ValidKey rejects) so redact can still mask a malformed line.
func ValidKey(key string) bool {
	return validKeyRE.MatchString(key)
}

// Parse reads a .env-format document, preserving everything needed for a
// byte-verbatim round-trip of every untouched line (see Bytes): comments,
// blanks, ordering, `export` prefixes, quote style, inline comments on
// quoted values, and a missing trailing newline — including PER-LINE CRLF
// vs LF fidelity: each physical line records its own terminator (Line.term),
// so a file that MIXES CRLF and LF lines round-trips every untouched line
// byte-for-byte regardless of which other line a Set touches. A Set-dirty
// or newly-appended line takes the file's DOMINANT terminator (majority
// across all lines; see dominantEOL) rather than any specific neighbor's.
//
// One deliberate non-goal: line splitting is on '\n' only, so a lone '\r'
// is never itself a terminator. A "classic Mac" (bare-CR-only) file parses
// as a SINGLE physical line — the '\r' bytes are ordinary content — and
// round-trips byte-verbatim as long as it's untouched, but collapses (like
// any other line) the moment a Set re-encodes it. Recognizing bare CR as a
// terminator is a materially riskier parser-model change (CR, LF, and CRLF
// would all need independent tracking), and such files are effectively
// extinct.
func Parse(r io.Reader) (*Document, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	doc := &Document{}
	if len(data) == 0 {
		return doc, nil
	}
	doc.crlf, doc.finalNL = dominantEOL(data)

	pos := 0
	for pos < len(data) {
		line, next := parseLine(data, pos)
		doc.Lines = append(doc.Lines, line)
		pos = next
	}
	return doc, nil
}

// dominantEOL scans every line terminator in data and reports whether CRLF
// is the MAJORITY style (a tie favors LF) — this is the default terminator
// Bytes uses for a Set-dirty or appended line, and the fallback for any
// Line whose own recorded terminator is empty. It also reports whether the
// file ends with a trailing newline. data must be non-empty.
func dominantEOL(data []byte) (crlf, finalNL bool) {
	var crlfCount, lfCount int
	for pos := 0; pos < len(data); {
		idx := bytes.IndexByte(data[pos:], '\n')
		if idx < 0 {
			break
		}
		end := pos + idx
		if end > pos && data[end-1] == '\r' {
			crlfCount++
		} else {
			lfCount++
		}
		pos = end + 1
	}
	crlf = crlfCount > lfCount
	finalNL = data[len(data)-1] == '\n'
	return crlf, finalNL
}

// readPhysicalLine returns the content of the physical line starting at pos
// (terminator stripped, CR stripped if present), that line's own terminator
// — "\r\n", "\n", or "" if pos's line has no trailing newline (the last
// line of a file missing a final newline) — and the position right after
// the terminator (or len(data) in the no-newline case).
func readPhysicalLine(data []byte, pos int) (content, term string, next int) {
	idx := bytes.IndexByte(data[pos:], '\n')
	if idx < 0 {
		return string(data[pos:]), "", len(data)
	}
	end := pos + idx
	raw := data[pos:end]
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
		term = "\r\n"
	} else {
		term = "\n"
	}
	return string(raw), term, end + 1
}

// parseLine classifies the physical (or, for a multiline quoted pair,
// physical-lines-spanning) line starting at pos, returning the Line and the
// data offset right after everything it consumed.
func parseLine(data []byte, pos int) (Line, int) {
	content, term, next := readPhysicalLine(data, pos)
	trimmed := strings.TrimLeft(content, " \t")

	if trimmed == "" {
		return Line{Kind: KindBlank, Raw: []string{content}, term: []string{term}}, next
	}
	if trimmed[0] == '#' {
		return Line{Kind: KindComment, Raw: []string{content}, term: []string{term}}, next
	}

	rest := trimmed
	export := false
	// "export " (or export\t) prefix — but only when it's genuinely a
	// separate keyword, not the start of a longer identifier (exportFOO=…)
	// or the literal key "export" itself (export=…, no separating
	// whitespace before the '=').
	if idx := strings.IndexAny(trimmed, " \t"); idx == len("export") && trimmed[:idx] == "export" {
		export = true
		rest = strings.TrimLeft(trimmed[idx:], " \t")
	}

	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return Line{Kind: KindMalformed, Raw: []string{content}, term: []string{term}}, next
	}
	key := rest[:eq]
	if !ValidKey(key) {
		return Line{Kind: KindMalformed, Raw: []string{content}, term: []string{term}}, next
	}
	valuePart := rest[eq+1:]

	if len(valuePart) == 0 || (valuePart[0] != '"' && valuePart[0] != '\'') {
		// Bare value: the entire remainder of the line, verbatim — no
		// comment boundary exists for an unquoted assignment (a trailing
		// "# …" is part of the value, per the redact spec).
		return Line{Kind: KindPair, Raw: []string{content}, term: []string{term}, Export: export, Key: key, Value: valuePart}, next
	}

	quote := valuePart[0]
	leadTrim := len(content) - len(trimmed)
	restOffset := len(trimmed) - len(rest)
	quoteOffsetInContent := leadTrim + restOffset + eq + 1
	quoteAbsPos := pos + quoteOffsetInContent
	bodyStart := quoteAbsPos + 1

	closeIdx, ok := findClosingQuote(data, bodyStart, quote)
	if !ok {
		// Unterminated quote at EOF: consume every remaining physical
		// line — from the opening line through EOF — as ONE KindMalformed
		// region. Splitting this into "opening line malformed,
		// continuation lines reparse independently" let a continuation
		// line that happens to be assignment-shaped and ValidKey-passing
		// (a base64 body line, say) reparse as its own KindPair —
		// redactPair then prints l.Key, a slice of key material, straight
		// through both redact and keys. None of the remaining file
		// content is safe to treat as anything but "part of the value
		// that never closed".
		var rawLines, terms []string
		cursor := pos
		for cursor < len(data) {
			c, tm, n := readPhysicalLine(data, cursor)
			rawLines = append(rawLines, c)
			terms = append(terms, tm)
			cursor = n
		}
		return Line{Kind: KindMalformed, Raw: rawLines, term: terms}, len(data)
	}

	// Walk physical lines from pos until the one containing closeIdx —
	// exactly one iteration for an ordinary same-line-closed value, more
	// than one for a multiline quoted value (a PEM key, a cert, embedded
	// JSON with literal newlines).
	var rawLines, terms []string
	cursor := pos
	var lastContent string
	var lastStart, finalNext int
	for {
		c, tm, n := readPhysicalLine(data, cursor)
		rawLines = append(rawLines, c)
		terms = append(terms, tm)
		if closeIdx < n {
			lastContent = c
			lastStart = cursor
			finalNext = n
			break
		}
		cursor = n
	}

	inline := ""
	if rel := closeIdx + 1 - lastStart; rel < len(lastContent) {
		inline = lastContent[rel:]
	}

	// What follows the closing quote decides whether this is a clean pair
	// with a comment, or an ambiguous line we must refuse. The rule is the
	// shell's own: a '#' begins a comment ONLY when it is preceded by
	// whitespace. `KEY="v" # note` is value `v` plus a comment; `KEY="v"#x`
	// is the single concatenated word `v#x` — the '#' glued to the close is
	// value data, not a comment marker (bash: `a"b"#c` is one word). So:
	//
	//   - trailer empty or all-whitespace  → clean pair (benign trailing ws)
	//   - whitespace, then '#', then …     → clean pair, that tail is a comment
	//   - anything else (a '#' glued to the close, non-'#' text, or a '#'
	//     reached without crossing whitespace) → the close is ambiguous
	//
	// Refusing the ambiguous line (KindMalformed, masked whole) beats the
	// two ways guessing fails at once: redact would print the trailer as if
	// it were safe comment text — leaking a secret glued after the quote
	// (`KEY="v"#sk_live_…`, embedded JSON, a single-quoted value with an
	// apostrophe) — while Get would hand back the TRUNCATED prefix, a broken
	// credential a caller copies believing it whole. An earlier fix keyed on
	// the value's own quote char reappearing in the trailer; that was the
	// wrong tell — a glued secret carrying no quote sailed straight through.
	// Whitespace-before-'#' is the discriminator that actually holds.
	if inline != "" {
		trimmed := strings.TrimLeft(inline, " \t")
		hadLeadingSpace := len(trimmed) < len(inline)
		allWhitespace := trimmed == ""
		isComment := hadLeadingSpace && strings.HasPrefix(trimmed, "#")
		if !allWhitespace && !isComment {
			return Line{Kind: KindMalformed, Raw: rawLines, term: terms}, finalNext
		}
	}

	value := decodeQuotedBody(data[bodyStart:closeIdx], quote)

	return Line{
		Kind:   KindPair,
		Raw:    rawLines,
		term:   terms,
		Export: export,
		Key:    key,
		Value:  value,
		Quote:  quote,
		Inline: inline,
	}, finalNext
}

// findClosingQuote scans data from start for the first occurrence of quote
// that isn't escaped. Only double-quoted values honor backslash-escaping
// while searching (matching bash: a single-quoted string can't contain an
// escaped quote at all, so a bare quote byte always closes it).
func findClosingQuote(data []byte, start int, quote byte) (int, bool) {
	i := start
	for i < len(data) {
		c := data[i]
		if quote == '"' && c == '\\' && i+1 < len(data) {
			i += 2
			continue
		}
		if c == quote {
			return i, true
		}
		i++
	}
	return -1, false
}

// decodeQuotedBody decodes the raw bytes between a pair of quotes into the
// logical value. Single-quoted content is literal (bash semantics — no
// escape processing at all). Double-quoted content reverses encodeValue's
// escaping: \\, \", \$, and \n (backslash-n → a real newline byte).
// Any other backslash-prefixed byte is kept as-is (backslash and byte both
// literal) — lenient toward a hand-authored file using an escape this
// package doesn't itself produce, rather than silently dropping data.
func decodeQuotedBody(body []byte, quote byte) string {
	if quote == '\'' {
		return string(body)
	}
	var b strings.Builder
	i := 0
	for i < len(body) {
		if body[i] == '\\' && i+1 < len(body) {
			switch body[i+1] {
			case '\\':
				b.WriteByte('\\')
				i += 2
				continue
			case '"':
				b.WriteByte('"')
				i += 2
				continue
			case '$':
				b.WriteByte('$')
				i += 2
				continue
			case 'n':
				b.WriteByte('\n')
				i += 2
				continue
			}
		}
		b.WriteByte(body[i])
		i++
	}
	return b.String()
}

// boundaryTerm returns l's own boundary terminator — the terminator
// recorded for its LAST physical source line — falling back to def when
// that terminator was never recorded (empty): a Line parsed as the file's
// final, unterminated line, later followed by a Set-appended Line, has no
// terminator of its own left to reproduce at that position.
func boundaryTerm(l Line, def string) string {
	if n := len(l.term); n > 0 && l.term[n-1] != "" {
		return l.term[n-1]
	}
	return def
}

// Bytes renders the Document back to .env text. Every Line untouched by Set
// is emitted from Raw byte-for-byte, each physical line followed by its OWN
// recorded terminator (see Line.term) — this is what gives a mixed CRLF/LF
// file per-line fidelity rather than one style for the whole document. A
// Line touched by Set is re-rendered from its Export/Key/Value fields via
// encode, followed by its boundary terminator (its term's last entry,
// carried over from before the edit — see Set). A recorded terminator that
// reads empty on a line that ISN'T actually the file's last physical line
// (e.g. a formerly-last, no-final-newline line that a later Set-append
// pushed off the end) falls back to the document's dominant default. The
// document's trailing-newline presence (finalNL) still gates only the very
// last physical emission, regardless of which lines were touched.
func (d *Document) Bytes() []byte {
	defaultTerm := "\n"
	if d.crlf {
		defaultTerm = "\r\n"
	}
	var b bytes.Buffer
	last := len(d.Lines) - 1
	for i, l := range d.Lines {
		isLastLine := i == last
		if l.dirty {
			b.WriteString(encode(l.Export, l.Key, l.Value))
			if !isLastLine || d.finalNL {
				b.WriteString(boundaryTerm(l, defaultTerm))
			}
			continue
		}
		lastPhys := len(l.Raw) - 1
		for j, p := range l.Raw {
			b.WriteString(p)
			if isLastLine && j == lastPhys {
				if d.finalNL {
					b.WriteString(boundaryTerm(l, defaultTerm))
				}
				continue
			}
			t := ""
			if j < len(l.term) {
				t = l.term[j]
			}
			if t == "" {
				t = defaultTerm
			}
			b.WriteString(t)
		}
	}
	return b.Bytes()
}

// Keys returns every valid KEY name in first-seen order, deduplicated.
// KindMalformed lines contribute nothing.
func (d *Document) Keys() []string {
	seen := make(map[string]bool)
	var keys []string
	for _, l := range d.Lines {
		if l.Kind != KindPair || seen[l.Key] {
			continue
		}
		seen[l.Key] = true
		keys = append(keys, l.Key)
	}
	return keys
}

// Get returns key's decoded value. When key appears more than once, the
// LAST occurrence wins (matching real dotenv-loader behavior — later
// assignments shadow earlier ones).
func (d *Document) Get(key string) (string, bool) {
	value, ok := "", false
	for _, l := range d.Lines {
		if l.Kind == KindPair && l.Key == key {
			value, ok = l.Value, true
		}
	}
	return value, ok
}

// Set assigns value to key: in place if key appears exactly once, appended
// as a new pair line if key is absent. A key appearing more than once is
// refused outright — deny-by-default, since silently editing one of two
// occurrences would leave the file lying about what "the" value is; the
// error names every line the key was found on so the caller can resolve it
// by hand.
//
// Setting an existing pair clears its Quote/Inline — those describe the
// OLD raw encoding, which Bytes no longer consults once the line is dirty,
// but Redacted always renders straight from these fields regardless of
// dirty state, so stale Quote/Inline would otherwise leak a comment that no
// longer corresponds to anything.
//
// Set is EXPORTED and renders key verbatim into the file (see encode) — a
// key containing '\n' or '=' would corrupt the file or inject an extra
// line/assignment. internal/cli already validates via ValidKey before ever
// calling into this package, but Set re-checks here as defense-in-depth at
// the exported package boundary: a direct domain-package caller (present or
// future, inside or outside this repo) gets the same guarantee without
// having to know the CLI enforces it upstream. The error is deliberately
// generic — never key itself — for the same reason errInvalidKey never
// echoes its argument: a key-shaped secret could be the argument.
func (d *Document) Set(key, value string) error {
	if !ValidKey(key) {
		return errors.New("invalid key")
	}

	var idxs []int
	for i, l := range d.Lines {
		if l.Kind == KindPair && l.Key == key {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) > 1 {
		at := make([]string, len(idxs))
		for i, idx := range idxs {
			at[i] = strconv.Itoa(d.lineNumber(idx))
		}
		return fmt.Errorf("duplicate key %q at lines %s; resolve manually", key, strings.Join(at, ","))
	}
	if len(idxs) == 1 {
		i := idxs[0]
		d.Lines[i].Value = value
		d.Lines[i].Quote = 0
		d.Lines[i].Inline = ""
		d.Lines[i].dirty = true
		return nil
	}
	defaultTerm := "\n"
	if d.crlf {
		defaultTerm = "\r\n"
	}
	d.Lines = append(d.Lines, Line{Kind: KindPair, Key: key, Value: value, dirty: true, term: []string{defaultTerm}})
	// An appended pair line always ends in a newline — this is the ONE new
	// line in the file, not a re-render of an existing one, so there's no
	// prior "no trailing newline" convention to preserve for it.
	d.finalNL = true
	return nil
}

// lineNumber returns the 1-based physical source line number where
// d.Lines[idx] begins, for duplicate-key error messages.
func (d *Document) lineNumber(idx int) int {
	n := 1
	for i := 0; i < idx; i++ {
		n += len(d.Lines[i].Raw)
	}
	return n
}

// Redacted renders the Document with every value masked: a fixed 4-char
// "****", no length hint, quotes dropped. Comments, blanks, and ordering
// are reproduced verbatim; a malformed line is masked in its ENTIRETY (see
// redactMalformed for why "preserve everything before the first '='" isn't
// safe). A multiline quoted pair collapses to ONE "KEY=****" line — its
// continuation lines never print. Each rendered line ends in its OWN
// boundary terminator (see boundaryTerm) rather than one style for the
// whole document — display-only consistency with Bytes on a mixed
// CRLF/LF file.
func (d *Document) Redacted() []byte {
	defaultTerm := "\n"
	if d.crlf {
		defaultTerm = "\r\n"
	}
	var b bytes.Buffer
	n := len(d.Lines)
	for i, l := range d.Lines {
		var rendered string
		switch l.Kind {
		case KindPair:
			rendered = redactPair(l)
		case KindMalformed:
			rendered = redactMalformed(l)
		default:
			rendered = l.Raw[0]
		}
		b.WriteString(rendered)
		if i != n-1 || d.finalNL {
			b.WriteString(boundaryTerm(l, defaultTerm))
		}
	}
	return b.Bytes()
}

// redactPair renders a single "KEY=****" (or "export KEY=****") line. The
// original trailing text is kept ONLY when the value was quoted — an
// unquoted value has no comment boundary, so any trailing "#…" was already
// folded into Value (and is masked along with it, by never being re-
// emitted here at all).
func redactPair(l Line) string {
	var b strings.Builder
	if l.Export {
		b.WriteString("export ")
	}
	b.WriteString(l.Key)
	b.WriteString("=****")
	if l.Quote != 0 && l.Inline != "" {
		b.WriteString(l.Inline)
	}
	return b.String()
}

// redactMalformed masks a malformed line (or, for an unterminated quoted
// region, the whole multi-line span — see parseLine's findClosingQuote
// branch) in its ENTIRETY — a constant "****", no pre-'=' preservation at
// all, and no per-physical-line breakdown. It used to keep everything
// before the first '=' on the theory that a key name can't be secret; that
// under-masks two real cases: (1) an unterminated quoted value at EOF — a
// truncated PEM's base64 body lines contain '=' padding, so preserving
// "everything before the first '='" preserved the base64 prefix and leaked
// key material (this is why parseLine now consumes the ENTIRE unterminated
// region as one Line, rather than letting a ValidKey-passing body line
// reparse independently into its own KindPair, whose Key redactPair would
// then print straight through); (2) a hand-mangled assignment whose key
// portion itself contains secret-ish text. Redaction's failure is
// asymmetric — under-masking leaks a secret permanently into a transcript,
// over-masking only costs legibility — and KindMalformed is exactly where
// hand-mangled input lands, which is redact's primary audience. A blank
// line never reaches here (Parse classifies it KindBlank), but the check is
// kept explicit rather than assumed.
func redactMalformed(l Line) string {
	if strings.TrimSpace(l.Raw[0]) == "" {
		return l.Raw[0]
	}
	return "****"
}

// bareValueRE matches a value safe to write unquoted.
var bareValueRE = regexp.MustCompile(`^[A-Za-z0-9_./:+@%-]+$`)

// encode renders a Line's Export/Key/Value into a single logical text line
// (no trailing terminator) — the dirty re-render path Bytes uses.
func encode(export bool, key, value string) string {
	var b strings.Builder
	if export {
		b.WriteString("export ")
	}
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(encodeValue(value))
	return b.String()
}

// encodeValue picks the tightest safe encoding for value: bare when it's
// made entirely of unambiguous characters, single-quoted (literal, no
// escaping at all) when it contains neither a single quote nor a newline,
// else double-quoted. Double-quote escaping order is pinned: backslash
// FIRST, then '"', then '$', then newline→"\n" — escaping in any other
// order double-escapes (e.g. escaping '"' before '\\' would re-escape the
// backslash the quote-escape just introduced).
func encodeValue(value string) string {
	if bareValueRE.MatchString(value) {
		return value
	}
	if !strings.Contains(value, "'") && !strings.Contains(value, "\n") {
		return "'" + value + "'"
	}
	v := value
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, `$`, `\$`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return `"` + v + `"`
}

// Diff compares file's and example's key sets by name only. missing is
// every key example has that file lacks (drift the operator must fix);
// extra is every key file has that example doesn't (reported but not an
// error — local-only secrets are benign). Both are returned sorted.
func Diff(file, example *Document) (missing, extra []string) {
	fileKeys := make(map[string]bool)
	for _, k := range file.Keys() {
		fileKeys[k] = true
	}
	exampleKeys := make(map[string]bool)
	for _, k := range example.Keys() {
		exampleKeys[k] = true
	}
	for _, k := range example.Keys() {
		if !fileKeys[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range file.Keys() {
		if !exampleKeys[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}
