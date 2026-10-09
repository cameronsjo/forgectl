package drain

import (
	"fmt"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// The closers (atelier P4, T10.3): a reported worker is closed once its PR
// merges, or ClosedGrace after its PR closes unmerged. The PR is found the
// way `surface status` finds it (merge.SelectPR): its head branch on the
// repository the ledger row recorded, not a fork, by the operator, at most
// one open.

const (
	// CloserReadEvery is how often the drain reads one reported row's PR.
	CloserReadEvery = 5 * time.Minute
	// ClosedGrace is how long a PR may stay closed unmerged before the
	// drain closes its worker.
	ClosedGrace = 24 * time.Hour
	// CloserReadsPerTick caps the PR reads one tick makes, so a queue of
	// reported rows costs a bounded number of GitHub calls per tick.
	CloserReadsPerTick = 4
)

// PRState is a worker PR's state as the closer reads it.
type PRState string

const (
	// PRNone: no PR on the worker's branch passes the discovery filter.
	PRNone PRState = "none"
	// PROpen, PRClosed and PRMerged are GitHub's pull request states.
	PROpen   PRState = "OPEN"
	PRClosed PRState = "CLOSED"
	PRMerged PRState = "MERGED"
)

// PRRead is one read of a worker's PR.
type PRRead struct {
	State  PRState
	Number int
}

// CloserCandidate reports whether the closer acts on q: a reported row whose
// own ledger row is readable and not closed. A ledger row that is closed,
// gone or another launch's is Settle's to handle; an unreadable one waits.
// identity is false when the ledger row records no GitHub repository: such
// a row is skipped and stays reported until the operator closes it.
func CloserCandidate(q worker.QueueRow, l Ledger) (candidate, identity bool) {
	if q.State != worker.QueueReported || q.LaunchID == "" || l.State != LedgerOurs || l.Row.Stage == worker.StageClosed {
		return false, false
	}
	return true, l.Row.GitHubRepo != "" && l.Row.GitHubRepoID > 0
}

// CloserDue reports whether a row last read at last may be read again at now.
func CloserDue(last, now time.Time) bool {
	return last.IsZero() || now.Sub(last) >= CloserReadEvery
}

// CloserDecision is what one PR read decides for a reported row.
type CloserDecision struct {
	// Change sets or clears pr_closed_at; it never changes the state.
	Change Change
	// Note says what Change does, for the drain's events.
	Note string
	// Close asks for the same close `surface close` runs; the row goes to
	// closed with Why as its last_error when the close does not refuse.
	Close bool
	// Merged asks for the session to be priced before the close.
	Merged bool
	Why    string
}

// DecideCloser decides what a reported row's PR read means:
//
//   - merged: close it, pricing the session first.
//   - closed unmerged: record pr_closed_at the first time; close once it is
//     ClosedGrace old.
//   - open: clear a recorded pr_closed_at (the PR was reopened).
//   - no PR: nothing; the row stays reported.
func DecideCloser(q worker.QueueRow, pr PRRead, now time.Time) CloserDecision {
	d := CloserDecision{Change: change(q)}
	if q.State != worker.QueueReported {
		return d
	}
	switch pr.State {
	case PRMerged:
		d.Close, d.Merged = true, true
		d.Why = fmt.Sprintf("PR #%d merged; the drain closed the worker", pr.Number)
	case PRClosed:
		if q.PRClosedAt == nil {
			at := now.UTC().Truncate(time.Second)
			d.Change.PRClosedAt, d.Change.SetPRClosedAt = &at, true
			d.Note = fmt.Sprintf("PR #%d closed unmerged; the drain closes the worker after %s", pr.Number, at.Add(ClosedGrace).Format(time.RFC3339))
			return d
		}
		if now.Sub(*q.PRClosedAt) >= ClosedGrace {
			d.Close = true
			d.Why = fmt.Sprintf("PR #%d closed unmerged at %s, %s ago; the drain closed the worker", pr.Number, q.PRClosedAt.UTC().Format(time.RFC3339), ClosedGrace)
		}
	case PROpen:
		if q.PRClosedAt != nil {
			d.Change.PRClosedAt, d.Change.SetPRClosedAt = nil, true
			d.Note = fmt.Sprintf("PR #%d is open again; the close timer is cleared", pr.Number)
		}
	}
	return d
}
