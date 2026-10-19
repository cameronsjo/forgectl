package sweep

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

// markerRe matches the first line of a review body that carries a
// cadence-review marker, with or without the HTML comment wrapper the
// chief-of-staff skill posts.
var markerRe = regexp.MustCompile(`^(?:<!--\s*)?cadence-review: ([a-z0-9-]{1,40}) head=([0-9a-f]{40}) crit=(0|[1-9][0-9]{0,3}) imp=(0|[1-9][0-9]{0,3})(?:\s*-->)?$`)

// Marker is a parsed review marker. It is a claim by whoever posted it.
type Marker struct {
	Reviewer string
	Head     string
	Crit     int
	Imp      int
}

// ParseMarker parses one review body's first line.
func ParseMarker(line string) (Marker, bool) {
	m := markerRe.FindStringSubmatch(line)
	if m == nil {
		return Marker{}, false
	}
	crit, _ := strconv.Atoi(m[3]) // the pattern admits digits only
	imp, _ := strconv.Atoi(m[4])
	return Marker{Reviewer: m[1], Head: m[2], Crit: crit, Imp: imp}, true
}

// PRFacts is what GitHub said about one open PR.
type PRFacts struct {
	Repo   string // owner/repo
	Number int
	Title  string
	Draft  bool
	// Head is the PR's current head commit.
	Head string
	// Mergeable is MERGEABLE, CONFLICTING or UNKNOWN.
	Mergeable string
	// Checks is the head commit's rollup state: SUCCESS, FAILURE, ERROR,
	// PENDING, EXPECTED, or "" when it has none.
	Checks    string
	CreatedAt string
	// Reviews holds the first line of each recent review body.
	Reviews []string
}

// ClassifyPR applies the state rule.
//
//   - draft: a draft is never waiting on a reviewer.
//   - blocked: conflicting, checks failing, or a marker at the PR's current
//     head with crit>0.
//   - marker-clean: a marker at the current head with crit=0 (and none with
//     crit>0), green checks, and mergeable.
//   - waiting: any other non-draft PR.
//
// A marker counts only if its head equals the PR's current head AND its
// reviewer is in reviewers. That is a claim filter, not an authenticity
// check: any session posting under the operator's gh identity can post any
// reviewer name, so marker-clean is a claim and never "ready".
func ClassifyPR(f PRFacts, reviewers []string) (state, reason string) {
	if f.Draft {
		return StateDraft, "draft"
	}
	var atHead, stale, crit int
	for _, line := range f.Reviews {
		m, ok := ParseMarker(line)
		if !ok || !slices.Contains(reviewers, m.Reviewer) {
			continue
		}
		if f.Head == "" || m.Head != f.Head {
			stale++
			continue
		}
		atHead++
		crit += m.Crit
	}
	switch {
	case f.Mergeable == "CONFLICTING":
		return StateBlocked, "conflicting"
	case f.Checks == "FAILURE" || f.Checks == "ERROR":
		return StateBlocked, "checks failing"
	case crit > 0:
		return StateBlocked, fmt.Sprintf("marker at head has crit=%d", crit)
	case atHead == 0 && stale > 0:
		return StateWaiting, "marker is for an older head"
	case atHead == 0:
		return StateWaiting, "no marker at head"
	case f.Checks != "SUCCESS":
		return StateWaiting, "checks not green"
	case f.Mergeable != "MERGEABLE":
		return StateWaiting, "mergeability unknown"
	}
	return StateMarkerClean, "marker at head with crit=0, checks green, mergeable"
}

// PRID is a PR's stable id.
func PRID(repo string, number int) string { return fmt.Sprintf("pr:%s#%d", repo, number) }

// WorkerID is a worker row's stable id: state is the worker's condition the
// row is about (blocked, plan, report).
func WorkerID(name, state string) string { return "worker:" + name + ":" + state }
