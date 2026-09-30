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
	// so runShown finds the longest entry that starts a run first.
	pats []maskPat
	// values is the bare values, and entries the whole KEY=VALUE entries,
	// for straddleLen.
	values  []string
	entries []string
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
		m.entries = append(m.entries, e)
	}
	return context.WithValue(ctx, maskKey{}, m.sorted())
}

// sorted orders pats and values the way text and straddleLen need them, in
// place, and returns m.
func (m argMask) sorted() argMask {
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
	return m
}

// withValues returns a copy of m that also scrubs each of values as a bare
// value, the way it scrubs a masked assignment's VALUE. m itself, which may
// be shared through a context, is not modified. An empty value is skipped,
// and so is a short one with no word byte in it: the whole-word rule cannot
// bound such a value, so "." or ":" would be scrubbed at every occurrence and
// leave nothing of the text readable.
func (m argMask) withValues(values []string) argMask {
	seen := make(map[string]bool, len(values))
	var add []string
	for _, v := range values {
		if v == "" || seen[v] || (len(v) < minScrubLen && !hasWordByte(v)) {
			continue
		}
		seen[v] = true
		add = append(add, v)
	}
	if len(add) == 0 {
		return m
	}
	out := argMask{
		shown:   m.shown,
		pats:    append(make([]maskPat, 0, len(m.pats)+len(add)), m.pats...),
		values:  append(make([]string, 0, len(m.values)+len(add)), m.values...),
		entries: m.entries,
	}
	for _, v := range add {
		out.pats = append(out.pats, maskPat{text: v, shown: Redacted})
		out.values = append(out.values, v)
	}
	return out.sorted()
}

func hasWordByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if isWordByte(s[i]) {
			return true
		}
	}
	return false
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

// text scrubs every marked entry and value from s.
//
// Masked values can overlap in the text (the end of one is the start of
// another), so replacing one value at a time is not enough: whichever goes
// first consumes the shared bytes, and the other no longer matches, leaving
// its unshared part in the clear (#661). text works in two phases instead.
// cover finds every byte that lies inside a qualifying match of any pattern,
// overlapping matches included; text then replaces each maximal run of
// covered bytes once. A run that starts with a whole entry renders as
// KEY=[redacted], so an echoed entry keeps its key; any other run renders as
// [redacted].
func (m argMask) text(s string) string {
	if len(m.pats) == 0 {
		return s
	}
	covered := m.cover(s)
	if covered == nil {
		return s
	}
	var b strings.Builder
	last := 0 // end of the text already copied to b
	for i := 0; i < len(s); {
		if !covered.has(i) {
			i++
			continue
		}
		end := i + 1
		for end < len(s) && covered.has(end) {
			end++
		}
		b.WriteString(s[last:i])
		b.WriteString(m.runShown(s[i:end]))
		last, i = end, end
	}
	b.WriteString(s[last:])
	return b.String()
}

// runShown is what one covered run renders as: KEY=[redacted] when a whole
// entry starts the run and lies inside it (the longest such entry), else
// [redacted]. The key is the only text shown, and the argv rendering already
// shows it.
func (m argMask) runShown(run string) string {
	for _, p := range m.pats {
		if p.shown != Redacted && strings.HasPrefix(run, p.text) {
			return p.shown
		}
	}
	return Redacted
}

