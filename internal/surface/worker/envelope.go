package worker

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// forgectl wraps all conversation text it puts into an agent session in an
// envelope, so the agent can tell it from text the operator types into the
// same terminal (cameronsjo/forgectl#1466). hc does the same for what
// `hc send` delivers, as <hc from="t7 review">…</hc>; forgectl mirrors that
// shape under its own element name, so a reader can tell the two senders
// apart:
//
//	<forgectl from="t1k" kind="brief">Run the tests. When you finish, …</forgectl>
//
// A launch prompt puts the tags on lines of their own around a multi-line
// body. The envelope is a convention that lowers the standing of automated
// text. It is not authentication: anything that can type into a pane can type
// bare text too, so bare text is never proof that the operator typed it.

// EnvelopeElement is the element name of forgectl's envelope.
const EnvelopeElement = "forgectl"

// Envelope kinds.
const (
	// KindLaunch marks a worker's launch prompt.
	KindLaunch = "launch"
	// KindBrief marks a brief typed into a running worker.
	KindBrief = "brief"
)

// DefaultSender is the from value when forgectl does not know which terminal
// launched it.
const DefaultSender = "forgectl"

// maxSenderLen bounds a sender name taken from the environment.
const maxSenderLen = 32

// senderShape is what a terminal id from the environment must look like to
// be written into the from attribute: hc ids ("t1k") and herdr pane ids
// ("w1-p2") both fit. Anything else, a quote or a space included, falls back
// to DefaultSender.
var senderShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// HcSenderEnv names the hc terminal a process runs in. hc's own envelope
// names the sending terminal from it.
const HcSenderEnv = "HC_TERMINAL_ID"

// herdrSenderEnv names the herdr pane a process runs in.
const herdrSenderEnv = "HERDR_PANE_ID"

// Sender names the terminal that caused this forgectl run, for the envelope's
// from attribute: the hc terminal, else the herdr pane, else DefaultSender.
func Sender(getenv func(string) string) string {
	for _, key := range []string{HcSenderEnv, herdrSenderEnv} {
		if v := getenv(key); len(v) <= maxSenderLen && senderShape.MatchString(v) {
			return v
		}
	}
	return DefaultSender
}

// hcElement is hc's envelope element. A body forgectl hands to hc to wrap
// must not close hc's envelope either.
const hcElement = "hc"

// envelopeTagLike matches anything in an NFKC-folded body that could read as
// an opening or closing tag of forgectl's element or hc's: any case, any
// space after the bracket or the slash, and the angle-bracket look-alikes
// NFKC leaves alone. NFKC has already turned fullwidth letters, brackets and
// slashes, and the compatibility spaces, into ASCII. The word boundary keeps
// <html> and the like.
var envelopeTagLike = regexp.MustCompile(`(?i)[<〈‹˂⟨]` + `[\s\p{Zs}]*[/⁄∕]?[\s\p{Zs}]*(?:` + EnvelopeElement + `|` + hcElement + `)\b`)

// neutralizeEnvelope rewrites each tag-like run of body so it cannot close an
// envelope early or open a forged one: the bracket becomes "&lt;", which no
// reader takes for a tag. Matching runs on each rune's NFKC fold, as
// mail.CleanBody folds a message, so a fullwidth or spaced spelling is
// caught; only the matched runes are rewritten (to their fold), and every
// other rune of the body keeps its own spelling.
func neutralizeEnvelope(body string) string {
	// folded is body with each rune NFKC-folded on its own; owner[k] is the
	// byte offset in body of the rune folded byte k came from.
	var fb strings.Builder
	owner := make([]int, 0, len(body))
	for i, r := range body {
		f := norm.NFKC.String(string(r))
		for range len(f) {
			owner = append(owner, i)
		}
		fb.WriteString(f)
	}
	folded := fb.String()
	locs := envelopeTagLike.FindAllStringIndex(folded, -1)
	if len(locs) == 0 {
		return body
	}
	var out strings.Builder
	last := 0 // body offset copied so far
	for _, loc := range locs {
		// Widen the match to whole source runes: from the first folded byte
		// of the rune it starts in to the last of the rune it ends in.
		lo, hi := loc[0], loc[1]
		for lo > 0 && owner[lo-1] == owner[loc[0]] {
			lo--
		}
		for hi < len(folded) && owner[hi] == owner[loc[1]-1] {
			hi++
		}
		start := owner[lo]
		// A later match inside a rune the last rewrite already emitted would
		// be skipped; no NFKC fold holds two tag-like runs, so none is.
		if start < last {
			continue // shares a rune with the run just rewritten
		}
		out.WriteString(body[last:start])
		out.WriteString(folded[lo:loc[0]])
		_, size := utf8.DecodeRuneInString(folded[loc[0]:])
		out.WriteString("&lt;" + folded[loc[0]+size:hi])
		last = len(body)
		if hi < len(folded) {
			last = owner[hi]
		}
	}
	out.WriteString(body[last:])
	return out.String()
}

// Envelope wraps body in forgectl's envelope. from is a Sender value and kind
// is KindLaunch or KindBrief. A launch body may span lines, so its tags sit on
// lines of their own; a brief is typed on one line and stays on one.
func Envelope(from, kind, body string) string {
	if len(from) > maxSenderLen || !senderShape.MatchString(from) {
		from = DefaultSender
	}
	open := "<" + EnvelopeElement + ` from="` + from + `" kind="` + kind + `">`
	end := "</" + EnvelopeElement + ">"
	body = neutralizeEnvelope(body)
	if kind == KindLaunch {
		return open + "\n" + body + "\n" + end
	}
	return open + body + end
}

// typedBriefRoom is how many characters a typed brief's own text may have:
// MaxTypedBrief less the report instruction and the envelope, measured with
// the longest sender name, so it is the same whichever terminal runs
// forgectl. A forgectl or hc tag written in the text grows by 3 when it is
// neutralized, which the cap counts and this figure does not.
var typedBriefRoom = MaxTypedBrief - (utf8.RuneCountInString(TypedBrief("x", strings.Repeat("0", 2*markerBytes), strings.Repeat("x", maxSenderLen))) - 1)

// TypedBrief is the one line forgectl types into a worker for a brief: the
// text with its report instruction (Compose), in forgectl's envelope.
func TypedBrief(text, marker, from string) string {
	return Envelope(from, KindBrief, Compose(text, marker, ViaTyped))
}

// BriefLine is the line forgectl types or sends for a brief on every
// surface: the text in forgectl's envelope naming Sender. On hc the send
// keeps the launcher's HC_TERMINAL_ID and HC_SCOPE
// (exec.UnsetLauncherSessionHandlesKeepHcSender), so hc may also wrap it in
// its own <hc from=…> tag; forgectl's envelope is written either way, so
// the text is never bare whatever hc does (forgectl#1466).
func BriefLine(text, marker string, getenv func(string) string) string {
	return TypedBrief(text, marker, Sender(getenv))
}
