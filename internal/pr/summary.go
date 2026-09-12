package pr

import "time"

// SessionSummary is one PRESENTATION row for `pr list` and `pr dash` — and
// deliberately nothing more.
//
// Session stays ACTIONABLE and live-only: it carries a workspace that callers
// hand to git, sandbox teardown, and tmux, so constructing one is a claim that
// the workspace is real. A summary makes no such claim — it exists precisely
// to describe records whose workspace is GONE, which is why it cannot be fed
// to discard and does not expose the workspace path or the agent at all.
//
// Every field is private and reached through methods. That is not ceremony:
// this type crosses into sibling package internal/cli, so it must be exported,
// and private state is what stops the compiler from letting a caller build one
// by hand — a hand-built summary would be a liveness assertion nobody
// verified. Go's internal/ rule keeps the type out of the supported external
// API regardless.
//
// The availability enum carries no JSON or text marshaling and no public
// labels. Human strings like "workspace missing" are CLI presentation policy,
// which is where they can change without touching this contract.
type SessionSummary struct {
	ref          Ref
	path         string
	createdAt    time.Time
	availability workspaceAvailability
	phase        Phase
	repairReason string
}

// Ref is the reviewed pull request.
func (s SessionSummary) Ref() Ref { return s.ref }

// Phase is what the record SAYS its lifecycle state is — empty on a legacy
// record, which predates phases. It is presentation data, never authority:
// whether a window exists is observed through WindowsLive, not read here.
func (s SessionSummary) Phase() Phase { return s.phase }

// RepairReason is the diagnostic a needs-repair record carries, written at
// the throw site. It is empty on every other phase — validation ties the
// two together — so a non-empty value here always means needs-repair.
func (s SessionSummary) RepairReason() string { return s.repairReason }

// IsWorkspaceNone reports a queued or preparing record, which has no
// workspace yet and is neither live nor missing.
func (s SessionSummary) IsWorkspaceNone() bool {
	return s.availability == workspaceAvailabilityNone
}

// Path is the breadcrumb pathname — the operand `pr teardown` takes.
func (s SessionSummary) Path() string { return s.path }

// CreatedAt is when the review session was recorded.
func (s SessionSummary) CreatedAt() time.Time { return s.createdAt }

// IsWorkspaceLive reports whether the recorded workspace is a live sandbox.
func (s SessionSummary) IsWorkspaceLive() bool {
	return s.availability == workspaceAvailabilityLive
}

// IsWorkspaceMissing reports whether the recorded workspace is cleanly absent.
func (s SessionSummary) IsWorkspaceMissing() bool {
	return s.availability == workspaceAvailabilityMissing
}

// NOTE FOR CONSUMERS: all three predicates are false on the zero value, and
// that is the intended fail-closed shape. A summary for which NO predicate
// holds never comes out of List — it means a summary was constructed outside
// the loader — so a consumer that sees one is looking at an internal error and
// must say so, not invent a label for it.
