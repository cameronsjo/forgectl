package exec

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"
)

// minScrubLen is the length from which a bare value is scrubbed wherever it
// appears. A shorter one is scrubbed only where it stands as a whole word:
// replacing every "1" or "grpc" inside a tmux error would mangle it, but a
// short value echoed on its own must still not survive.
const minScrubLen = 8

type maskKey struct{}

// argMask is what WithMaskedAssignments stores: each marked KEY=VALUE argv
// element mapped to its display form, plus the bare values to scrub from
// stderr.
type argMask struct {
	shown map[string]string
	// pats is every string text scrubs, whole entries and bare values
	// together, sorted longest first (entries before values at equal length),
	// so the first pattern that matches at a position is the longest one.
	pats []maskPat
	// values is the bare values, for straddleLen.
	values []string
}

// maskPat is one string text scrubs and what a run starting with it renders
// as: KEY=[redacted] for a whole entry, so an echoed entry keeps its key, and
// [redacted] for a bare value.
type maskPat struct {
	text, shown string
}

// WithMaskedAssignments returns a context under which the Runner does not
// render the VALUE of any of these KEY=VALUE argv elements. It covers the
// three places a Runner writes argv down: the debug log, *CommandError's text,
// and the stderr and failure-path stdout that CommandError keeps (and, for
// stderr, the log line). Each shows as KEY=[redacted]. Stdout returned on
// success is not scrubbed, and a value shorter than minScrubLen is scrubbed
// only where it stands as a whole word.
//
// The child still receives the real value. What this does NOT cover is the
// process table: argv is readable through ps for the life of the process, so
// this is "will not be written down", not "safe for a secret" — the same
// limit the sensitive seam states. It exists for callers such as tmux
// new-window -e, whose values belong on argv but not in a log file (#529).
func WithMaskedAssignments(ctx context.Context, entries []string) context.Context {
	if len(entries) == 0 {
		return ctx
	}
	m := argMask{shown: make(map[string]string, len(entries))}
	for _, e := range entries {
		key, value, ok := strings.Cut(e, "=")
		if !ok || value == "" {
			continue
		}
		m.shown[e] = key + "=" + Redacted
		m.pats = append(m.pats, maskPat{text: e, shown: m.shown[e]}, maskPat{text: value, shown: Redacted})
		m.values = append(m.values, value)
	}
	sort.Slice(m.pats, func(i, j int) bool {
		a, b := m.pats[i], m.pats[j]
		if len(a.text) != len(b.text) {
			return len(a.text) > len(b.text)
		}
		// An entry and a value of equal length and text: the entry, so the
		// key shows. Then lexical, so the order never depends on input order.
		if a.text != b.text {
			return a.text < b.text
		}
		return a.shown != Redacted && b.shown == Redacted
	})
	longestFirst(m.values)
	return context.WithValue(ctx, maskKey{}, m)
}

func maskFrom(ctx context.Context) argMask {
	m, _ := ctx.Value(maskKey{}).(argMask)
	return m
}

// args returns argv as it may be rendered. It copies only when something is
// masked, so the unmasked path renders exactly as before.
func (m argMask) args(args []string) []string {
	if len(m.shown) == 0 {
		return args
	}
	out := make([]string, len(args))
	for i, a := range args {
		if s, ok := m.shown[a]; ok {
			a = s
		}
		out[i] = a
	}
	return out
}

