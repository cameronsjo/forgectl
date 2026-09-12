package sops

import (
	"errors"
	"fmt"
	"strings"
)

// Outcome reports what SetScalar did, because the two cases mean different
// things to an operator: adding a key is new configuration, replacing one is a
// rotation.
type Outcome int

const (
	// OutcomeUnspecified is the ineligible zero value.
	OutcomeUnspecified Outcome = iota
	// OutcomeAdded means the key was not present and a line was inserted.
	OutcomeAdded
	// OutcomeReplaced means an existing line's value was overwritten.
	OutcomeReplaced
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAdded:
		return "added"
	case OutcomeReplaced:
		return "replaced"
	default:
		return "unspecified"
	}
}

// line keeps a document line's text and its own terminator separate, so an
// insertion into a CRLF document is CRLF-terminated and a mixed-ending
// document is not silently normalised.
type line struct {
	text   string
	ending string
}

// indent returns the number of leading spaces. Tabs are refused document-wide
// before any line is inspected (see refuseUnsupportedDocument), so a space
// count is the whole story here.
func (l line) indent() int {
	for i := 0; i < len(l.text); i++ {
		if l.text[i] != ' ' {
			return i
		}
	}
	return len(l.text)
}

// leadingWhitespace returns the run of spaces AND tabs at the start of the
// line. It is deliberately separate from indent(): indent() stops at the
// first non-space, so a tab-indented line reports indent 0 and its tab never
// appears in the slice indent() would suggest — which made the tab refusal
// silently inspect an empty string.
func (l line) leadingWhitespace() string {
	for i := 0; i < len(l.text); i++ {
		if l.text[i] != ' ' && l.text[i] != '\t' {
			return l.text[:i]
		}
	}
	return l.text
}

func (l line) isBlank() bool { return strings.TrimSpace(l.text) == "" }

func (l line) isComment() bool { return strings.HasPrefix(strings.TrimSpace(l.text), "#") }

// isSequenceItem reports whether the line starts a YAML sequence entry. A
// sequence where a mapping was expected is a refusal, not something to edit.
func (l line) isSequenceItem() bool {
	t := strings.TrimSpace(l.text)
	return t == "-" || strings.HasPrefix(t, "- ")
}

// SetScalar writes value at path in a YAML document, editing text line-wise.
//
// # Why this is not a YAML round-trip
//
// Decoding and re-emitting would reflow every other block and reorder keys,
// turning a one-line change into a whole-file diff nobody can review — and in
// an encrypted file every reflowed line is a ciphertext change, so the diff
// would also destroy the property that makes a one-key write auditable. A real
// write measures 3 insertions and 2 deletions: the content line plus sops'
// own lastmodified and mac.
//
// # What it refuses
//
// The line model can bound a block only for documents of a particular shape,
// and every shape it cannot bound is a REFUSAL rather than a guess. That
// asymmetry is the whole design: a mis-bounded block produces a corrupted
// encrypted file, or a duplicate YAML key, and neither is visible until a
// consumer reads the wrong value. A missing block — at any level — is likewise
// refused and never created, because a block this code invents encrypts
// perfectly well and the consumer reads nothing from it.
func SetScalar(doc []byte, path []string, value string) ([]byte, Outcome, error) {
	if len(path) == 0 {
		return nil, OutcomeUnspecified, errors.New("path is empty")
	}

	lines := splitLines(string(doc))
	if err := refuseUnsupportedDocument(lines); err != nil {
		return nil, OutcomeUnspecified, err
	}

	// The top level: headers sit at indent 0 and the search covers the whole
	// document.
	start, end, wantIndent := 0, len(lines), 0

	// Walk every segment but the last, narrowing to its block each time.
	for _, segment := range path[:len(path)-1] {
		headerIdx, err := findHeader(lines, start, end, wantIndent, segment)
		if err != nil {
			return nil, OutcomeUnspecified, err
		}
		blockStart, blockEnd := blockRange(lines, headerIdx, wantIndent)
		childIndent, err := bodyIndent(lines, blockStart, blockEnd, wantIndent)
		if err != nil {
			return nil, OutcomeUnspecified, err
		}
		start, end, wantIndent = blockStart, blockEnd, childIndent
	}

	leaf := path[len(path)-1]
	if idx := findLeaf(lines, start, end, wantIndent, leaf); idx >= 0 {
		if leafIsMappingHeader(lines, idx, wantIndent, leaf) {
			// Names the rule and not the segment, the same discipline every
			// other refusal in this package follows: ParsePath's grammar is
			// wide enough that plenty of provider token formats parse as one
			// valid segment, so a secret pasted into the key slot reaches
			// here — and this message is relayed out of the child process to
			// the operator's terminal, which is the transcript the feature
			// exists to keep the value out of.
			//
			// It also leads with a word rather than the segment for a second
			// reason: fang title-cases an error's first token when it renders,
			// so opening with the key produced `"App" names a block` — a
			// capitalisation of the operator's own key, reading as a different
			// key than the one they typed.
			return nil, OutcomeUnspecified, errors.New("the path names a block rather than a value; only a scalar key can be set")
		}
		lines[idx] = replaceValue(lines[idx], value)
		return joinLines(lines), OutcomeReplaced, nil
	}

	insertAt := insertionPoint(lines, start, end)
	newLine := line{
		text:   strings.Repeat(" ", wantIndent) + leaf + ": " + encodeScalar(value),
		ending: endingFor(lines, insertAt),
	}
	// A document whose last line has no terminator: appending after it would
	// otherwise splice the two lines together. Terminate what was the last
	// line and leave the new one unterminated, so the file keeps its
	// no-trailing-newline shape instead of gaining one.
	if insertAt > 0 && insertAt == len(lines) && lines[insertAt-1].ending == "" {
		lines[insertAt-1].ending = newLine.ending
		newLine.ending = ""
	}
	lines = append(lines[:insertAt], append([]line{newLine}, lines[insertAt:]...)...)
	return joinLines(lines), OutcomeAdded, nil
}