// cover marks every byte of s that lies inside a qualifying match of any
// pattern, and returns nil when nothing matched.
//
// Every occurrence counts, including ones that overlap each other or
// themselves. A pattern of minScrubLen or more always qualifies. A shorter one
// qualifies only where it stands as a whole word: not glued to a word
// character on either side. The check applies only at an edge where the
// pattern itself starts or ends with a word character, so a value like "a:b"
// is still found next to a letter, and a neighboring byte that is already
// covered is not a word character, since it is about to become [redacted].
// Covering a short match can therefore qualify another one beside it, on
// either side, and that one can qualify the next: the result is the fixpoint.
//
// Cost is linear in len(s) for each pattern (#708):
//
//   - A long pattern is found with strings.Index, which skips a stretch with
//     no match quickly; after a match, a KMP automaton carries on until the
//     partial match dies, so a stream that repeats a value overlapping itself
//     never re-verifies the shared bytes. That was O(len(s)·len(value)).
//   - The short patterns get one full scan each. After that, each byte that
//     becomes covered is pushed on a worklist exactly once, and popping it
//     re-checks only the short matches that end right before it or start right
//     after it: the only ones whose qualification it can change. The
//     rescan-until-nothing-changes loop this replaced needed one full pass per
//     link of a cascade, which is quadratic.
//
// Linear per pattern still means the whole pass is O(patterns × len(s)), and
// maskFor adds one short pattern for every withheld argv value under
// minScrubLen (#816). Measured on 64 KiB of stderr: 1000 short values take
// about 0.8 s, and 5000 take about 4.5 s. The count is deliberately not
// capped: a value past a cap would go back into the error text unscrubbed,
// and the scrub's correctness is the point. The cost is paid only on the
// failure path (stderr and failure stdout are captured and bounded before
// this runs), and an argv that long is already near the OS's ARG_MAX.
func (m argMask) cover(s string) bitset {
	var covered bitset
	ensure := func() {
		if covered == nil {
			covered = newBitset(len(s))
		}
	}
	for _, p := range m.pats {
		if len(p.text) < minScrubLen {
			continue
		}
		done := 0 // bytes of this pattern's earlier matches already marked
		eachMatch(s, p.text, func(i int) {
			ensure()
			end := i + len(p.text)
			covered.set(max(i, done), end)
			done = end
		})
	}

	var short []string
	for _, p := range m.pats {
		if len(p.text) < minScrubLen {
			short = append(short, p.text)
		}
	}
	if len(short) == 0 {
		return covered
	}
	isWord := func(i int) bool { return isWordByte(s[i]) && !covered.has(i) }
	qualifies := func(p string, i int) bool {
		end := i + len(p)
		gluedBefore := i > 0 && isWordByte(p[0]) && isWord(i-1)
		gluedAfter := end < len(s) && isWordByte(p[len(p)-1]) && isWord(end)
		return !gluedBefore && !gluedAfter
	}
	// work holds spans of bytes newly covered by a short match and not yet
	// examined. It is drained after every short mark, so it stays as deep as
	// the current cascade, not as long as the stream.
	type span struct{ from, to int }
	var work []span
	mark := func(from, to int) {
		ensure()
		for i := from; i < to; {
			if covered.has(i) {
				i++
				continue
			}
			j := i
			for j < to && !covered.has(j) {
				j++
			}
			covered.set(i, j)
			work = append(work, span{i, j})
			i = j
		}
	}
	// try marks p at i when it occurs there and qualifies.
	try := func(p string, i int) {
		if i < 0 || i+len(p) > len(s) || s[i:i+len(p)] != p || !qualifies(p, i) {
			return
		}
		mark(i, i+len(p))
	}
	drain := func() {
		for len(work) > 0 {
			sp := work[len(work)-1]
			work = work[:len(work)-1]
			for k := sp.from; k < sp.to; k++ {
				for _, p := range short {
					try(p, k-len(p)) // ends right before k: k was its after byte
					try(p, k+1)      // starts right after k: k was its before byte
				}
			}
		}
	}
	for _, p := range short {
		eachMatch(s, p, func(i int) {
			if qualifies(p, i) {
				mark(i, i+len(p))
				drain()
			}
		})
	}
	return covered
}

// eachMatch calls fn with the start of every occurrence of p in s, overlapping
// ones included, in increasing order. strings.Index finds each first match,
// skipping a gap with no match quickly; from there a KMP automaton carries the
// partial match forward until it dies, so overlapping matches cost O(1)
// amortized each instead of a full re-verification of p.
func eachMatch(s, p string, fn func(i int)) {
	var fail []int // KMP failure function, built on the first match
	for from := 0; from+len(p) <= len(s); {
		rel := strings.Index(s[from:], p)
		if rel < 0 {
			return
		}
		i := from + rel
		fn(i)
		if fail == nil {
			fail = kmpFailure(p)
		}
		q := fail[len(p)-1] // p's longest proper border: the partial match carried on
		j := i + len(p)
		for q > 0 && j < len(s) {
			for q > 0 && s[j] != p[q] {
				q = fail[q-1]
			}
			if s[j] == p[q] {
				q++
			}
			j++
			if q == len(p) {
				fn(j - len(p))
				q = fail[q-1]
			}
		}
		// The loop ended with q == 0 (no partial match straddles j) or at
		// the end of s, so the next match starts at j or later.
		from = j
	}
}

