// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package ask holds the operator's "asks": things an agent needs a person to
// do (a login, a purchase, a device check) that are not scripts. An ask has a
// title, markdown steps, a priority bucket, links, who asked, and the
// responses it allows: done, a free-text reply, a link, or a snooze.
//
// # Store
//
// Everything lives in one append-only log, <desk>/asks/log.jsonl: one JSON
// record per line, each an ask, a response, or a notify outcome. The current
// state of every ask is a fold over the log ([Fold]); nothing is ever
// rewritten. Writers take an exclusive lock on log.jsonl.lock (the
// config.WithFileLock helper), so a check-then-append (is this ask still
// open?) cannot race another writer.
//
// # Trust
//
// Ask text comes from agents and is untrusted. This package bounds it (one
// line titles, capped sizes, valid UTF-8, http/https links only); the front
// ends render it inert (termsafe in a terminal, escaped markdown in HTML).
// Responses come from the operator.
package ask

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bucket is an ask's priority.
type Bucket string

// The buckets, most urgent first.
const (
	BucketNow      Bucket = "now"
	BucketSoon     Bucket = "soon"
	BucketOptional Bucket = "optional"
	BucketComing   Bucket = "coming"
)

// Buckets lists every bucket, most urgent first.
var Buckets = []Bucket{BucketNow, BucketSoon, BucketOptional, BucketComing}

// Label is the bucket's heading in a front end.
func (b Bucket) Label() string {
	switch b {
	case BucketNow:
		return "Now"
	case BucketSoon:
		return "When you have 10 minutes"
	case BucketOptional:
		return "Optional"
	case BucketComing:
		return "Coming"
	}
	return string(b)
}

// ParseBucket accepts a bucket name.
func ParseBucket(s string) (Bucket, error) {
	b := Bucket(strings.ToLower(strings.TrimSpace(s)))
	if slices.Contains(Buckets, b) {
		return b, nil
	}
	return "", fmt.Errorf("ask: bucket %q is not one of now, soon, optional, coming", clip(s, 40))
}

// Kind is a response's kind.
type Kind string

// Response kinds.
const (
	KindDone   Kind = "done"
	KindReply  Kind = "reply"
	KindLink   Kind = "link"
	KindSnooze Kind = "snooze"
)

// Kinds lists every response kind.
var Kinds = []Kind{KindDone, KindReply, KindLink, KindSnooze}

// Resolves reports whether a response of this kind answers the ask. A snooze
// only hides it for a while.
func (k Kind) Resolves() bool { return k == KindDone || k == KindReply || k == KindLink }

// ParseKinds reads a comma-separated list of response kinds.
func ParseKinds(s string) ([]Kind, error) {
	var out []Kind
	for _, part := range strings.Split(s, ",") {
		k := Kind(strings.ToLower(strings.TrimSpace(part)))
		if k == "" {
			continue
		}
		if !slices.Contains(Kinds, k) {
			return nil, fmt.Errorf("ask: response kind %q is not one of done, reply, link, snooze", clip(string(k), 40))
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("ask: no response kinds given")
	}
	return out, nil
}

// Link is a labelled URL on an ask.
type Link struct {
	Label string `json:"label,omitempty"`
	URL   string `json:"url"`
}

// Ask is one request for the operator.
type Ask struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Title string    `json:"title"`
	// Steps is markdown. Untrusted: render with raw HTML off.
	Steps  string `json:"steps,omitempty"`
	Bucket Bucket `json:"bucket"`
	Links  []Link `json:"links,omitempty"`
	// From is who asked: a worker name or a session.
	From string `json:"from,omitempty"`
	// Notify is a terminal address told about each response, or "".
	Notify string `json:"notify,omitempty"`
	Allow  []Kind `json:"allow"`
}

// Allows reports whether the ask accepts a response of kind k.
func (a Ask) Allows(k Kind) bool { return slices.Contains(a.Allow, k) }

// Response is the operator's answer to an ask.
type Response struct {
	ID   string    `json:"id"`
	Ask  string    `json:"ask"`
	At   time.Time `json:"at"`
	Kind Kind      `json:"kind"`
	// Text is a reply's text.
	Text string `json:"text,omitempty"`
	// URL is a link response's URL.
	URL string `json:"url,omitempty"`
	// Until is when a snooze ends.
	Until *time.Time `json:"until,omitempty"`
	// Via is the front end that recorded it: "ui", "web" or "cli".
	Via string `json:"via,omitempty"`
}

