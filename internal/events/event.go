// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package events is forgectl's event log: what changed on the pull requests
// a Claude Code session watches, as typed facts. A poller (Poll) reads each
// watched PR from GitHub, compares it with the last snapshot, and appends one
// Event per change to a machine log and to the feed of every session that
// watches the PR. Readers are the c3 mod, which hands new feed events to its
// session when a turn ends, and cadence-hooks, which reads a PR's last
// snapshot (State) before it lets a merge through.
//
// Every field is typed and checked on write and again on read: ids, enums,
// SHAs and counts, never a PR title, body, comment or pane text. A feed line
// reaches a model's context, so nothing in it can carry words someone else
// wrote.
package events

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/pr"
)

// Kinds of event, one per change Diff can see.
const (
	KindPushed = "pr.pushed" // the head moved
	KindChecks = "pr.checks" // the head's check rollup finished (or changed after finishing)
	KindReview = "pr.review" // a new cadence-review marker counts at the head
	KindReady  = "pr.ready"  // draft to ready for review
	KindCITL   = "pr.citl"   // the CITL label went on: the last step needs the operator
	KindMerged = "pr.merged"
	KindClosed = "pr.closed" // closed without a merge
)

var kinds = map[string]bool{KindPushed: true, KindChecks: true, KindReview: true, KindReady: true, KindCITL: true, KindMerged: true, KindClosed: true}

// Check rollups a snapshot or event may carry.
var checkStates = map[string]bool{"SUCCESS": true, "FAILURE": true, "ERROR": true, "PENDING": true, "EXPECTED": true}

// terminalChecks are the rollups that end a wait on checks.
var terminalChecks = map[string]bool{"SUCCESS": true, "FAILURE": true, "ERROR": true}

var (
	sha40Re   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	refRe     = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#([1-9][0-9]{0,8})$`)
	urlRe     = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/pull/([1-9][0-9]{0,8})/?$`)
)

// maxCount bounds a marker's crit or imp count on read; a larger one is not
// a review anyone wrote.
const maxCount = 9999

// ErrBadRef is a PR reference that is neither owner/repo#N nor a github.com
// pull request URL.
var ErrBadRef = errors.New("not a PR reference: use owner/repo#N or a github.com pull request URL")

// ErrBadSession is a session id the store will not name a file after.
var ErrBadSession = errors.New("not a session id: letters, digits, '.', '_' and '-', up to 128, not starting with '.'")

// Ref names one pull request.
type Ref struct {
	Repo   string // owner/name
	Number int
}

// String is the ref's canonical owner/repo#N form, the key the store uses.
func (r Ref) String() string { return r.Repo + "#" + strconv.Itoa(r.Number) }

// ParseRef reads owner/repo#N or https://github.com/owner/repo/pull/N.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	m := refRe.FindStringSubmatch(s)
	if m == nil {
		m = urlRe.FindStringSubmatch(s)
	}
	if m == nil || !pr.ValidOwnerRepoPart(m[1]) || !pr.ValidOwnerRepoPart(m[2]) {
		return Ref{}, ErrBadRef
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return Ref{}, ErrBadRef
	}
	return Ref{Repo: m[1] + "/" + m[2], Number: n}, nil
}

// ValidSession reports whether id may name a feed.
func ValidSession(id string) bool { return sessionRe.MatchString(id) }

// Event is one change on one PR. Seq orders the machine log; a feed holds
// the same events, with the same Seq, for the sessions that watch the PR.
type Event struct {
	Seq  int64  `json:"seq"`
	At   string `json:"at"` // RFC 3339, UTC
	Kind string `json:"kind"`
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	Head string `json:"head,omitempty"`
	// Checks is the rollup a pr.checks event reports.
	Checks string `json:"checks,omitempty"`
	// Crit and Imp are a pr.review event's counts at the head, summed the
	// way sweep sums them.
	Crit *int `json:"crit,omitempty"`
	Imp  *int `json:"imp,omitempty"`
}

// Ref is the PR the event is about.
func (e Event) Ref() Ref { return Ref{Repo: e.Repo, Number: e.PR} }

// Validate checks every field. The store runs it before writing a line and
// after reading one, so a hand-edited or damaged line never reaches a reader.
func (e Event) Validate() error {
	if e.Seq <= 0 {
		return errors.New("event: seq must be positive")
	}
	if _, err := time.Parse(time.RFC3339, e.At); err != nil {
		return errors.New("event: at is not RFC 3339")
	}
	if !kinds[e.Kind] {
		return fmt.Errorf("event: unknown kind")
	}
	if _, err := ParseRef(e.Ref().String()); err != nil {
		return errors.New("event: bad repo or PR number")
	}
	if e.Head != "" && !sha40Re.MatchString(e.Head) {
		return errors.New("event: head is not a full SHA")
	}
	if e.Checks != "" && !checkStates[e.Checks] {
		return errors.New("event: unknown check rollup")
	}
	for _, c := range []*int{e.Crit, e.Imp} {
		if c != nil && (*c < 0 || *c > maxCount) {
			return errors.New("event: review count out of range")
		}
	}
	if (e.Crit == nil) != (e.Imp == nil) {
		return errors.New("event: crit and imp come together")
	}
	if e.Kind == KindReview && e.Crit == nil {
		return errors.New("event: a review event carries its counts")
	}
	if e.Kind == KindChecks && e.Checks == "" {
		return errors.New("event: a checks event carries its rollup")
	}
	return nil
}

// Line is the event as one plain line, the form c3 hands to a session:
//
//	#41 pr.checks cameronsjo/forgectl#12 FAILURE at 0a1b2c3
func (e Event) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s %s", e.Seq, e.Kind, e.Ref())
	if e.Checks != "" {
		b.WriteString(" " + e.Checks)
	}
	if e.Crit != nil && e.Imp != nil {
		fmt.Fprintf(&b, " crit=%d imp=%d", *e.Crit, *e.Imp)
	}
	if e.Head != "" {
		b.WriteString(" at " + e.Head[:7])
	}
	return b.String()
}