// text scrubs every marked entry and value from s in one left-to-right pass.
//
// Masked values can overlap in the text (the end of one is the start of
// another), so replacing one value at a time is not enough: whichever goes
// first consumes the shared bytes, and the other no longer matches, leaving
// its unshared part in the clear (#661). text instead looks, at every byte,
// for any pattern that starts there, and treats the bytes each match covers as
// one run: a match that starts inside the open run and ends past it extends
// the run. Each run is replaced once, by the display form of the pattern it
// started with, so an echoed entry keeps its key (KEY=[redacted]) and
// everything else in the run becomes part of that one [redacted].
//
// A pattern shorter than minScrubLen counts only where it stands as a whole
// word: not glued to a word character on either side. The check applies only
// at an edge where the pattern itself starts or ends with a word character, so
// a value like "a:b" is still found next to a letter. A byte before the match
// that is already inside a run does not count as a word character, since it
// is about to become [redacted].
//
// Cost is linear in s for realistic text. Its worst case is a stream that
// repeats a long masked value byte for byte, where each position compares up
// to that value's length; stdout is capped at maxStdoutBytes and stderr at
// maxStderrTail, so that is bounded, and the stream is the failing child's
// own output.
func (m argMask) text(s string) string {
	if len(m.pats) == 0 {
		return s
	}
	// Patterns by first byte, longest first within each, so most bytes of a
	// long stream cost one table lookup.
	var byFirst [256][]maskPat
	for _, p := range m.pats {
		byFirst[p.text[0]] = append(byFirst[p.text[0]], p)
	}
	var b strings.Builder
	last := 0          // end of the text already copied to b
	open := false      // whether a run is being extended
	start, end := 0, 0 // the open run, or the last closed one
	shown := ""        // what the open run renders as
	closeRun := func() {
		b.WriteString(s[last:start])
		b.WriteString(shown)
		last = end
		open = false
	}
	for i := 0; i < len(s); i++ {
		if open && i >= end {
			closeRun()
		}
		for _, p := range byFirst[s[i]] {
			pend := i + len(p.text)
			if open && pend <= end {
				// Longest first: neither this match nor a shorter one at i
				// could reach past the open run.
				break
			}
			if !strings.HasPrefix(s[i:], p.text) {
				continue
			}
			if len(p.text) < minScrubLen {
				// The byte before is covered iff the open or last closed run
				// reaches it. The byte after never is: this match ends past
				// the open run, and no later run has started yet.
				gluedBefore := i > 0 && i-1 >= end && isWordByte(p.text[0]) && isWordByte(s[i-1])
				gluedAfter := pend < len(s) && isWordByte(p.text[len(p.text)-1]) && isWordByte(s[pend])
				if gluedBefore || gluedAfter {
					continue
				}
			}
			if !open {
				open, start, shown = true, i, p.shown
			}
			end = pend
			// Longest first: no later pattern at i ends further out.
			break
		}
	}
	if open {
		closeRun()
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// straddleLen returns how many leading bytes of s to drop so that no masked
// value is split at the start of what remains. s is the tail of a longer
// stream, so a value that began before the cut shows up here only as its end:
// a proper suffix of the value, which text can no longer match. straddleLen
// drops the longest such suffix s starts with, then checks the new start
// again, because values can overlap in the stream and removing one fragment
// can expose the end of another. It works on the raw bytes, before masking,
// so no replacement text can shift where a fragment ends. Every drop removes
// at least one byte, so the loop ends. A coincidental match (the stream
// happens to start with the last byte of some value) only drops a few more
// bytes of a tail that is already cut, which is harmless.
func (m argMask) straddleLen(s string) int {
	total := 0
	for {
		n := 0
		for _, v := range m.values {
			for l := min(len(v)-1, len(s)); l > n; l-- {
				if strings.HasPrefix(s, v[len(v)-l:]) {
					n = l
					break
				}
			}
		}
		// Neither the cut nor a drop respects rune boundaries; do not start
		// the text on a continuation byte.
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n++
		}
		if n == 0 {
			return total
		}
		s = s[n:]
		total += n
	}
}

// longestFirst sorts in place by descending length, ties in lexical order so
// the result does not depend on input order.
func longestFirst(xs []string) {
	sort.Slice(xs, func(i, j int) bool {
		if len(xs[i]) != len(xs[j]) {
			return len(xs[i]) > len(xs[j])
		}
		return xs[i] < xs[j]
	})
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
