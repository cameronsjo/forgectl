// Package workstate maps forgectl's native states onto one shared status
// vocabulary, so every --json row says the same thing in the same words:
// where the work stands, and while it is open, who owes the next step.
//
// The native states stay what each store records. queue.json and drain.json
// decode strictly, so a renamed value would make them unreadable to an older
// binary after a rollback; the shared words are computed on read and printed
// beside the native state, never stored.
//
// The vocabulary (docs/status-vocabulary.md):
//
//   - open: queued, working, delivered, blocked, stalled
//   - ended: resolved, dropped, expired
//
// "done" is not a status. A worker that says it finished is delivered; work
// is resolved only with evidence that its goal was met.
package workstate

import "strings"

// Status is where a piece of work stands.
type Status string

const (
	// Queued: accepted and not started; it waits only for capacity.
	Queued Status = "queued"
	// Working: an agent or process is acting on it now.
	Working Status = "working"
	// Delivered: the actor says it is finished and nobody has verified it.
	// On names the verifier.
	Delivered Status = "delivered"
	// Blocked: it cannot move until the party On names acts.
	Blocked Status = "blocked"
	// Stalled: nobody is acting on it and nobody is named to: a lost run, a
	// stale drain. Unowned work is the operator's to look at.
	Stalled Status = "stalled"

	// Resolved: the goal was met. Evidence says how that is known.
	Resolved Status = "resolved"
	// Dropped: it ended on purpose without meeting its goal. Reason says why.
	Dropped Status = "dropped"
	// Expired: a timeout ended it and nobody decided.
	Expired Status = "expired"
)

// Ended reports whether s ends the work.
func (s Status) Ended() bool {
	return s == Resolved || s == Dropped || s == Expired
}

// Party is who owes the next step on delivered or blocked work.
type Party string

const (
	// Operator is the person at the machine: the one the needs-you inbox is
	// for.
	Operator Party = "operator"
	// ChiefOfStaff is the chief-of-staff session that queued the work and
	// verifies its reports.
	ChiefOfStaff Party = "chief-of-staff"
	// System is forgectl or a service it depends on, which retries by
	// itself: herdr, GitHub.
	System Party = "system"
)

// Work is the shared status of one row, printed as its "work" object.
// On is set for delivered and blocked work only. Reason is a few words of
// why, free text; Evidence backs a resolved status.
type Work struct {
	Status   Status `json:"status"`
	On       Party  `json:"on,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// NeedsOperator reports whether w waits on the operator: blocked or
// delivered on them, or stalled with nobody named.
func (w Work) NeedsOperator() bool {
	return w.On == Operator || w.Status == Stalled
}

// chiefOfStaffBatch is the batch prefix the chief of staff gives the rows it
// queues (`cos-<date>`), the only sign on a row that a session will verify
// its report.
const chiefOfStaffBatch = "cos-"

func verifierFor(batch string) Party {
	if strings.HasPrefix(batch, chiefOfStaffBatch) {
		return ChiefOfStaff
	}
	return Operator
}