// refuseUnsupportedDocument rejects whole-document shapes the line model
// cannot reason about, before any block is located.
func refuseUnsupportedDocument(lines []line) error {
	for _, l := range lines {
		// A tab in the leading whitespace makes every indent comparison in
		// this file meaningless — and YAML forbids tabs as indentation
		// anyway, so this is a malformed document rather than a limitation.
		if strings.ContainsRune(l.leadingWhitespace(), '\t') {
			return errors.New("document uses tab indentation; only spaces are supported")
		}
		// A multi-document stream has more than one root, so "the block at
		// indent 0" is ambiguous and the walk could enter the wrong document.
		if strings.HasPrefix(l.text, "---") || strings.HasPrefix(l.text, "...") {
			return errors.New("document is a multi-document stream; only a single document is supported")
		}
	}
	return nil
}

// findHeader locates a mapping header for segment at exactly wantIndent within
// [start,end).
//
// The match requires the line to be nothing but `<indent><segment>:` — no
// value, no trailing comment. A header carrying a comment (`block: # notes`)
// is refused explicitly rather than reported as missing, because "not found"
// would send the reader looking for a block that is plainly there.
func findHeader(lines []line, start, end, wantIndent int, segment string) (int, error) {
	prefix := strings.Repeat(" ", wantIndent) + segment + ":"
	for i := start; i < end; i++ {
		l := lines[i]
		if l.isBlank() || l.isComment() || l.indent() != wantIndent {
			continue
		}
		if !strings.HasPrefix(l.text, prefix) {
			continue
		}
		if strings.TrimSpace(l.text) != segment+":" {
			return 0, fmt.Errorf("block %q carries a value or a trailing comment on its header line; only a bare `%s:` is supported", segment, segment)
		}
		return i, nil
	}
	return 0, fmt.Errorf("no block %q at this level; creating one is out of scope", segment)
}

// blockRange returns the half-open line range holding a header's body.
//
// It ends at the first later line that is non-blank, NOT a comment, and
// indented at or below the header. Comments are excluded from the terminator
// on purpose: a comment sitting at column 0 in the middle of a block would
// otherwise cut the range short, the search would miss an existing sibling
// past it, and the key would be added a SECOND time — a duplicate YAML key,
// which is the worst outcome available here. bodyIndent refuses that document
// separately; this function simply must not make the decision.
func blockRange(lines []line, headerIdx, headerIndent int) (start, end int) {
	start = headerIdx + 1
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if l.isBlank() || l.isComment() {
			continue
		}
		if l.indent() <= headerIndent {
			return start, i
		}
	}
	return start, len(lines)
}

// bodyIndent derives the indent a child of this block must sit at.
//
// It is the indent of the block's first non-comment content line; an empty
// block falls back to the header's indent plus two. Comments are excluded
// from the derivation because `  # note` matches "indented content" perfectly
// well, and a comment indented differently from the real keys would silently
// reparent the inserted key.
func bodyIndent(lines []line, start, end, headerIndent int) (int, error) {
	for i := start; i < end; i++ {
		l := lines[i]
		if l.isBlank() {
			continue
		}
		if l.isComment() {
			// A comment at or below the header's indent, inside the block, is
			// ambiguous: it reads as belonging to the next block as easily as
			// to this one, and the range rule above deliberately does not let
			// it terminate the block. Rather than pick an interpretation,
			// refuse.
			if l.indent() <= headerIndent {
				return 0, errors.New("a comment inside the block is indented at or below its header; the block's extent is ambiguous")
			}
			continue
		}
		// A sequence where a mapping was expected: inserting `key: value`
		// would produce a document that is half list and half map.
		if l.isSequenceItem() {
			return 0, errors.New("the block holds a sequence, not a mapping; only scalar keys in a mapping are supported")
		}
		return l.indent(), nil
	}
	// An empty block. Two spaces past the header is the conventional depth,
	// and there is nothing else to infer from.
	return headerIndent + 2, nil
}

