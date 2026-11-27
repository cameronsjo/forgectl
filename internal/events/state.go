// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package events

import (
	"errors"
	"time"
)

// PR states a snapshot may carry.
var prStates = map[string]bool{"OPEN": true, "MERGED": true, "CLOSED": true}

// Merge methods a repository may allow.
var mergeMethods = map[string]bool{"merge": true, "squash": true, "rebase": true}

// Review is the cadence-review markers at a PR's head, summed by sweep's
// rule. Reviewers is not kept: a reviewer name is a claim anyone posting
// under the operator's login can make, and the gate needs only the counts.
type Review struct {
	AtHead int `json:"at_head"` // markers at the head that count
	Stale  int `json:"stale"`   // counted markers for another head
	Crit   int `json:"crit"`
	Imp    int `json:"imp"`
}

// PRState is the last thing the poller read about one PR. cadence-hooks'
// merge gate reads it; PolledAt says how old it is, and the reader decides
// what is too old.
type PRState struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	State  string `json:"state"` // OPEN, MERGED or CLOSED
	Draft  bool   `json:"draft"`
	Head   string `json:"head,omitempty"`
	Checks string `json:"checks,omitempty"` // the head's rollup, "" when it has none
	// Review is nil when no marker counts.
	Review *Review `json:"review,omitempty"`
	// CITL is true while the PR carries the CITL label.
	CITL bool `json:"citl"`
	// MergeMethods are the methods the repository allows, sorted.
	MergeMethods []string `json:"merge_methods,omitempty"`
	PolledAt     string   `json:"polled_at"` // RFC 3339, UTC
}

// Ref is the PR the snapshot is about.
func (s PRState) Ref() Ref { return Ref{Repo: s.Repo, Number: s.Number} }

// Validate checks every field, on write and after read.
func (s PRState) Validate() error {
	if _, err := ParseRef(s.Ref().String()); err != nil {
		return errors.New("state: bad repo or PR number")
	}
	if !prStates[s.State] {
		return errors.New("state: unknown PR state")
	}
	if s.Head != "" && !sha40Re.MatchString(s.Head) {
		return errors.New("state: head is not a full SHA")
	}
	if s.Checks != "" && !checkStates[s.Checks] {
		return errors.New("state: unknown check rollup")
	}
	if r := s.Review; r != nil {
		for _, n := range []int{r.AtHead, r.Stale, r.Crit, r.Imp} {
			if n < 0 || n > maxCount {
				return errors.New("state: review count out of range")
			}
		}
	}
	for _, m := range s.MergeMethods {
		if !mergeMethods[m] {
			return errors.New("state: unknown merge method")
		}
	}
	if _, err := time.Parse(time.RFC3339, s.PolledAt); err != nil {
		return errors.New("state: polled_at is not RFC 3339")
	}
	return nil
}

// Diff is the events between two snapshots of one PR, in a fixed order,
// with no Seq or At (the store stamps them). A first snapshot (old nil)
// yields none: watching a PR is not news about it.
func Diff(old *PRState, cur PRState) []Event {
	if old == nil {
		return nil
	}
	base := Event{Repo: cur.Repo, PR: cur.Number, Head: cur.Head}
	var out []Event
	add := func(kind string, mod func(*Event)) {
		e := base
		e.Kind = kind
		if mod != nil {
			mod(&e)
		}
		out = append(out, e)
	}
	headMoved := cur.Head != "" && cur.Head != old.Head
	if headMoved {
		add(KindPushed, nil)
	}
	if old.Draft && !cur.Draft && cur.State == "OPEN" {
		add(KindReady, nil)
	}
	// Checks: news when a rollup finishes, or a finished one changes. A
	// rollup going back to pending is not news; the push that caused it is.
	if terminalChecks[cur.Checks] && (headMoved || cur.Checks != old.Checks) {
		add(KindChecks, func(e *Event) { e.Checks = cur.Checks })
	}
	if r := cur.Review; r != nil && r.AtHead > 0 {
		before := 0
		if old.Review != nil && !headMoved {
			before = old.Review.AtHead
		}
		if r.AtHead > before {
			crit, imp := r.Crit, r.Imp
			add(KindReview, func(e *Event) { e.Crit, e.Imp = &crit, &imp })
		}
	}
	if cur.CITL && !old.CITL {
		add(KindCITL, nil)
	}
	if old.State == "OPEN" {
		switch cur.State {
		case "MERGED":
			add(KindMerged, nil)
		case "CLOSED":
			add(KindClosed, nil)
		}
	}
	return out
}
