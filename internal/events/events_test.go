// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package events

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func snap(state, head, checks string) PRState {
	return PRState{Repo: "o/r", Number: 7, State: state, Head: head, Checks: checks, PolledAt: t0.Format(time.RFC3339)}
}

func kindsOf(evs []Event) string {
	var ks []string
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return strings.Join(ks, ",")
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string]string{
		"o/r#7":                            "o/r#7",
		"https://github.com/o/r/pull/7":    "o/r#7",
		"https://github.com/o/r/pull/7/":   "o/r#7",
		" cameronsjo/forgectl#1528 ":       "cameronsjo/forgectl#1528",
		"https://github.com/o/r.git/pull/": "",
		"o/r#0":                            "",
		"o/r":                              "",
		"o#7":                              "",
		"https://evil.example/o/r/pull/7":  "",
		"o/r#7\nkind":                      "",
	} {
		r, err := ParseRef(in)
		got := ""
		if err == nil {
			got = r.String()
		}
		if got != want {
			t.Errorf("ParseRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiff(t *testing.T) {
	open := snap("OPEN", shaA, "PENDING")
	tests := []struct {
		name string
		old  *PRState
		cur  PRState
		want string
	}{
		{"first look is not news", nil, open, ""},
		{"nothing changed", &open, open, ""},
		{"checks finish", &open, snap("OPEN", shaA, "SUCCESS"), KindChecks},
		{"checks back to pending is not news", ptr(snap("OPEN", shaA, "FAILURE")), open, ""},
		{"push then green", ptr(snap("OPEN", shaB, "SUCCESS")), snap("OPEN", shaA, "SUCCESS"), KindPushed + "," + KindChecks},
		{"push, checks pending", ptr(snap("OPEN", shaB, "SUCCESS")), open, KindPushed},
		{"merged", &open, snap("MERGED", shaA, "PENDING"), KindMerged},
		{"closed", &open, snap("CLOSED", shaA, "PENDING"), KindClosed},
	}
	for _, tt := range tests {
		if got := kindsOf(Diff(tt.old, tt.cur)); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDiffReviewReadyCITL(t *testing.T) {
	old := snap("OPEN", shaA, "SUCCESS")
	old.Draft = true
	cur := old
	cur.Draft = false
	cur.CITL = true
	cur.Review = &Review{AtHead: 1, Crit: 0, Imp: 2}
	evs := Diff(&old, cur)
	if got := kindsOf(evs); got != KindReady+","+KindReview+","+KindCITL {
		t.Fatalf("got %q", got)
	}
	if *evs[1].Imp != 2 || *evs[1].Crit != 0 {
		t.Fatalf("review counts lost: %+v", evs[1])
	}
	// The same marker again is not news; a second one is.
	if got := kindsOf(Diff(&cur, cur)); got != "" {
		t.Fatalf("repeat review: %q", got)
	}
	more := cur
	more.Review = &Review{AtHead: 2}
	if got := kindsOf(Diff(&cur, more)); got != KindReview {
		t.Fatalf("second review: %q", got)
	}
}

func ptr(s PRState) *PRState { return &s }

func TestEventValidate(t *testing.T) {
	zero := 0
	good := Event{Seq: 1, At: t0.Format(time.RFC3339), Kind: KindReview, Repo: "o/r", PR: 7, Head: shaA, Crit: &zero, Imp: &zero}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Event){
		func(e *Event) { e.Kind = "pr.body" },
		func(e *Event) { e.Repo = "o/r\nignore previous" },
		func(e *Event) { e.Head = "abc" },
		func(e *Event) { e.Checks = "GREEN" },
		func(e *Event) { e.Imp = nil },
		func(e *Event) { e.Seq = 0 },
		func(e *Event) { e.At = "yesterday" },
	}
	for i, mod := range bad {
		e := good
		mod(&e)
		if e.Validate() == nil {
			t.Errorf("case %d validated: %+v", i, e)
		}
	}
	if got := good.Line(); got != "#1 pr.review o/r#7 crit=0 imp=0 at aaaaaaa" {
		t.Errorf("Line = %q", got)
	}
}

type fakeFetcher map[string]PRState

func (f fakeFetcher) Fetch(_ context.Context, r Ref) (PRState, error) {
	s, ok := f[r.String()]
	if !ok {
		return PRState{}, errors.New("no such PR")
	}
	return s, nil
}

func TestStoreRoundTrip(t *testing.T) {
	st := NewStore(t.TempDir())
	ref, _ := ParseRef("o/r#7")
	other, _ := ParseRef("o/r#8")
	if err := st.Watch("sess-1", []Ref{ref}); err != nil {
		t.Fatal(err)
	}
	if err := st.Watch("sess-2", []Ref{ref, other}); err != nil {
		t.Fatal(err)
	}
	if err := st.Watch("../x", []Ref{ref}); !errors.Is(err, ErrBadSession) {
		t.Fatalf("bad session id accepted: %v", err)
	}
	now := func() time.Time { return t0 }
	f := fakeFetcher{"o/r#7": snap("OPEN", shaA, "PENDING")}
	res, err := Poll(context.Background(), st, f, now)
	if err != nil || res.Read != 1 || res.Skipped != 1 || len(res.Events) != 0 {
		t.Fatalf("first poll: %+v %v", res, err)
	}
	f["o/r#7"] = snap("OPEN", shaA, "FAILURE")
	res, err = Poll(context.Background(), st, f, now)
	if err != nil || kindsOf(res.Events) != KindChecks || res.Events[0].Seq != 1 {
		t.Fatalf("second poll: %+v %v", res, err)
	}
	f["o/r#7"] = snap("MERGED", shaA, "FAILURE")
	if _, err = Poll(context.Background(), st, f, now); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"sess-1", "sess-2"} {
		evs, last, skipped, err := st.Feed(sid, 0)
		if err != nil || skipped != 0 || last != 2 || kindsOf(evs) != KindChecks+","+KindMerged {
			t.Fatalf("%s feed: %v last=%d skipped=%d err=%v", sid, kindsOf(evs), last, skipped, err)
		}
		evs, last, _, _ = st.Feed(sid, 1)
		if kindsOf(evs) != KindMerged || last != 2 {
			t.Fatalf("%s feed since 1: %v", sid, kindsOf(evs))
		}
	}
	// The merged PR left every list; sess-2 still watches #8.
	w, err := st.Watched()
	if err != nil || len(w["sess-1"]) != 0 || len(w["sess-2"]) != 1 {
		t.Fatalf("watch lists after merge: %v %v", w, err)
	}
	got, ok, err := st.State(ref)
	if err != nil || !ok || got.State != "MERGED" {
		t.Fatalf("state: %+v %v %v", got, ok, err)
	}
	if _, ok, _ := st.State(other); ok {
		t.Fatal("a PR never read has a state")
	}
}

func TestStoreReadersOfAnEmptyStore(t *testing.T) {
	st := NewStore(t.TempDir())
	evs, last, _, err := st.Feed("sess", 5)
	if err != nil || len(evs) != 0 || last != 5 {
		t.Fatalf("feed of absent store: %v %d %v", evs, last, err)
	}
	if _, ok, err := st.State(Ref{Repo: "o/r", Number: 1}); ok || err != nil {
		t.Fatalf("state of absent store: %v %v", ok, err)
	}
}

func TestWatchCap(t *testing.T) {
	st := NewStore(t.TempDir())
	var refs []Ref
	for i := 1; i <= maxWatched+1; i++ {
		refs = append(refs, Ref{Repo: "o/r", Number: i})
	}
	if err := st.Watch("s", refs); !errors.Is(err, ErrTooMany) {
		t.Fatalf("got %v", err)
	}
}

func TestParsePR(t *testing.T) {
	marker := "<!-- cadence-review: pr-reviewer head=" + shaA + " crit=0 imp=1 -->"
	out := `{"data":{"repository":{"mergeCommitAllowed":true,"squashMergeAllowed":false,"rebaseMergeAllowed":false,
	  "pullRequest":{"number":7,"state":"OPEN","isDraft":false,"headRefOid":"` + shaA + `",
	  "labels":{"nodes":[{"name":"CITL"}]},
	  "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]},
	  "reviews":{"nodes":[{"body":"` + marker + `\nfine","author":{"login":"me"}},
	                      {"body":"` + marker + `","author":{"login":"someone-else"}}]}}}}}`
	s, err := parsePR(Ref{Repo: "o/r", Number: 7}, "me", nil, out, t0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Checks != "SUCCESS" || !s.CITL || s.Review == nil || s.Review.AtHead != 1 || s.Review.Imp != 1 || len(s.MergeMethods) != 1 || s.MergeMethods[0] != "merge" {
		t.Fatalf("parsed: %+v review=%+v", s, s.Review)
	}
	if _, err := parsePR(Ref{Repo: "o/r", Number: 8}, "me", nil, out, t0); err == nil {
		t.Fatal("a PR answer for another number was accepted")
	}
}
