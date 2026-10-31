package workerwatch

import (
	"context"
	"errors"
	"slices"
)

// Runner is the process seam for git and gh: exec.Runner satisfies it. hc
// does not go through it (see Source).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// PR is what GitHub says about a worker's pull request.
type PR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// State is OPEN, MERGED or CLOSED.
	State string `json:"state"`
	Draft bool   `json:"draft"`
	Head  string `json:"head,omitempty"`
	// Checks is the head commit's rollup: SUCCESS, FAILURE, ERROR, PENDING,
	// EXPECTED, or "" when it has none.
	Checks string `json:"checks,omitempty"`
	// Markers is every cadence-review marker among the viewer's last 20
	// reviews, newest first. Each is a claim: anything posting under the
	// operator's gh login, a worker included, can post any name.
	Markers []Marker `json:"markers,omitempty"`
	// Review is Markers summed by sweep's rule (SummarizeMarkers); nil when
	// no marker counts.
	Review *Review `json:"review,omitempty"`
}

// Review is the headline verdict: sweep's rule over the PR's markers.
type Review struct {
	// AtHead counts the markers at the PR's head that count.
	AtHead int `json:"at_head"`
	// Stale counts the counted markers for another head.
	Stale int `json:"stale"`
	// Crit and Imp are summed over every counted marker at head.
	Crit int `json:"crit"`
	Imp  int `json:"imp"`
	// Reviewers names the reviewers at head, first seen first.
	Reviewers []string `json:"reviewers,omitempty"`
	// Filtered is true when [sweep] reviewers limited which names count.
	Filtered bool `json:"filtered"`
}

// Marker is a parsed cadence-review marker: a claim by whoever posted it.
type Marker struct {
	Reviewer string `json:"reviewer"`
	Head     string `json:"head"`
	Crit     int    `json:"crit"`
	Imp      int    `json:"imp"`
	AtHead   bool   `json:"at_head"`
}

var (
	// ErrNoPR means the branch has no PR.
	ErrNoPR = errors.New("no PR from this branch")
	// ErrNotGitHub means the worktree's origin is not a github.com repository.
	ErrNotGitHub = errors.New("origin is not a github.com repository")
)

// Git is the read-only git a worktree's repo and branch come from. The
// implementation lives in ghsource, so internal/tui never links internal/pr.
type Git interface {
	// Branch is the worktree's current branch ("" when detached).
	Branch(ctx context.Context, worktree string) (string, error)
	// Repo is the worktree's origin as owner/name, or ErrNotGitHub.
	Repo(ctx context.Context, worktree string) (string, error)
}

// SummarizeMarkers applies sweep's marker rule (sweep.ClassifyPR) so the
// watch is never less alarming than the sweep beside it: only reviewers in
// allow count (every reviewer when allow is empty), and crit and imp are
// summed over every counted marker at head, with no per-reviewer
// deduplication, so a later clean pass cannot cancel an earlier crit>0.
// It returns nil when no marker counts.
func SummarizeMarkers(ms []Marker, allow []string) *Review {
	r := Review{Filtered: len(allow) > 0}
	for _, m := range ms {
		if r.Filtered && !slices.Contains(allow, m.Reviewer) {
			continue
		}
		if !m.AtHead {
			r.Stale++
			continue
		}
		r.AtHead++
		r.Crit += m.Crit
		r.Imp += m.Imp
		if !slices.Contains(r.Reviewers, m.Reviewer) {
			r.Reviewers = append(r.Reviewers, m.Reviewer)
		}
	}
	if r.AtHead == 0 && r.Stale == 0 {
		return nil
	}
	return &r
}
