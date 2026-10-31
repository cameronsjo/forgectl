package workerwatch

import (
	"context"
	"errors"
)

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
	// Markers is the newest cadence-review marker per reviewer name among
	// the viewer's reviews, newest first. Each is a claim: anything posting
	// under the operator's gh login, a worker included, can post any name.
	Markers []Marker `json:"markers,omitempty"`
	// Marker is the one the row shows: WorstMarker of Markers.
	Marker *Marker `json:"marker,omitempty"`
	// MarkersAtHead counts Markers at the PR's head.
	MarkersAtHead int `json:"markers_at_head,omitempty"`
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

// WorstMarker picks the marker a row shows, so a clean later review never
// hides a worse one at the same head: among markers at head, the most crit,
// then the most imp; with none at head, the newest (which is stale).
func WorstMarker(ms []Marker) *Marker {
	var worst *Marker
	for i := range ms {
		m := &ms[i]
		if !m.AtHead {
			continue
		}
		if worst == nil || m.Crit > worst.Crit || (m.Crit == worst.Crit && m.Imp > worst.Imp) {
			worst = m
		}
	}
	if worst == nil && len(ms) > 0 {
		worst = &ms[0]
	}
	if worst == nil {
		return nil
	}
	cp := *worst
	return &cp
}

// AtHeadCount counts the markers at the PR's head.
func AtHeadCount(ms []Marker) int {
	n := 0
	for _, m := range ms {
		if m.AtHead {
			n++
		}
	}
	return n
}
