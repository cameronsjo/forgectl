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
// themselves: each search restarts one byte past the last match. A pattern of
// minScrubLen or more always qualifies. A shorter one qualifies only where it
// stands as a whole word: not glued to a word character on either side. The
// check applies only at an edge where the pattern itself starts or ends with
// a word character, so a value like "a:b" is still found next to a letter,
// and a neighboring byte that is already covered is not a word character,
// since it is about to become [redacted]. Covering a short match can
// therefore qualify another one beside it, on either side, so the short
// patterns are rescanned until a pass covers nothing new.
//
// Cost: each pattern is one strings.Index scan per match plus one, so a
// stream with few matches costs near-linear time whatever the values'
// lengths. The short patterns are rescanned after every pass that covered
// something new, and the loop ends on the first pass that covers nothing. Every match is verified in full, so a stream that repeats a long
// value overlapping itself byte for byte costs that value's length per byte;
// stdout is capped at maxStdoutBytes and stderr at maxStderrTail, and the
// stream is the failing child's own output.
func (m argMask) cover(s string) bitset {
	var covered bitset
	mark := func(from, to int) bool {
		if covered == nil {
			covered = newBitset(len(s))
		}
		return covered.set(from, to)
	}
	for _, p := range m.pats {
		if len(p.text) < minScrubLen {
			continue
		}
		done := 0 // bytes of this pattern's earlier matches already marked
		for from := 0; ; {
			rel := strings.Index(s[from:], p.text)
			if rel < 0 {
				break
			}
			i := from + rel
			end := i + len(p.text)
			mark(max(i, done), end)
			done = end
			from = i + 1
		}
	}
	isWord := func(i int) bool { return isWordByte(s[i]) && !covered.has(i) }
	for changed := true; changed; {
		changed = false
		for _, p := range m.pats {
			if len(p.text) >= minScrubLen {
				continue
			}
			for from := 0; ; {
				rel := strings.Index(s[from:], p.text)
				if rel < 0 {
					break
				}
				i := from + rel
				end := i + len(p.text)
				from = i + 1
				gluedBefore := i > 0 && isWordByte(p.text[0]) && isWord(i-1)
				gluedAfter := end < len(s) && isWordByte(p.text[len(p.text)-1]) && isWord(end)
				if !gluedBefore && !gluedAfter && mark(i, end) {
					changed = true
				}
			}
		}
	}
	return covered
}

// bitset is one bit per byte of the text cover scans. A nil bitset has no
// bit set.
type bitset []uint64

func newBitset(n int) bitset { return make(bitset, (n+63)/64) }

func (b bitset) has(i int) bool { return b != nil && b[i/64]&(uint64(1)<<(i%64)) != 0 }

// set sets the bits in [from, to) and reports whether any was clear.
func (b bitset) set(from, to int) bool {
	changed := false
	for i := from; i < to; i++ {
		w, bit := i/64, uint64(1)<<(i%64)
		if b[w]&bit == 0 {
			b[w] |= bit
			changed = true
		}
	}
	return changed
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