// findLeaf locates an existing `<indent><leaf>:` line, or -1.
//
// The trailing colon stops `llm_key_hermes_old:` from matching
// `llm_key_hermes`, and the exact-indent requirement keeps a same-named key in
// a nested block from being mistaken for this one.
//
// # Why the colon is not enough on its own
//
// A bare prefix match on `leaf + ":"` also matches a DIFFERENT key whose name
// merely begins that way: the key `a:b` matches the leaf `a`, and since
// replaceValue cuts at the first colon, replacing `a` would rewrite
// `    a:b: 'v'` as `    a: 'new'` — destroying a real key and its encrypted
// value, reporting `replaced a`, and passing every downstream check, because
// the extract of `["block"]["a"]` then returns exactly the value supplied.
//
// `a:b: 'v'` is valid YAML and decodes to the key `a:b`, so it can legitimately
// be in a SOPS file. Requiring a space or end-of-line after the colon is what
// separates the two. It also refuses the `key:value` no-space shape, which
// YAML reads as a plain scalar rather than a mapping at all.
func findLeaf(lines []line, start, end, wantIndent int, leaf string) int {
	prefix := strings.Repeat(" ", wantIndent) + leaf + ":"
	for i := start; i < end; i++ {
		l := lines[i]
		if l.isBlank() || l.isComment() || l.indent() != wantIndent {
			continue
		}
		if !strings.HasPrefix(l.text, prefix) {
			continue
		}
		rest := l.text[len(prefix):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return i
		}
	}
	return -1
}

// leafIsMappingHeader reports whether the line at idx is a block header rather
// than a scalar assignment — `sub:` with indented content beneath it.
//
// Writing a scalar over a mapping header produces invalid YAML: the header's
// children survive at their old depth under what is now a scalar, which
// yaml.v3 rejects with `did not find expected key`. The child's own parse
// catches that and the encrypted file survives, but the operator gets "the
// edited document does not parse as YAML", which names nothing they can act
// on and reads as a forgectl bug. Refusing by name here says what is actually
// wrong: the path names a block, not a value.
func leafIsMappingHeader(lines []line, idx, wantIndent int, leaf string) bool {
	// A line carrying anything after the colon is an assignment, not a header.
	if strings.TrimSpace(lines[idx].text) != leaf+":" {
		return false
	}
	for i := idx + 1; i < len(lines); i++ {
		l := lines[i]
		if l.isBlank() || l.isComment() {
			continue
		}
		return l.indent() > wantIndent
	}
	return false
}

// insertionPoint returns the index to insert a new key at, walking back past
// trailing blank lines so the key lands INSIDE the block rather than after the
// gap that separates it from whatever follows.
func insertionPoint(lines []line, start, end int) int {
	at := end
	for at > start && lines[at-1].isBlank() {
		at--
	}
	return at
}

// replaceValue rewrites a line's value, preserving the line's own indent and
// any trailing inline comment — `llm_key: xxx  # rotated 2026-01` keeps the
// note. Losing it would quietly discard operator context that is often the
// only record of why a key exists.
func replaceValue(l line, value string) line {
	colon := strings.Index(l.text, ":")
	head := l.text[:colon+1]
	rest := l.text[colon+1:]
	_, comment := splitValueAndComment(rest)

	text := head + " " + encodeScalar(value)
	if comment != "" {
		text += "  " + comment
	}
	return line{text: text, ending: l.ending}
}

// splitValueAndComment separates a scalar from a trailing `#` comment.
//
// It tracks quote state rather than searching for the first `#`, because a `#`
// inside a quoted scalar is data: `key: 'pass#word'` has no comment, and
// treating it as one would silently truncate the stored value. A `#` only
// begins a comment when it follows whitespace and sits outside quotes.
func splitValueAndComment(rest string) (value, comment string) {
	var inSingle, inDouble bool
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i > 0 && (rest[i-1] == ' ' || rest[i-1] == '\t') {
				return strings.TrimRight(rest[:i], " \t"), rest[i:]
			}
		}
	}
	return strings.TrimRight(rest, " \t"), ""
}

// endingFor picks the line terminator for a line inserted at idx: the
// terminator of the line it follows, falling back to the document's first
// terminator and finally to "\n" for a single-line document with none.
func endingFor(lines []line, idx int) string {
	if idx > 0 && lines[idx-1].ending != "" {
		return lines[idx-1].ending
	}
	for _, l := range lines {
		if l.ending != "" {
			return l.ending
		}
	}
	return "\n"
}

// splitLines splits s into lines, keeping each line's own terminator. A
// document with no trailing newline yields a final line with an empty ending,
// so joinLines reproduces the input byte-for-byte.
func splitLines(s string) []line {
	var out []line
	for len(s) > 0 {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			out = append(out, line{text: s})
			break
		}
		text := s[:idx]
		ending := "\n"
		if strings.HasSuffix(text, "\r") {
			text = text[:len(text)-1]
			ending = "\r\n"
		}
		out = append(out, line{text: text, ending: ending})
		s = s[idx+1:]
	}
	return out
}

func joinLines(lines []line) []byte {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.text)
		b.WriteString(l.ending)
	}
	return []byte(b.String())
}
