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
}

// Ref is the reviewed pull request.
func (s SessionSummary) Ref() Ref { return s.ref }

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

// NOTE FOR CONSUMERS: both predicates are false on the zero value, and that
// is the intended fail-closed shape. A summary for which NEITHER predicate
// holds never comes out of List — it means a summary was constructed outside
// the loader — so a consumer that sees one is looking at an internal error and
// must say so, not invent a label for it.
