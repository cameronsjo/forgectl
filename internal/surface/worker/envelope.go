package worker

import (
	"regexp"
	"unicode/utf8"
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

// HcWraps reports whether `hc send` run by this process wraps what it
// delivers in hc's own envelope, so forgectl must not add its own. hc names
// the sending terminal in that envelope (<hc from="t1k">), which it can do
// only when the sender runs in an hc terminal; forgectl keeps that
// terminal's id for its text sends
// (exec.UnsetLauncherSessionHandlesKeepHcSender). Run from a plain shell,
// hc delivers the text bare, and forgectl wraps it instead.
func HcWraps(getenv func(string) string) bool {
	return getenv(HcSenderEnv) != ""
}

// hcElement is hc's envelope element. A body forgectl hands to hc to wrap
// must not close hc's envelope either.
const hcElement = "hc"

// envelopeTagLike matches anything in a body that could read as an opening
// or closing tag of forgectl's element or hc's: any case, spaces after the
// angle bracket or the slash, and the fullwidth and small-form angle
// brackets. The word boundary keeps <html> and the like.
var envelopeTagLike = regexp.MustCompile(`(?i)[<＜﹤]\s*/?\s*(?:` + EnvelopeElement + `|` + hcElement + `)\b`)

// neutralizeEnvelope rewrites each tag-like run of body so it cannot close an
// envelope early or open a forged one: the angle bracket becomes "&lt;", which
// no reader takes for a tag.
func neutralizeEnvelope(body string) string {
	return envelopeTagLike.ReplaceAllStringFunc(body, func(m string) string {
		_, size := utf8.DecodeRuneInString(m)
		return "&lt;" + m[size:]
	})
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

// TypedBrief is the one line forgectl types into a worker for a brief: the
// text with its report instruction (Compose), in forgectl's envelope.
func TypedBrief(text, marker, from string) string {
	return Envelope(from, KindBrief, Compose(text, marker, ViaTyped))
}

// HcTypedBrief is the line forgectl hands `hc send` when hc wraps it in its
// own envelope (HcWraps): Compose's text with tag-like runs neutralized, and
// no envelope of forgectl's on top.
func HcTypedBrief(text, marker string) string {
	return neutralizeEnvelope(Compose(text, marker, ViaTyped))
}
