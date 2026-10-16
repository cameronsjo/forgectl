// Package attention is the per-machine attention queue behind `forgectl
// attention`: each Claude Code session publishes the `What's next:` items of
// its latest reply to a private spool, and `attention list` merges those with
// the desk's waiting items and the chief-of-staff sweep into one queue,
// ordered by what a delay costs.
//
// Everything a record holds is untrusted. Any process running as this user
// can write the spool, workers included, so a record is re-validated on every
// read, its text is stripped and cut, and the pane a viewer jumps to comes
// from herdr, never from the record.
package attention

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Item kinds. Only questions and actions become queue rows; a note (a
// free-form bold lead) and an option (a lettered choice under a question)
// are read so they count as parsed, and are not waits of their own.
const (
	KindQuestion = "q"
	KindAction   = "a"
	KindNote     = "note"
	KindOption   = "option"
)

// Actors an action names.
const (
	ActorCameron = "Cameron"
	ActorClaude  = "Claude"
)

// Recommended values, after mapping.
const (
	RecNow    = "now"
	RecLater  = "later"
	RecPark   = "park"
	RecForget = "forget"
)

// Caps on what one block yields.
const (
	// MaxItems is the most items kept from one block.
	MaxItems = 20
	// MaxText is the most runes kept of one item's text.
	MaxText = 200
	// maxN is the largest item number accepted.
	maxN = 999
)

// Item is one parsed line of a `What's next:` block.
type Item struct {
	Kind  string `json:"kind"`
	N     int    `json:"n,omitempty"`
	Actor string `json:"actor,omitempty"`
	// Tag is an option's letter; empty for other kinds.
	Tag  string `json:"tag,omitempty"`
	Text string `json:"text"`
	Rec  string `json:"rec,omitempty"`
}

// Parsed is what Parse found. Seen counts the item lines in the block and
// Read the ones the grammar understood, so a partial parse still reports
// how partial it was.
type Parsed struct {
	Found bool
	Items []Item
	Seen  int
	Read  int
}

var (
	headerRe = regexp.MustCompile(`(?i)^(?:#{1,6}\s+)?(?:\*\*|__)?\s*what['’]s\s+next\s*:?\s*(?:\*\*|__)?\s*:?\s*$`)
	bulletRe = regexp.MustCompile(`^\s{0,6}(?:[-*+]|(\d{1,3})[.)])\s+(.*)$`)
	// **Question 1**: / **Action 2 (Cameron)**: / **Action 2** (Cameron):
	boldQARe = regexp.MustCompile(`(?i)^\*\*\s*(question|q|action|a)\s*(\d{1,3})\s*(?:\(\s*(cameron|claude)\s*\))?\s*:?\s*\*\*\s*(?:\(\s*(cameron|claude)\s*\))?\s*(.*)$`)
	// Question 1: / Action 2 (Cameron): without bold.
	plainQARe = regexp.MustCompile(`(?i)^(question|action)\s+(\d{1,3})\s*(?:\(\s*(cameron|claude)\s*\))?\s*:\s*(.*)$`)
	// **Cameron** — / **Claude**:
	bareActorRe = regexp.MustCompile(`(?i)^\*\*\s*(cameron|claude)\s*:?\s*\*\*\s*(.*)$`)
	// **C.** / **B)** / **(A)**
	letterRe = regexp.MustCompile(`^\*\*\s*\(?([A-Z])[.)]?\s*:?\s*\*\*\s*(.*)$`)
	// **Any bold lead.** rest
	freeBoldRe = regexp.MustCompile(`^\*\*([^*]+)\*\*\s*(.*)$`)
	recRe      = regexp.MustCompile(`(?i)\s*[(\[]?\s*\**\s*recommended\s*\**\s*:\s*\**\s*([a-z]+)\**\s*[)\]]?\.?`)
	// leadSepRe is the separator between a lead and its text.
	leadSepRe = regexp.MustCompile(`^[\s:.\-–—]+`)
	// ruleRe is a thematic break (spaces removed first); it ends a block.
	ruleRe = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})$`)
)

// Parse reads the last `What's next:` block outside fenced code in a reply.
// It never fails: text with no block is Parsed{Found: false}.
func Parse(text string) Parsed {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	header := -1
	var fence string
	for i, line := range lines {
		if marker, rest, ok := fenceMarker(line); ok {
			fence = toggleFence(fence, marker, rest)
			continue
		}
		if fence == "" && headerRe.MatchString(strings.TrimSpace(line)) {
			header = i
		}
	}
	if header < 0 {
		return Parsed{}
	}
	p := Parsed{Found: true}
	b := blockParser{p: &p}
	for _, line := range lines[header+1:] {
		if !b.line(line) {
			break
		}
	}
	return p
}

// blockParser walks the lines after a header.
type blockParser struct {
	p        *Parsed
	sawBlank bool
	items    int // items seen so far, for an unnumbered bare actor
	last     *Item
}