// kmpFailure returns, for each prefix p[:k+1], the length of its longest
// proper prefix that is also a suffix.
func kmpFailure(p string) []int {
	fail := make([]int, len(p))
	for k, q := 1, 0; k < len(p); k++ {
		for q > 0 && p[k] != p[q] {
			q = fail[q-1]
		}
		if p[k] == p[q] {
			q++
		}
		fail[k] = q
	}
	return fail
}

// bitset is one bit per byte of the text cover scans. A nil bitset has no
// bit set.
type bitset []uint64

func newBitset(n int) bitset { return make(bitset, (n+63)/64) }

func (b bitset) has(i int) bool { return b != nil && b[i/64]&(uint64(1)<<(i%64)) != 0 }

// set sets the bits in [from, to).
func (b bitset) set(from, to int) {
	for i := from; i < to; i++ {
		b[i/64] |= uint64(1) << (i % 64)
	}
}

// maxStraddleRounds bounds straddleLen's loop. Each round costs a scan of
// every masked value and entry, and a stream built to drop one byte per round
// (a tail of "b" with a masked value ending in "b") would otherwise run one
// round per byte of the 64 KiB tail (#749).
const maxStraddleRounds = 64

// straddleLen returns how many leading bytes of s to drop so that no masked
// value is split at the start of what remains. s is the tail of a longer
// stream, so a value that began before the cut shows up here only as its end:
// a proper suffix of the value, which text can no longer match. straddleLen
// drops the longest such suffix s starts with, then checks the new start
// again, because values can overlap in the stream and removing one fragment
// can expose the end of another.
//
// A cut inside an entry's KEY leaves the value whole, but it can still
// escape: a long entry is masked wherever it appears, while its short value
// alone qualifies only as a whole word, so "…EY=ab" followed by "cd" would
// show "ab" (#708). So a proper suffix of an entry that contains the entry's
// whole value is dropped too. It works on the raw bytes, before masking,
// so no replacement text can shift where a fragment ends. Every drop removes
// at least one byte, so the loop ends. A coincidental match (the stream
// happens to start with the last byte of some value, or with a whole value)
// only drops more of a tail that is already cut, which is harmless; a leading
// run of a repeated short value is dropped whole, because the loop repeats.
//
// Cost (#749): the longest suffix of each value or entry that starts s is
// found with one KMP pass over it, so a round is linear in the mask's size.
// Probing every suffix length with HasPrefix, as this once did, was quadratic
// in each value's length per round (24.8 s for a 4 KiB value against a 64 KiB
// tail that drops one byte per round). Rounds are capped at
// maxStraddleRounds: a tail that still starts with a fragment after that many
// drops is dropped whole, which, like any extra drop, only loses more of a
// tail that was already cut.
func (m argMask) straddleLen(s string) int {
	longest := 0
	for _, e := range m.entries {
		longest = max(longest, len(e)-1)
	}
	total := 0
	for round := 0; ; round++ {
		if round == maxStraddleRounds {
			return total + len(s)
		}
		// One failure table for the longest prefix of s any pattern can
		// match; a shorter pattern prefix reuses its leading part.
		fail := kmpFailure(s[:min(longest, len(s))])
		n := 0
		for _, v := range m.values {
			n = max(n, suffixPrefix(v, s, len(v)-1, fail))
		}
		for _, e := range m.entries {
			_, v, _ := strings.Cut(e, "=")
			// At least as long as the value: a suffix of exactly the value
			// is the cut landing right after '=', where the bare value then
			// starts the tail glued to whatever followed it. Dropping a tail
			// that merely starts with the value is harmless.
			if l := suffixPrefix(e, s, len(e)-1, fail); l > len(v)-1 {
				n = max(n, l)
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

// suffixPrefix returns the largest l <= limit such that the last l bytes of v
// are the first l bytes of s. fail is kmpFailure of a prefix of s at least
// min(limit, len(s)) long. It runs the KMP automaton for p = s[:limit] over v:
// the state after v's last byte is the longest prefix of p that ends v.
func suffixPrefix(v, s string, limit int, fail []int) int {
	p := s[:max(0, min(limit, len(s)))]
	if len(p) == 0 {
		return 0
	}
	q := 0
	for i := 0; i < len(v); i++ {
		if q == len(p) {
			q = fail[q-1]
		}
		for q > 0 && v[i] != p[q] {
			q = fail[q-1]
		}
		if v[i] == p[q] {
			q++
		}
	}
	return q
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
