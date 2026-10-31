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
	// Marker is the newest cadence-review marker among the viewer's reviews.
	Marker *Marker `json:"marker,omitempty"`
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
