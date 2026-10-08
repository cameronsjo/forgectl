package termsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// safeLineCappedReference is safeLineCapped as it was before the #963 item D
// plain-ASCII fast path: every rune through safeRune, counted one by one. The
// fast path is performance only, so SafeLineMax and SafeLineMaxJSON must
// render every input exactly as this does.
func safeLineCappedReference(s string, maxRunes, maxBytes int) string {
	markerBytes := 0
	if maxBytes > 0 {
		markerBytes = jsonStringBytes(TruncatedMarker)
	}
	var safe strings.Builder
	runes, size := 0, 0
	fit := 0
	for _, r := range s {
		piece := safeRune(r)
		n := utf8.RuneCountInString(piece)
		b := 0
		if maxBytes > 0 {
			b = jsonStringBytes(piece)
		}
		if (maxRunes > 0 && runes+n > maxRunes) || (maxBytes > 0 && size+b > maxBytes) {
			if maxBytes > 0 && markerBytes > maxBytes {
				return ""
			}
			return safe.String()[:fit] + TruncatedMarker
		}
		safe.WriteString(piece)
		runes += n
		size += b
		if maxBytes < 1 || size+markerBytes <= maxBytes {
			fit = safe.Len()
		}
	}
	return safe.String()
}

// safeLineMaxReference is SafeLineMax over the reference loop.
func safeLineMaxReference(s string, maxRunes int) string {
	if maxRunes < 1 {
		return safeLineReference(s)
	}
	return safeLineCappedReference(s, maxRunes, 0)
}

// safeLineMaxCaps are the caps the differential tests try: no cap, the
// smallest caps (so the cut lands on the first runes and the marker alone
// overflows a byte cap), the lengths around a short seed, and the caps
// production uses.
var safeLineMaxCaps = []int{-1, 0, 1, 2, 3, 5, 8, 13, 14, 15, 16, 17, 40, 80, 512, 1280, 1536, 4096}

// assertSafeLineMaxMatches checks SafeLineMax and SafeLineMaxJSON against the
// reference loop for s at every cap pair.
func assertSafeLineMaxMatches(t *testing.T, s string) {
	t.Helper()
	for _, maxRunes := range safeLineMaxCaps {
		if got, want := SafeLineMax(s, maxRunes), safeLineMaxReference(s, maxRunes); got != want {
			t.Fatalf("SafeLineMax(%.60q, %d) = %.60q, want %.60q", s, maxRunes, got, want)
		}
		for _, maxBytes := range safeLineMaxCaps {
			if got, want := SafeLineMaxJSON(s, maxRunes, maxBytes), safeLineCappedReference(s, maxRunes, maxBytes); got != want {
				t.Fatalf("SafeLineMaxJSON(%.60q, %d, %d) = %.60q, want %.60q", s, maxRunes, maxBytes, got, want)
			}
		}
	}
}

// TestSafeLineMaxMatchesTheSlowPath is #963 item D's differential: the
// plain-ASCII fast path in SafeLineMax and SafeLineMaxJSON is performance
// only, so each must render every input byte for byte as the per-rune loop
// does, at every cap. The inputs cover each branch of the fast path: all
// plain ASCII shorter than, equal to and longer than the cap; a plain prefix
// that ends before, at and after the cap; the ASCII bytes JSON escapes ('"',
// '\\', '<', '>', '&'); every single byte; and each byte after a plain prefix.
//
// Mutations that turn it red: widen isPlainASCII to c <= 0x7f (DEL is copied
// raw instead of escaped); return s[:maxRunes+1] in SafeLineMax's whole-ASCII
// cut; count every plain ASCII byte as one JSON byte in safeLineCapped (the
// '<' escape is undercounted).
func TestSafeLineMaxMatchesTheSlowPath(t *testing.T) {
	inputs := append([]string{}, safeLineEquivalenceSeeds...)
	for _, n := range []int{1, 2, 13, 14, 15, 16, 17, 79, 80, 81, 1279, 1280, 1281, 5000} {
		inputs = append(inputs,
			strings.Repeat("a", n),
			strings.Repeat("a", n)+"\x1b",
			strings.Repeat("a", n)+"\u202e tail",
			strings.Repeat("<&\"\\>", n),
			strings.Repeat("a", n)+"é"+strings.Repeat("b", n),
		)
	}
	for _, s := range inputs {
		assertSafeLineMaxMatches(t, s)
	}
	for b := range 256 {
		for _, s := range []string{string([]byte{byte(b)}), "ab" + string([]byte{byte(b)}) + "cd"} {
			assertSafeLineMaxMatches(t, s)
		}
	}
}

// FuzzSafeLineMaxMatchesTheSlowPath fuzzes the #963 item D equivalence.
func FuzzSafeLineMaxMatchesTheSlowPath(f *testing.F) {
	for _, s := range safeLineEquivalenceSeeds {
		f.Add(s, 5, 20)
	}
	f.Add(strings.Repeat("a", 100), 40, 0)
	f.Add(strings.Repeat("<", 100), 0, 40)
	f.Fuzz(func(t *testing.T, s string, maxRunes, maxBytes int) {
		if got, want := SafeLineMax(s, maxRunes), safeLineMaxReference(s, maxRunes); got != want {
			t.Fatalf("SafeLineMax(%q, %d) = %q, want %q", s, maxRunes, got, want)
		}
		if got, want := SafeLineMaxJSON(s, maxRunes, maxBytes), safeLineCappedReference(s, maxRunes, maxBytes); got != want {
			t.Fatalf("SafeLineMaxJSON(%q, %d, %d) = %q, want %q", s, maxRunes, maxBytes, got, want)
		}
	})
}

// BenchmarkSafeLineMax compares SafeLineMax and SafeLineMaxJSON with the
// per-rune reference at recordText's 1280-rune cap: a short error line, a
// 1 MB message that is cut, and mostly-ASCII text with a non-ASCII rune every
// 64 bytes. #963 item D asked for the fast path only if this shows a gain.
func BenchmarkSafeLineMax(b *testing.B) {
	const maxRunes, maxBytes = 1280, 1536
	inputs := []struct{ name, s string }{
		{"short", "open /home/user/src/project/file.go: permission denied (exit 1)"},
		{"long", strings.Repeat("open /home/user/src/project/file.go: denied ", 1<<20/44)},
		{"mixed", strings.Repeat(strings.Repeat("a", 61)+"é", 1<<20/63)},
	}
	for _, in := range inputs {
		b.Run(in.name+"/max/fast", func(b *testing.B) {
			for b.Loop() {
				_ = SafeLineMax(in.s, maxRunes)
			}
		})
		b.Run(in.name+"/max/reference", func(b *testing.B) {
			for b.Loop() {
				_ = safeLineMaxReference(in.s, maxRunes)
			}
		})
		b.Run(in.name+"/json/fast", func(b *testing.B) {
			for b.Loop() {
				_ = SafeLineMaxJSON(in.s, maxRunes, maxBytes)
			}
		})
		b.Run(in.name+"/json/reference", func(b *testing.B) {
			for b.Loop() {
				_ = safeLineCappedReference(in.s, maxRunes, maxBytes)
			}
		})
	}
}
