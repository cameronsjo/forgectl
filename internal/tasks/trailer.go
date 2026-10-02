package tasks

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// A trailer is the one line this client appends to a task description to say
// who wrote to the task and through what. It is provenance, not proof: the
// description is editable by anyone the project is shared with, so nothing may
// read a trailer to decide whether a write is allowed or already happened.
//
// Two kinds exist. Their text is fixed here so the line a create writes and
// the line a close writes cannot drift apart:
//
//	created-by: <closer> via forgectl tasks mcp <RFC3339 UTC>
//	closed-by: <closer> via forgectl tasks <surface> <RFC3339 UTC> — <evidence>
const (
	trailerCreatedBy = "created-by"
	trailerClosedBy  = "closed-by"
)

// SurfaceMCP and SurfaceDone are the two values a trailer's surface field
// takes: the MCP server, and the `forgectl tasks done` verb.
const (
	SurfaceMCP  = "mcp"
	SurfaceDone = "done"
)

// defaultCloserMCP and defaultCloserDone name the closer when the declared
// name is empty once sanitized. A trailer with no name in it would not match
// its own grammar, and a client that declares a name made only of stripped
// characters should not be able to produce one.
const (
	defaultCloserMCP  = "forgectl (mcp)"
	defaultCloserDone = "cli"
)

// DefaultDoneCloser is the closer `forgectl tasks done` names when its caller
// declares none: the same name an empty declaration falls back to.
const DefaultDoneCloser = defaultCloserDone

// maxEvidenceRunes and maxCloserRunes bound the two caller-supplied fields.
// Evidence names a merged PR or a command and its result; 300 characters holds
// either, and is short enough that the line stays readable in the web UI.
const (
	maxEvidenceRunes = 300
	maxCloserRunes   = 100
)

// evidenceTokenRe finds a Vikunja API token anywhere in evidence. It is
// deliberately not tokenShape: that one is anchored and wants 40 hex, because
// it answers "is this whole value a token". Evidence is free text pasted by an
// agent, so the question here is "is there a token, or the front of one, in
// it" — unanchored, and 20 hex is already more of a credential than a board
// that every shared user can read should carry.
var evidenceTokenRe = regexp.MustCompile(`tk_[0-9a-fA-F]{20,}`)

// closedByLineRe is the strict grammar of a closed-by line: exactly what
// trailerLine writes and nothing looser. The closer class is the sanitizer's
// allowlist, which holds neither ':' nor '—', so a closer cannot contain a
// timestamp or the evidence separator and the fields cannot be re-split.
var closedByLineRe = regexp.MustCompile(
	`^closed-by: ([A-Za-z0-9._()/@ -]{1,100}) via forgectl tasks (mcp|done) ` +
		`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z) — ([^\r\n]+)$`)

// closingTrailer is a closed-by line split into its fields.
type closingTrailer struct {
	closer, surface, stamp, evidence string
}

// parseClosingTrailer reports whether line is a closed-by trailer in the
// strict grammar, and its fields when it is.
func parseClosingTrailer(line string) (closingTrailer, bool) {
	m := closedByLineRe.FindStringSubmatch(line)
	if m == nil {
		return closingTrailer{}, false
	}
	tr := closingTrailer{closer: m[1], surface: m[2], stamp: m[3], evidence: m[4]}
	// The closer class admits spaces and letters, so the expression alone
	// would accept "a via b" as a closer. sanitizeCloser never writes one, and
	// a line that holds one was not written by this client.
	if tr.closer != sanitizeCloser(tr.closer, "") || strings.TrimSpace(tr.evidence) == "" {
		return closingTrailer{}, false
	}
	return tr, true
}

// trailerLine builds one trailer line. kind is trailerCreatedBy or
// trailerClosedBy; surface is SurfaceMCP or SurfaceDone. A created-by trailer
// has no evidence field and refuses one; a closed-by trailer requires it.
//
// Every refusal is local and names what was wrong without repeating the
// evidence: the caller is usually an agent, the error is usually shown to it,
// and text refused for holding a credential or a control sequence must not
// come back out through the refusal.
func trailerLine(kind, closer, surface string, now time.Time, evidence string) (string, error) {
	fallback, err := defaultCloser(surface)
	if err != nil {
		return "", err
	}
	// A zero time would put year 1 on the board as a close date.
	if now.IsZero() {
		now = time.Now()
	}
	head := fmt.Sprintf("%s: %s via forgectl tasks %s %s",
		kind, sanitizeCloser(closer, fallback), surface, now.UTC().Format(time.RFC3339))

	switch kind {
	case trailerCreatedBy:
		if evidence != "" {
			return "", fmt.Errorf("tasks: a %s trailer has no evidence field", trailerCreatedBy)
		}
		return head, nil
	case trailerClosedBy:
		safe, err := sanitizeEvidence(evidence)
		if err != nil {
			return "", err
		}
		return head + " — " + safe, nil
	default:
		return "", fmt.Errorf("tasks: unknown trailer kind %s", termsafe.QuoteArgMax(kind, 0))
	}
}

