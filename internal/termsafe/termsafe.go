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
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
		safe.WriteString(safeRune(r))
	}
	return safe.String()
}

// TruncatedMarker ends a SafeLineMax result that dropped text, so a reader
// can tell a capped value from one that simply ended there.
const TruncatedMarker = " … [truncated]"

// SafeLineMax is SafeLine capped at maxRunes runes of OUTPUT, for a sink
// whose value can be arbitrarily long (#506). The cap counts escaped runes,
// so a string of controls cannot expand past it, and it cuts only between
// whole escapes: a \u202e is kept or dropped entire, never split into text
// that reads as something else. A cut value ends in TruncatedMarker, which
// is not counted against maxRunes. maxRunes < 1 means no cap.
func SafeLineMax(s string, maxRunes int) string {
	if maxRunes < 1 {
		return SafeLine(s)
	}
	var safe strings.Builder
	used := 0
	for _, r := range s {
		piece := safeRune(r)
		n := utf8.RuneCountInString(piece)
		if used+n > maxRunes {
			safe.WriteString(TruncatedMarker)
			return safe.String()
		}
		safe.WriteString(piece)
		used += n
	}
	return safe.String()
}

// safeRune is SafeLine's per-rune rule: a safe graphic rune as itself, any
// other rune as its Go graphic escape without the surrounding quotes.
func safeRune(r rune) string {
	if !IsUnsafeTerminalRune(r) && unicode.IsGraphic(r) {
		return string(r)
	}
	quoted := strconv.QuoteRuneToGraphic(r)
	if len(quoted) >= 2 {
		return quoted[1 : len(quoted)-1]
	}
	return quoted
}

// QuoteText visibly quotes an untrusted text field without allowing it to
// contribute terminal controls. Unlike applying %q after SafeLine, it escapes
// each original rune exactly once, so a newline is shown as \n rather than
// the more confusing \\n.
func QuoteText(text string) string {
	return strconv.QuoteToGraphic(text)
}

// ArgEchoMaxRunes is the input budget for echoing a rejected command-line
// argument back to the operator (#562): enough to show a mistyped ref, host,
// or owner in full, never enough to flood a terminal or a log.
const ArgEchoMaxRunes = 80

// argEchoEllipsis marks a QuoteArgMax result whose input was cut. It sits
// OUTSIDE the closing quote, so it cannot be mistaken for input text.
const argEchoEllipsis = "…"

// QuoteArgMax is QuoteText over at most maxRunes runes of s, followed by an
// ellipsis when s was longer. It is the echo form for a rejected value the
// operator chose, where showing it back is the whole diagnostic: an argument
// just typed on the command line, or a value in their own config.toml named
// by a validation error (#706). It is NOT for text a subprocess, a server, or
// a file forgectl did not ask the operator to write supplies: those get a
// categorical error that never renders them, because nobody in front of the
// terminal chose that text (#562).
//
// The cut counts INPUT runes, before escaping, so it never splits an escape;
// escaping can lengthen the output (at most 10 bytes per rune, for \U0010ffff),
// which keeps it bounded by the input budget. Invalid UTF-8 counts one rune
// per bad byte, as range does. maxRunes < 1 means ArgEchoMaxRunes.
func QuoteArgMax(s string, maxRunes int) string {
	if maxRunes < 1 {
		maxRunes = ArgEchoMaxRunes
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return QuoteText(s[:i]) + argEchoEllipsis
		}
		n++
	}
	return QuoteText(s)
}

// QuotePath is QuoteText named for filesystem sinks, where the surrounding
// quotes also keep spaces and path boundaries legible. It is capped at
// PathEchoMaxRunes input runes (QuotePathMax), because a path is often clone-,
// config-, or server-derived and nobody at the terminal chose its length
// (#832). A caller that must render the whole path, such as a
// machine-parseable field, quotes with QuoteText instead.
func QuotePath(path string) string {
	return QuotePathMax(path, 0)
}

// PathEchoMaxRunes is the input budget for a path QuotePath renders (#821,
// #832). It is larger than ArgEchoMaxRunes because an ordinary worktree or
// clone path runs past 80 runes; 512 shows any realistic path in full and
// still bounds a hostile PATH_MAX-long one.
const PathEchoMaxRunes = 512