// Notified records one attempt to tell the asking agent about a response.
type Notified struct {
	Response string    `json:"response"`
	At       time.Time `json:"at"`
	Address  string    `json:"address"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
}

// Record is one log line. Exactly one of Ask, Response and Notified is set.
type Record struct {
	V        int       `json:"v"`
	Ask      *Ask      `json:"ask,omitempty"`
	Response *Response `json:"response,omitempty"`
	Notified *Notified `json:"notified,omitempty"`
	// Offset is the byte offset just past this record's line in the log.
	// Not stored; set when the log is read.
	Offset int64 `json:"-"`
}

// recordVersion is the log format version every record carries.
const recordVersion = 1

// Limits on what an ask or response may hold.
const (
	MaxTitle    = 200
	MaxSteps    = 16 << 10
	MaxLinks    = 12
	MaxURL      = 2048
	MaxLabel    = 120
	MaxFrom     = 120
	MaxReply    = 4000
	MaxSnooze   = 30 * 24 * time.Hour
	maxAddress  = 64
	maxIDLength = 32
)

// Errors callers branch on.
var (
	// ErrInvalid wraps every validation refusal.
	ErrInvalid = errors.New("ask: invalid")
	// ErrNotFound reports an ask id the log does not hold.
	ErrNotFound = errors.New("ask: no such ask")
	// ErrClosed reports a response to an ask that is already answered.
	ErrClosed = errors.New("ask: already answered")
	// ErrNotAllowed reports a response kind the ask does not accept.
	ErrNotAllowed = errors.New("ask: response kind not allowed for this ask")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// oneLine reports whether s is valid UTF-8 with no control or format
// characters (no newlines, no bidi overrides).
func oneLine(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// multiLine is oneLine that allows newlines and tabs.
func multiLine(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// SafeURL reports whether raw is an absolute http or https URL with a host,
// the only links any front end renders as links.
func SafeURL(raw string) bool {
	if raw == "" || len(raw) > MaxURL || !oneLine(raw) || strings.ContainsAny(raw, " \\") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// ValidAddress reports whether s looks like an hc terminal address or name:
// 1-64 of a-z A-Z 0-9 - _ . : starting with a letter or digit.
func ValidAddress(s string) bool {
	if s == "" || len(s) > maxAddress {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && c != '-' && c != '_' && c != '.' && c != ':' {
			return false
		}
	}
	return true
}

// ValidID reports whether s has the shape of an ask or response id.
func ValidID(s string) bool {
	if len(s) < 3 || len(s) > maxIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Normalize trims and checks a new ask. It fills Allow with every kind when
// empty. ID and At are the store's to set.
func Normalize(a Ask) (Ask, error) {
	a.Title = strings.TrimSpace(a.Title)
	switch {
	case a.Title == "":
		return a, invalid("the title is empty")
	case utf8.RuneCountInString(a.Title) > MaxTitle:
		return a, invalid("the title is longer than %d characters", MaxTitle)
	case !oneLine(a.Title):
		return a, invalid("the title must be one line of text with no control characters")
	}
	a.Steps = strings.TrimRight(strings.ReplaceAll(a.Steps, "\r\n", "\n"), "\n ")
	if len(a.Steps) > MaxSteps {
		return a, invalid("the steps are longer than %d bytes", MaxSteps)
	}
	if !multiLine(a.Steps) {
		return a, invalid("the steps must be UTF-8 text with no control characters other than newline and tab")
	}
	if !slices.Contains(Buckets, a.Bucket) {
		return a, invalid("bucket %q is not one of now, soon, optional, coming", clip(string(a.Bucket), 40))
	}
	if len(a.Links) > MaxLinks {
		return a, invalid("more than %d links", MaxLinks)
	}
	for i, l := range a.Links {
		l.Label = strings.TrimSpace(l.Label)
		if !SafeURL(l.URL) {
			return a, invalid("link %d is not an http or https URL", i+1)
		}
		if utf8.RuneCountInString(l.Label) > MaxLabel || !oneLine(l.Label) {
			return a, invalid("link %d's label must be one line of at most %d characters", i+1, MaxLabel)
		}
		a.Links[i] = l
	}
	a.From = strings.TrimSpace(a.From)
	if utf8.RuneCountInString(a.From) > MaxFrom || !oneLine(a.From) {
		return a, invalid("--from must be one line of at most %d characters", MaxFrom)
	}
	if a.Notify != "" && !ValidAddress(a.Notify) {
		return a, invalid("--notify %q is not a terminal address", clip(a.Notify, 40))
	}
	if len(a.Allow) == 0 {
		a.Allow = slices.Clone(Kinds)
	}
	for _, k := range a.Allow {
		if !slices.Contains(Kinds, k) {
			return a, invalid("response kind %q is not one of done, reply, link, snooze", clip(string(k), 40))
		}
	}
	return a, nil
}

// CheckResponse validates r against the ask it answers, at time now. It
// trims a reply's text.
func CheckResponse(it Item, r Response, now time.Time) (Response, error) {
	if it.State == StateAnswered {
		return r, fmt.Errorf("%w: %s", ErrClosed, it.Ask.ID)
	}
	if !it.Ask.Allows(r.Kind) {
		return r, fmt.Errorf("%w: %s does not accept %q", ErrNotAllowed, it.Ask.ID, r.Kind)
	}
	switch r.Kind {
	case KindDone:
		r.Text, r.URL, r.Until = "", "", nil
	case KindReply:
		r.Text = strings.TrimSpace(strings.ReplaceAll(r.Text, "\r\n", "\n"))
		if r.Text == "" {
			return r, invalid("the reply is empty")
		}
		if utf8.RuneCountInString(r.Text) > MaxReply || !multiLine(r.Text) {
			return r, invalid("the reply must be text of at most %d characters", MaxReply)
		}
		r.URL, r.Until = "", nil
	case KindLink:
		r.URL = strings.TrimSpace(r.URL)
		if !SafeURL(r.URL) {
			return r, invalid("the link must be an http or https URL")
		}
		r.Text, r.Until = "", nil
	case KindSnooze:
		if r.Until == nil || !r.Until.After(now) {
			return r, invalid("a snooze needs a time in the future")
		}
		if r.Until.Sub(now) > MaxSnooze {
			return r, invalid("a snooze may last at most %d days", int(MaxSnooze/(24*time.Hour)))
		}
		u := r.Until.UTC()
		r.Until = &u
		r.Text, r.URL = "", ""
	default:
		return r, invalid("response kind %q is not one of done, reply, link, snooze", clip(string(r.Kind), 40))
	}
	return r, nil
}

// State is an ask's state in the fold.
type State string

// Ask states.
const (
	StateOpen     State = "open"
	StateSnoozed  State = "snoozed"
	StateAnswered State = "answered"
)

// Item is an ask with its responses folded in.
type Item struct {
	Ask       Ask        `json:"ask"`
	State     State      `json:"state"`
	Responses []Response `json:"responses,omitempty"`
	// Answer is the response that answered the ask, when answered.
	Answer *Response `json:"answer,omitempty"`
	// SnoozedUntil is the end of the current snooze, when snoozed.
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
	// Notified lists every notify attempt, in order.
	Notified []Notified `json:"notified,omitempty"`
}

// Fold turns the log's records into one Item per ask, in the order the asks
// were made. A response or notify record for an unknown ask is ignored. A
// response after an answer is ignored too: the store never writes one, so it
// can only come from a hand edit.
func Fold(records []Record, now time.Time) []Item {
	var items []Item
	index := map[string]int{}
	byResponse := map[string]int{}
	for _, rec := range records {
		switch {
		case rec.Ask != nil:
			if _, dup := index[rec.Ask.ID]; dup {
				continue
			}
			index[rec.Ask.ID] = len(items)
			items = append(items, Item{Ask: *rec.Ask, State: StateOpen})
		case rec.Response != nil:
			i, ok := index[rec.Response.Ask]
			if !ok || items[i].State == StateAnswered {
				continue
			}
			r := *rec.Response
			byResponse[r.ID] = i
			items[i].Responses = append(items[i].Responses, r)
			switch {
			case r.Kind.Resolves():
				items[i].State = StateAnswered
				items[i].Answer = &items[i].Responses[len(items[i].Responses)-1]
				items[i].SnoozedUntil = nil
			case r.Kind == KindSnooze && r.Until != nil:
				u := *r.Until
				items[i].SnoozedUntil = &u
			}
		case rec.Notified != nil:
			if i, ok := byResponse[rec.Notified.Response]; ok {
				items[i].Notified = append(items[i].Notified, *rec.Notified)
			}
		}
	}
	for i := range items {
		if items[i].State == StateAnswered {
			// Answer points into Responses, which may have been reallocated
			// by later appends; re-point it at the last response.
			items[i].Answer = &items[i].Responses[len(items[i].Responses)-1]
			continue
		}
		if items[i].SnoozedUntil != nil {
			if items[i].SnoozedUntil.After(now) {
				items[i].State = StateSnoozed
			} else {
				items[i].SnoozedUntil = nil
			}
		}
	}
	return items
}

// Find returns the item with id.
func Find(items []Item, id string) (Item, bool) {
	for _, it := range items {
		if it.Ask.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

// Grouped returns the open items per bucket (most urgent first), then the
// snoozed ones, then the answered ones most recent first.
func Grouped(items []Item) (open map[Bucket][]Item, snoozed, answered []Item) {
	open = map[Bucket][]Item{}
	for _, it := range items {
		switch it.State {
		case StateOpen:
			open[it.Ask.Bucket] = append(open[it.Ask.Bucket], it)
		case StateSnoozed:
			snoozed = append(snoozed, it)
		case StateAnswered:
			answered = append(answered, it)
		}
	}
	slices.SortStableFunc(answered, func(a, b Item) int { return b.Answer.At.Compare(a.Answer.At) })
	return open, snoozed, answered
}

// NotifyText is the one line sent to the asking agent's terminal for r.
func NotifyText(a Ask, r Response) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DESK-RESPONSE %s %s", a.ID, r.Kind)
	switch r.Kind {
	case KindReply:
		b.WriteString(": " + flatten(r.Text, 300))
	case KindLink:
		b.WriteString(": " + r.URL)
	case KindSnooze:
		if r.Until != nil {
			b.WriteString(" until " + r.Until.UTC().Format(time.RFC3339))
		}
	}
	fmt.Fprintf(&b, " (ask: %s; full record: forgectl desk watch --responses %s)", flatten(a.Title, 80), a.ID)
	return b.String()
}

// flatten makes s one line of at most n runes.
func flatten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, n)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
