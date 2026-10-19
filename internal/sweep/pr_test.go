package sweep

import (
	"strconv"
	"strings"
	"testing"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func marker(reviewer, head string, crit, imp int) string {
	return "<!-- cadence-review: " + reviewer + " head=" + head + " crit=" + itoa(crit) + " imp=" + itoa(imp) + " -->"
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestParseMarker(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		ok         bool
	}{
		{"wrapped", marker("pr-reviewer", headA, 0, 2), true},
		{"bare", "cadence-review: pr-reviewer head=" + headA + " crit=0 imp=0", true},
		{"short head", "cadence-review: pr-reviewer head=abc123 crit=0 imp=0", false},
		{"upper head", "cadence-review: pr-reviewer head=" + strings.ToUpper(headA) + " crit=0 imp=0", false},
		{"leading zero count", "cadence-review: pr-reviewer head=" + headA + " crit=01 imp=0", false},
		{"trailing prose", marker("pr-reviewer", headA, 0, 0) + " lgtm", false},
		{"not a marker", "Looks good to me", false},
	} {
		if _, ok := ParseMarker(tc.line); ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
		}
	}
	m, _ := ParseMarker(marker("security-reviewer", headB, 3, 1))
	if m.Reviewer != "security-reviewer" || m.Head != headB || m.Crit != 3 || m.Imp != 1 {
		t.Errorf("parsed = %+v", m)
	}
}

func TestClassifyPR(t *testing.T) {
	allow := []string{"pr-reviewer", "security-reviewer"}
	green := PRFacts{Repo: "acme/widget", Number: 1, Head: headA, Mergeable: "MERGEABLE", Checks: "SUCCESS"}
	with := func(f PRFacts, mut func(*PRFacts)) PRFacts { mut(&f); return f }
	for _, tc := range []struct {
		name  string
		facts PRFacts
		want  string
	}{
		{"clean marker at head", with(green, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)} }), StateMarkerClean},
		{"stale-head marker is not marker-clean", with(green, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headB, 0, 0)} }), StateWaiting},
		{"non-allowlisted reviewer does not count", with(green, func(f *PRFacts) { f.Reviews = []string{marker("worker-self-review", headA, 0, 0)} }), StateWaiting},
		{"no marker", green, StateWaiting},
		{"crit at head blocks", with(green, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headA, 2, 0)} }), StateBlocked},
		{"crit on a stale head does not block", with(green, func(f *PRFacts) { f.Reviews = []string{marker("pr-reviewer", headB, 2, 0)} }), StateWaiting},
		{"crit from an unlisted reviewer does not block", with(green, func(f *PRFacts) { f.Reviews = []string{marker("someone", headA, 2, 0)} }), StateWaiting},
		{"clean plus crit at head blocks", with(green, func(f *PRFacts) {
			f.Reviews = []string{marker("pr-reviewer", headA, 0, 0), marker("security-reviewer", headA, 1, 0)}
		}), StateBlocked},
		{"conflicting blocks", with(green, func(f *PRFacts) { f.Mergeable = "CONFLICTING" }), StateBlocked},
		{"failing checks block", with(green, func(f *PRFacts) { f.Checks = "FAILURE" }), StateBlocked},
		{"pending checks wait", with(green, func(f *PRFacts) {
			f.Checks = "PENDING"
			f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)}
		}), StateWaiting},
		{"no checks is not green", with(green, func(f *PRFacts) {
			f.Checks = ""
			f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)}
		}), StateWaiting},
		{"unknown mergeability waits", with(green, func(f *PRFacts) {
			f.Mergeable = "UNKNOWN"
			f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)}
		}), StateWaiting},
		{"draft", with(green, func(f *PRFacts) {
			f.Draft = true
			f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)}
		}), StateDraft},
		{"empty head matches nothing", with(green, func(f *PRFacts) {
			f.Head = ""
			f.Reviews = []string{marker("pr-reviewer", headA, 0, 0)}
		}), StateWaiting},
	} {
		got, reason := ClassifyPR(tc.facts, allow)
		if got != tc.want {
			t.Errorf("%s: state = %s (%s), want %s", tc.name, got, reason, tc.want)
		}
		if reason == "" {
			t.Errorf("%s: no reason", tc.name)
		}
	}
}

func TestIDs(t *testing.T) {
	if got := PRID("acme/widget", 42); got != "pr:acme/widget#42" {
		t.Errorf("PRID = %q", got)
	}
	if got := WorkerID("cos-widget", "blocked"); got != "worker:cos-widget:blocked" {
		t.Errorf("WorkerID = %q", got)
	}
}
