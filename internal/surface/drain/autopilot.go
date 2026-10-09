package drain

import (
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// The autopilot (atelier P4, T10.4): with [surface.merge] mode "auto", the
// drain makes at most one merge attempt a tick, through the same merge path
// `surface merge` uses, for a reported worker whose launch it can vouch for.
// A merged PR is then closed by the closers at their next read.

// AutopilotEvery is how often the autopilot tries one row: each attempt
// reads the PR in full from GitHub.
const AutopilotEvery = 5 * time.Minute

// AutopilotCandidate reports whether the autopilot may try q: reported, its
// own ledger row readable, still launched, with the queue's launch id, a
// branch the drain made new, and a recorded GitHub repository. Evaluate
// checks all of this again from the facts; this keeps the drain from
// reading GitHub for rows that cannot pass.
func AutopilotCandidate(q worker.QueueRow, l Ledger) bool {
	return q.State == worker.QueueReported && q.LaunchID != "" && l.State == LedgerOurs &&
		l.Row.Stage == worker.StageLaunched && l.Row.LaunchID == q.LaunchID && l.Row.BranchFrom == worker.BranchNew &&
		l.Row.GitHubRepo != "" && l.Row.GitHubRepoID > 0
}

// AutopilotDue reports whether a row last tried at last may be tried again
// at now.
func AutopilotDue(last, now time.Time) bool {
	return last.IsZero() || now.Sub(last) >= AutopilotEvery
}