// defaultCloser returns the fallback closer name for surface, and refuses a
// surface that is not one of the two this client writes. The surface is
// written into the trailer verbatim, so an unchecked value would be a third
// caller-supplied field with no sanitizer of its own.
func defaultCloser(surface string) (string, error) {
	switch surface {
	case SurfaceMCP:
		return defaultCloserMCP, nil
	case SurfaceDone:
		return defaultCloserDone, nil
	default:
		return "", fmt.Errorf("tasks: trailer surface must be %q or %q, got %s",
			SurfaceMCP, SurfaceDone, termsafe.QuoteArgMax(surface, 0))
	}
}

// sanitizeCloser reduces a self-declared name to one trailer field. The name
// comes from an MCP client's `initialize` or a CLI flag: untrusted, and
// written onto a shared board.
//
// It is an allowlist, not a list of characters to remove. Everything outside
// [A-Za-z0-9._()/@ -] is dropped, which takes out every line break, control,
// invisible character, and piece of markup without having to name them, and
// takes out ':' and '—' with them — the two characters a trailer's timestamp
// and evidence separator need, so a name cannot spell out fields of its own.
// The word "via" is dropped as well: it is the separator between the closer
// and the rest of the line, and a name that kept it would read as a shorter
// name followed by forged fields.
//
// A name with nothing left becomes fallback. So does a name that holds
// anything shaped like an API token: the allowlist would drop the underscore
// and keep the hex, and the hex is the credential. The test is on the name as
// declared, before the allowlist can break the shape it looks for.
func sanitizeCloser(closer, fallback string) string {
	if evidenceTokenRe.MatchString(closer) {
		return fallback
	}
	var kept strings.Builder
	for _, r := range closer {
		if isCloserRune(r) {
			kept.WriteRune(r)
		}
	}
	// Fields also collapses runs of spaces and trims the ends, so the result
	// has single spaces only.
	words := strings.Fields(kept.String())
	out := words[:0]
	for _, word := range words {
		if word != "via" {
			out = append(out, word)
		}
	}
	name := strings.Join(out, " ")
	// Every kept rune is one byte, so a byte cut is a rune cut.
	if len(name) > maxCloserRunes {
		name = strings.TrimRight(name[:maxCloserRunes], " ")
		// The cut can shorten a longer last word to exactly "via", which the
		// word filter above has already passed. Only the last word can be
		// affected, so only the end is rechecked.
		for name == "via" || strings.HasSuffix(name, " via") {
			name = strings.TrimRight(strings.TrimSuffix(name, "via"), " ")
		}
	}
	if name == "" {
		return fallback
	}
	return name
}

func isCloserRune(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("._()/@ -", r)
}

// ValidateEvidence reports why evidence would be refused as a closed-by
// trailer's evidence field, or nil. It is the check a close makes itself; a
// caller runs it first to refuse a bad argument before it reads a credential.
// The refusal never repeats the evidence.
func ValidateEvidence(evidence string) error {
	_, err := sanitizeEvidence(evidence)
	return err
}

// sanitizeEvidence validates the evidence field and returns it ready to
// append. Unlike the closer, evidence is refused rather than repaired: it is
// the record of why a task was closed, and a silently altered record is worse
// than a refused close the caller can retry with clean text.
//
// The rune test is the union of three classifiers. !IsGraphic takes every
// control (CR, LF, tab, U+000B, U+000C, U+0085), the line and paragraph
// separators, the bidi and zero-width format characters, and unassigned and
// private-use code points. IsInvisibleRune adds the runes Unicode calls
// graphic that still render as nothing or as a blank: variation selectors,
// the Hangul fillers, U+2800. IsUnsafeTerminalRune is a subset of !IsGraphic
// today and is named anyway, so the set every terminal sink in this repo
// refuses stays refused here if either definition moves. Any of these in a
// single-line field either starts a second line in some renderer or hides
// text from a reader.
func sanitizeEvidence(evidence string) (string, error) {
	if !utf8.ValidString(evidence) {
		return "", fmt.Errorf("tasks: evidence is not valid UTF-8")
	}
	position := 0
	for _, r := range evidence {
		position++
		if termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r) || !unicode.IsGraphic(r) {
			return "", fmt.Errorf("tasks: evidence must be one line of visible text, and character %d is a line break, "+
				"a control, or an invisible character", position)
		}
	}
	evidence = strings.TrimSpace(evidence)
	if evidence == "" {
		return "", fmt.Errorf("tasks: evidence is required and must not be blank")
	}
	if n := utf8.RuneCountInString(evidence); n > maxEvidenceRunes {
		return "", fmt.Errorf("tasks: evidence is %d characters, over the %d limit", n, maxEvidenceRunes)
	}
	if evidenceTokenRe.MatchString(evidence) {
		return "", fmt.Errorf("tasks: evidence holds something shaped like an API token, and a task description is readable by everyone the project is shared with")
	}
	// The board renders a description as markup. The replacer makes one pass
	// over the input, so the '&' of an entity it writes is not rewritten.
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(evidence), nil
}