// QuotePathMax quotes path like QuoteText, keeping at most maxRunes of its
// input runes. A longer path is cut in the MIDDLE, because the part that
// identifies a file is its name at the end (#832): the result is the quoted
// head, an ellipsis, and the quoted tail, the ellipsis sitting between the
// two quotes so it cannot be read as path text. The tail is the final path
// element (from its separator on, trailing separators included) when that
// fits in three quarters of the budget, and the last half of the budget
// otherwise; the head gets the rest.
//
// The cut counts INPUT runes, before escaping, so it never splits an escape.
// Invalid UTF-8 counts one rune per bad byte, as range does. maxRunes < 1
// means PathEchoMaxRunes.
func QuotePathMax(path string, maxRunes int) string {
	if maxRunes < 1 {
		maxRunes = PathEchoMaxRunes
	}
	// A path holds at least one byte per rune, so one no longer in bytes than
	// the budget fits it, and skips the rune-offset slice below.
	if len(path) <= maxRunes {
		return QuoteText(path)
	}
	// starts[i] is the byte offset of input rune i, as range yields them.
	starts := make([]int, 0, len(path))
	for i := range path {
		starts = append(starts, i)
	}
	total := len(starts)
	if total <= maxRunes {
		return QuoteText(path)
	}
	tail := maxRunes / 2
	// Trailing separators belong to the final element, so a directory path
	// ending in `/` keeps its name rather than a bare "/".
	if sep := strings.LastIndexAny(strings.TrimRight(path, `/\`), `/\`); sep >= 0 {
		// The separator is ASCII, so it starts a rune; count the runes from it.
		elem := total - sort.SearchInts(starts, sep)
		if elem <= maxRunes-maxRunes/4 {
			tail = elem
		}
	}
	head := maxRunes - tail
	if tail == 0 {
		return QuoteText(path[:starts[head]]) + argEchoEllipsis
	}
	return QuoteText(path[:starts[head]]) + argEchoEllipsis + QuoteText(path[starts[total-tail]:])
}

// QuotePathIfUnsafe returns path verbatim when quoting would have changed
// nothing but the surrounding quotes, and the full, uncapped QuoteText
// escaping otherwise.
//
// It exists for sinks whose output is BOTH rendered to a terminal and a
// machine-parseable field, used at the `pr` listing, findings, queue, and
// repair rows. The documented one is `forgectl pr list` field 3, which
// `pr teardown` is fed. Unconditional quoting there would rewrite every
// ordinary row and break callers parsing it; printing raw would let a planted
// breadcrumb filename drive the reader's terminal. Quoting only the paths that
// need it keeps both properties, and the test for "needs it" is the escaping
// itself rather than a second, drift-prone predicate.
//
// Prefer plain QuotePath on any sink that is human-only.
func QuotePathIfUnsafe(path string) string {
	// QuoteText, not the capped QuotePath: its callers print machine-parseable
	// fields, and a cut would rewrite a long but ordinary path in them (#832).
	if quoted := QuoteText(path); quoted != `"`+path+`"` {
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

// Categorical returns an error whose text is exactly message and whose unwrap
// chain is cause. It is the #562 form for a failure whose cause text carries
// something nobody at the terminal chose: a subprocess's stderr (gh, git, tea
// print what the server or transport sends), a server-supplied URL, or a
// config value. The message never renders cause; errors.Is and errors.As
// still reach it, so a caller's disposition (a missing binary, a canceled
// context) keeps working. Log the cause at the call site if it is worth
// keeping. message must be a fixed string, not built from untrusted text.
func Categorical(message string, cause error) error {
	return safeError{message: message, cause: cause}
}

// Error converts a nested filesystem/config error into terminal-safe text
// while preserving its unwrap chain for errors.Is/errors.As disposition.
// Known filesystem errors are reconstructed from individually escaped fields
// so a raw path can never be reinserted by their native Error method, and each
// path is capped as QuotePath caps it. A *PathError or *LinkError wrapped
// inside another error (a fmt.Errorf %w) is found in the chain and its span of
// the message is rendered the same way when its path is over the cap (#837);
// that relies on the wrapper embedding the native text verbatim, as %w does.
// Converting it with Error before wrapping it (#832) remains the reliable form.
//
// An Error method that panics gets errTextUnavailable in place of its text,
// and the rest of the message still renders; see errorText.
func Error(err error) error {
	if err == nil {
		return nil
	}
	var message string
	// A typed-nil *LinkError or *PathError is a non-nil error whose fields
	// cannot be read (forgectl#794); it falls through to errorText, whose
	// recover turns its panicking Error method into errTextUnavailable.
	if linkErr, ok := err.(*os.LinkError); ok && linkErr != nil {
		message = fmt.Sprintf("%s %s %s: %s", SafeLine(linkErr.Op), QuotePathMax(linkErr.Old, 0), QuotePathMax(linkErr.New, 0), causeText(linkErr.Err))
	} else if pathErr, ok := err.(*os.PathError); ok && pathErr != nil {
		message = fmt.Sprintf("%s %s: %s", SafeLine(pathErr.Op), QuotePathMax(pathErr.Path, 0), causeText(pathErr.Err))
	} else {
		message = capWrappedPaths(errorText(err), overlongPathErrors(err))
	}
	return safeError{message: message, cause: err}
}

// causeText renders the Err field of a *PathError or *LinkError as Error
// renders any error, so a path error nested there is capped too rather than
// echoed whole by its native Error method (#845). A nil Err keeps errorText's
// fallback, since the native Error method would panic on it.
func causeText(err error) string {
	if err == nil {
		return SafeLine(errorText(err))
	}
	return Error(err).Error()
}

// chainWalkBudget bounds how many errors overlongPathErrors visits in one
// chain. The depth bound alone stops a cycle through Unwrap() error, but a
// cycle through Unwrap() []error fans out, and a fan-out-2 cycle 100 deep is
// 2^100 visits (#845). It is far above any chain forgectl builds: an
// errors.Join of thousands of path errors still fits.
const chainWalkBudget = 10000

// overlongPathErrors returns every *PathError and *LinkError in err's chain,
// err itself excluded, whose path QuotePath would cut, in chain order. It
// follows both Unwrap forms, so an errors.Join branch is searched too.
//
// An Unwrap method can panic as an Error method can: a typed-nil *PathError
// dereferences its receiver (forgectl#794). The walk then stops and keeps
// what it found, and the message renders as errorText gives it.
func overlongPathErrors(err error) (found []error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Unwrap method panicked; the path-cap walk stopped.",
				"error_type", fmt.Sprintf("%T", err), "panic_type", fmt.Sprintf("%T", r))
		}
	}()
	budget := chainWalkBudget
	var walk func(e error, depth int)
	walk = func(e error, depth int) {
		// The depth bound stops a cyclic Unwrap() error from recursing
		// forever; the node budget stops a fan-out cycle through
		// Unwrap() []error, which the depth bound alone lets grow
		// exponentially. A chain past either keeps what was found so far.
		if e == nil || depth > 100 || budget <= 0 {
			return
		}
		budget--
		if depth > 0 {
			if linkErr, ok := e.(*os.LinkError); ok && linkErr != nil {
				if pathOverCap(linkErr.Old) || pathOverCap(linkErr.New) {
					found = append(found, e)
				}
			} else if pathErr, ok := e.(*os.PathError); ok && pathErr != nil && pathOverCap(pathErr.Path) {
				found = append(found, e)
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap(), depth+1)
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner, depth+1)
			}
		}
	}
	walk(err, 0)
	return found
}

// pathOverCap reports whether QuotePath would cut path. The byte length is a
// cheap upper bound on the rune count, so most paths never count runes.
func pathOverCap(path string) bool {
	return len(path) > PathEchoMaxRunes && utf8.RuneCountInString(path) > PathEchoMaxRunes
}

// capWrappedPaths is SafeLine(message), except that each span of it that
// renders the native text of one of pathErrs becomes that error's capped
// Error form instead (#837). fmt.Errorf's %w writes the wrapped error's
// Error() text verbatim, so a wrapper that composed its message that way
// still holds the span to find; a wrapper that did not leaves no span, and
// its text renders as SafeLine renders it.
//
// The spans are found in the SafeLine form of message, matched against the
// SafeLine form of each native text, so a raw copy that only escapes to a
// native text is capped on the first pass too (#845). Every occurrence of
// each native text is capped; where two overlap, the one earlier in chain
// order wins. See findSpans for the cost.
//
// A second pass over the result changes nothing: the output is already
// SafeLine-inert, and every matched span was replaced by a cut, quoted form
// that no longer holds the whole path. The one exception is a path whose own
// text contains another over-cap error's capped rendering, quotes and
// ellipsis included, which a second pass may cap again. That output is still
// safe to print; only the cap can differ.
func capWrappedPaths(message string, pathErrs []error) string {
	escaped := SafeLine(message)
	var needles []string
	var owners []error
	searched := make(map[string]bool, len(pathErrs))
	for _, pathErr := range pathErrs {
		native := errorText(pathErr)
		if native == errTextUnavailable || native == "" {
			continue
		}
		// A cyclic chain can list one error thousands of times; dedupe on
		// its native text before paying for SafeLine.
		if !searched[native] {
			searched[native] = true
			needles = append(needles, SafeLine(native))
			owners = append(owners, pathErr)
		}
	}
	type span struct {
		start, end int
		with       string
	}
	var spans []span // accepted, sorted by start, never overlapping
	renders := make([]string, len(needles))
	for _, m := range findSpans(escaped, needles) {
		start, end := m.start, m.start+len(needles[m.needle])
		at := sort.Search(len(spans), func(k int) bool { return spans[k].start >= start })
		if (at > 0 && spans[at-1].end > start) || (at < len(spans) && spans[at].start < end) {
			continue
		}
		if renders[m.needle] == "" {
			renders[m.needle] = Error(owners[m.needle]).Error()
		}
		spans = append(spans, span{})
		copy(spans[at+1:], spans[at:])
		spans[at] = span{start: start, end: end, with: renders[m.needle]}
	}
	if len(spans) == 0 {
		return escaped
	}
	var out strings.Builder
	out.Grow(len(escaped))
	pos := 0
	for _, s := range spans {
		out.WriteString(escaped[pos:s.start])
		out.WriteString(s.with)
		pos = s.end
	}
	out.WriteString(escaped[pos:])
	return out.String()
}

// spanMatch is one occurrence of needles[needle] in a text, at byte start.
type spanMatch struct{ needle, start int }

// findSpans returns the occurrences of each needle in text, ordered by needle
// index and then by start. A needle's own occurrences never overlap: a match
// resumes the search for that needle after its end, as a strings.Index loop
// would.
//
// It is one Rabin-Karp scan of text over a window as long as the shortest
// needle, so the cost is linear in len(text) rather than a scan per needle
// (#845): an errors.Join of 2000 over-cap path errors shares a long
// "open /…" prefix, which made per-needle strings.Index re-compare it at
// every candidate. A window hit is confirmed by comparing the whole needle,
// so a hash collision costs time, never a wrong match; needles sharing their
// entire first window are the case that degrades toward a scan per needle.
func findSpans(text string, needles []string) []spanMatch {
	if len(needles) == 0 {
		return nil
	}
	window := len(needles[0])
	for _, n := range needles[1:] {
		window = min(window, len(n))
	}
	if window == 0 || window > len(text) {
		return nil
	}
	const prime = 16777619 // the multiplier Go's strings package uses for Rabin-Karp
	var pow uint32 = 1     // prime^window, to drop the byte leaving the window
	for range window {
		pow *= prime
	}
	hashOf := func(s string) uint32 {
		var h uint32
		for i := range len(s) {
			h = h*prime + uint32(s[i])
		}
		return h
	}
	byHash := make(map[uint32][]int, len(needles))
	for i, n := range needles {
		h := hashOf(n[:window])
		byHash[h] = append(byHash[h], i)
	}
	nextFree := make([]int, len(needles)) // where needle i may next match
	var matches []spanMatch
	h := hashOf(text[:window])
	for pos := 0; ; pos++ {
		for _, i := range byHash[h] {
			if pos >= nextFree[i] && strings.HasPrefix(text[pos:], needles[i]) {
				matches = append(matches, spanMatch{needle: i, start: pos})
				nextFree[i] = pos + len(needles[i])
			}
		}
		if pos+window >= len(text) {
			break
		}
		h = h*prime + uint32(text[pos+window]) - pow*uint32(text[pos])
	}
	sort.SliceStable(matches, func(a, b int) bool { return matches[a].needle < matches[b].needle })
	return matches
}

// errTextUnavailable is the text Error gives an error whose Error method
// panicked. It says what happened without repeating the panic value.
const errTextUnavailable = "error text unavailable: its Error method panicked"

// errorText is err.Error(), or errTextUnavailable when that call panics.
//
// Go 1.26.0's os.RemoveAll and os.Root.RemoveAll can return a *PathError
// wrapping the runtime's internal errSymlink when a same-uid racer swaps a
// directory for a symlink mid-walk, and errSymlink's Error method is a panic
// (forgectl#783, #764). go.mod's go directive now requires a patch that maps
// it away, but this is the boundary every filesystem error crosses on its way
// to the terminal, so it does not rely on the toolchain alone. fmt and slog
// recover such a panic; a direct call does not.
//
// The cause stays in the chain Error returns, so errors.Is and errors.As keep
// working. A caller that unwraps to it and calls its Error method directly
// reintroduces the panic.
//
// The recovery leaves a Debug trace naming the error's and the panic value's
// Go types, never the panic value itself, which may carry the text the
// fallback exists to withhold (forgectl#794).
func errorText(err error) (text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Error method panicked; its text is withheld.",
				"error_type", fmt.Sprintf("%T", err), "panic_type", fmt.Sprintf("%T", r))
			text = errTextUnavailable
		}
	}()
	return err.Error()
}
