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
	// entries and values are sorted longest first, so a value that is a
	// prefix of another never gets replaced first and leaves the longer
	// one's tail in the text.
	entries []string
	values  []string
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
		m.entries = append(m.entries, e)
		m.values = append(m.values, value)
	}
	longestFirst(m.entries)
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

// text scrubs whole entries first, then bare values, so an echoed entry keeps
// its key: KEY=[redacted] rather than a bare [redacted].
func (m argMask) text(s string) string {
	for _, entry := range m.entries {
		if len(entry) >= minScrubLen {
			s = strings.ReplaceAll(s, entry, m.shown[entry])
		} else {
			s = replaceWholeWord(s, entry, m.shown[entry])
		}
	}
	for _, v := range m.values {
		if len(v) >= minScrubLen {
			s = strings.ReplaceAll(s, v, Redacted)
		} else {
			s = replaceWholeWord(s, v, Redacted)
		}
	}
	return s
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

// replaceWholeWord replaces each occurrence of v in s that is not glued to a
// word character on either side. The check applies only at an edge where v
// itself starts or ends with a word character, so a value like "a:b" is still
// found next to a letter.
func replaceWholeWord(s, v, with string) string {
	var b strings.Builder
	last := 0 // end of the text already copied to b
	for from := 0; ; {
		rel := strings.Index(s[from:], v)
		if rel < 0 {
			b.WriteString(s[last:])
			return b.String()
		}
		i := from + rel
		end := i + len(v)
		// Neighbors are read from the original string, so a match right
		// after another occurrence still sees the byte before it.
		gluedBefore := i > 0 && isWordByte(v[0]) && isWordByte(s[i-1])
		gluedAfter := end < len(s) && isWordByte(v[len(v)-1]) && isWordByte(s[end])
		if !gluedBefore && !gluedAfter {
			b.WriteString(s[last:i])
			b.WriteString(with)
			last = end
		}
		from = end
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