// line takes one line and reports whether the block goes on.
func (b *blockParser) line(line string) bool {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		b.sawBlank = true
		return true
	case strings.HasPrefix(trimmed, "#"), ruleRe.MatchString(strings.ReplaceAll(trimmed, " ", "")):
		return false
	}
	if _, _, ok := fenceMarker(line); ok {
		return false
	}
	if m := bulletRe.FindStringSubmatch(line); m != nil {
		b.sawBlank = false
		b.items++
		b.p.Seen++
		it, ok := parseItem(m[1], m[2], b.items)
		if !ok {
			b.last = nil
			return true
		}
		b.p.Read++
		if len(b.p.Items) < MaxItems {
			b.p.Items = append(b.p.Items, it)
			b.last = &b.p.Items[len(b.p.Items)-1]
		} else {
			b.last = nil
		}
		return true
	}
	// Prose: after a blank line it ends the block; right under an item it
	// continues that item; right under the header it is skipped.
	if b.sawBlank {
		return false
	}
	if b.last != nil {
		b.continueItem(trimmed)
	}
	return true
}

// continueItem appends a wrapped line to the item above it.
func (b *blockParser) continueItem(text string) {
	text, rec := splitRec(text)
	if b.last.Rec == "" {
		b.last.Rec = rec
	}
	b.last.Text = Clean(b.last.Text+" "+text, MaxText)
}

// parseItem reads one bullet's content. num is the list number ("" for a
// dash bullet) and ordinal the item's position in the block.
func parseItem(num, rest string, ordinal int) (Item, bool) {
	listN := ordinal
	if n, err := strconv.Atoi(num); err == nil && n > 0 && n <= maxN {
		listN = n
	}
	rest = strings.TrimSpace(rest)
	var it Item
	switch {
	case boldQARe.MatchString(rest):
		m := boldQARe.FindStringSubmatch(rest)
		it = qaItem(m[1], m[2], firstNonEmpty(m[3], m[4]), m[5])
	case plainQARe.MatchString(rest):
		m := plainQARe.FindStringSubmatch(rest)
		it = qaItem(m[1], m[2], m[3], m[4])
	case bareActorRe.MatchString(rest):
		m := bareActorRe.FindStringSubmatch(rest)
		it = Item{Kind: KindAction, N: listN, Actor: actorName(m[1]), Text: m[2]}
	case letterRe.MatchString(rest):
		m := letterRe.FindStringSubmatch(rest)
		it = Item{Kind: KindOption, Tag: m[1], Text: m[2]}
	case freeBoldRe.MatchString(rest):
		it = Item{Kind: KindNote, Text: rest}
	default:
		return Item{}, false
	}
	text, rec := splitRec(leadSepRe.ReplaceAllString(it.Text, ""))
	it.Text, it.Rec = Clean(text, MaxText), rec
	return it, true
}

func qaItem(word, num, actor, text string) Item {
	n, _ := strconv.Atoi(num) // the pattern allows digits only
	it := Item{N: n, Text: text, Kind: KindAction}
	if w := strings.ToLower(word); w == "question" || w == "q" {
		it.Kind = KindQuestion
		return it
	}
	it.Actor = actorName(actor)
	return it
}

func actorName(s string) string {
	switch strings.ToLower(s) {
	case "cameron":
		return ActorCameron
	case "claude":
		return ActorClaude
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// splitRec cuts a `Recommended: X` clause out of text and maps X.
func splitRec(text string) (string, string) {
	loc := recRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return text, ""
	}
	rec := mapRec(text[loc[2]:loc[3]])
	if rec == "" {
		// An unknown value stays in the text, where a reader can still see it.
		return text, ""
	}
	rest := strings.TrimSpace(text[:loc[0]] + " " + text[loc[1]:])
	rest = strings.TrimRight(rest, " —–-·,;")
	return strings.TrimSpace(rest), rec
}

func mapRec(s string) string {
	switch strings.ToLower(s) {
	case "now", "go", "yes":
		return RecNow
	case "later", "defer":
		return RecLater
	case "park":
		return RecPark
	case "forget", "leave":
		return RecForget
	}
	return ""
}

// fenceMarker reports whether line is a code fence line, with its marker run
// (``` or ~~~, three or more) and the text after it. As in CommonMark, a
// backtick run followed by text holding a backtick is not a fence (it is an
// inline code span).
func fenceMarker(line string) (marker, rest string, ok bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return "", "", false
	}
	for _, c := range []string{"`", "~"} {
		n := 0
		for n < len(t) && t[n:n+1] == c {
			n++
		}
		if n < 3 {
			continue
		}
		if c == "`" && strings.Contains(t[n:], "`") {
			return "", "", false
		}
		return t[:n], t[n:], true
	}
	return "", "", false
}

// toggleFence returns the open fence after seeing marker: a bare marker of
// the same character, at least as long, closes it; any marker opens one when
// none is open.
func toggleFence(open, marker, rest string) string {
	if open == "" {
		return marker
	}
	if marker[0] == open[0] && len(marker) >= len(open) && strings.TrimSpace(rest) == "" {
		return ""
	}
	return open
}

// Clean makes untrusted text one plain line: invalid UTF-8 is replaced,
// controls, bidi controls and invisible characters (zero-width, format,
// tag characters) are dropped, whitespace runs become one space, and the
// result is cut to maxRunes runes, the last of them an ellipsis when cut.
func Clean(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "�")
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
