// Package attention is the per-machine attention queue behind `forgectl
// attention`: `attention list` merges the desk's waiting items, the
// operator's open desk asks, the chief-of-staff sweep and a stuck drain into
// one queue, ordered by what a delay costs.
package attention

import (
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// MaxText is the most runes kept of one row's text.
const MaxText = 200

// Clean makes untrusted text one plain line: invalid UTF-8 is replaced,
// controls, bidi controls and invisible characters (zero-width, format,
// tag characters) are dropped, whitespace runs become one space, and the
// result is cut to maxRunes runes, the last of them an ellipsis when cut.
func Clean(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == ' ':
			space = true
			continue
		case termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if maxRunes > 0 && utf8.RuneCountInString(out) > maxRunes {
		runes := []rune(out)
		out = strings.TrimRight(string(runes[:maxRunes-1]), " ") + "…"
	}
	return out
}
