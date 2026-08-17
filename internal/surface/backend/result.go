package backend

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
)

// ErrInvalidResult reports a result whose field combination cannot describe a
// real outcome. The service checks for it before acting, so an adapter that
// builds a contradictory result stops the launch rather than steering cleanup.
var ErrInvalidResult = errors.New("surface: adapter returned an invalid result")

// MutationOutcome is what we know about whether the daemon changed state. The
// three values are exhaustive by design: there is no fourth answer, and in
// particular there is no "an error came back so presumably nothing happened".
type MutationOutcome uint8

const (
	// NotMutated is a definitive refusal that reached us before the daemon
	// accepted a creation request.
	NotMutated MutationOutcome = iota
	// RefKnown is an exact object on a server whose incarnation we matched.
	RefKnown
	// OutcomeUnknown is a daemon that may have created something and did not
	// tell us, after one bounded reconciliation failed to settle it.
	OutcomeUnknown

	mutationOutcomeCount
)

var mutationOutcomeNames = [mutationOutcomeCount]string{
	NotMutated:     "not-mutated",
	RefKnown:       "ref-known",
	OutcomeUnknown: "outcome-unknown",
}

func (m MutationOutcome) String() string {
	if m >= mutationOutcomeCount {
		return "invalid(" + strconv.Itoa(int(m)) + ")"
	}
	return mutationOutcomeNames[m]
}

// StartResult is an adapter's answer to Start.
//
// Note that NotMutated is the zero value of MutationOutcome, so a zero
// StartResult looks like "nothing happened" — which would be the most
// dangerous default there is. That is why NotMutated requires a cause: the
// zero value carries none, so it fails Validate and can never be mistaken for
// a real refusal.
//
// The recovery tag is not stored for RefKnown. It is read back off the
// reference, so the two cannot drift apart.
type StartResult struct {
	outcome MutationOutcome

	ref    Ref
	hasRef bool

	tag    RecoveryTag
	hasTag bool

	cause StartCause
	// hasCause records that a constructor *meant* to attach a failure, which
	// is not the same as the cause being non-zero. Without it,
	// NewRefKnownWithCause(ref, StartCause{}) produces a value byte-identical
	// to NewRefKnown(ref) — so the one constructor whose entire purpose is
	// "this failed after creating something" would silently report a clean
	// success when its caller forgot to classify.
	hasCause bool
}

// NewNotMutated reports a definitive failure that happened before the daemon
// accepted anything. The cause is required: "nothing was created" is a claim,
// and a claim with no classified reason behind it is the one an adapter would
// reach for by accident.
func NewNotMutated(cause StartCause) StartResult {
	return StartResult{outcome: NotMutated, cause: cause, hasCause: true}
}

// NewRefKnown reports a successful start with an exact, closeable reference.
func NewRefKnown(ref Ref) StartResult {
	return StartResult{outcome: RefKnown, ref: ref, hasRef: true}
}

// NewRefKnownWithCause reports a failure that happened *after* creation
// succeeded — herdr's workspace-created-but-pane-run-failed shape, for
// instance. The service cleans the exact reference before commit.
//
// It is a separate constructor rather than an optional argument because the
// two calls mean opposite things at the call site, and a variadic would let
// the failing case be written as the succeeding one by omission.
func NewRefKnownWithCause(ref Ref, cause StartCause) StartResult {
	return StartResult{outcome: RefKnown, ref: ref, hasRef: true, cause: cause, hasCause: true}
}

// NewOutcomeUnknown reports an irreducibly ambiguous daemon outcome. It
// carries the ownership tag so an operator can find the object natively, and
// deliberately carries no reference: there is nothing here we are entitled to
// close.
func NewOutcomeUnknown(tag RecoveryTag, cause StartCause) StartResult {
	return StartResult{outcome: OutcomeUnknown, tag: tag, hasTag: true, cause: cause, hasCause: true}
}

// Validate enforces the whole matrix. It is the gate the service runs before
// touching cleanup, and the reason an adapter cannot express "I created
// something, and also nothing was created".
func (r StartResult) Validate() error {
	switch r.outcome {
	case NotMutated:
		if r.hasRef {
			return fmt.Errorf("%w: a not-mutated result carries a reference", ErrInvalidResult)
		}
		if r.hasTag {
			return fmt.Errorf("%w: a not-mutated result carries a recovery tag", ErrInvalidResult)
		}
		if !r.hasCause || !r.cause.Valid() {
			return fmt.Errorf("%w: a not-mutated result needs a classified cause", ErrInvalidResult)
		}
	case RefKnown:
		if !r.hasRef {
			return fmt.Errorf("%w: a ref-known result carries no reference", ErrInvalidResult)
		}
		if err := r.ref.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidResult, err)
		}
		if r.hasTag {
			return fmt.Errorf("%w: a ref-known result carries a separate recovery tag", ErrInvalidResult)
		}
		// A cause is optional here — its presence is what distinguishes a
		// successful start from a post-creation failure — but a constructor
		// that declared one must have classified it.
		if r.hasCause && !r.cause.Valid() {
			return fmt.Errorf("%w: a ref-known result carries an unclassified cause", ErrInvalidResult)
		}
		// And the reverse: a cause sitting in the field that no constructor
		// declared. This is the one shape that flips Failed(), so it reads as
		// a clean success while carrying a real failure — and PlanCleanup then
		// plans no close for a workspace that needs one.
		if !r.hasCause && r.cause.set() {
			return fmt.Errorf("%w: a ref-known result carries an undeclared cause", ErrInvalidResult)
		}
	case OutcomeUnknown:
		if r.hasRef {
			return fmt.Errorf("%w: an outcome-unknown result carries a reference", ErrInvalidResult)
		}
		if !r.hasTag || !r.tag.Valid() {
			return fmt.Errorf("%w: an outcome-unknown result needs a recovery tag", ErrInvalidResult)
		}
		if !r.hasCause || !r.cause.Valid() {
			return fmt.Errorf("%w: an outcome-unknown result needs a classified cause", ErrInvalidResult)
		}
	default:
		return fmt.Errorf("%w: unknown mutation outcome %d", ErrInvalidResult, r.outcome)
	}
	return nil
}

// Outcome returns what we know about the daemon's state.
func (r StartResult) Outcome() MutationOutcome { return r.outcome }

// Ref returns the exact reference, present only for RefKnown.
func (r StartResult) Ref() (Ref, bool) { return r.ref, r.hasRef }

// RecoveryTag returns the ownership tag: the reference's own tag when we have
// a reference, and the reserved tag when the outcome is unknown.
func (r StartResult) RecoveryTag() (RecoveryTag, bool) {
	if r.hasRef {
		return r.ref.Tag(), true
	}
	return r.tag, r.hasTag
}

// Cause returns the classified failure, if any. A RefKnown result without one
// is a successful start.
func (r StartResult) Cause() (StartCause, bool) { return r.cause, r.hasCause }

// Failed reports whether this result ends the launch, regardless of what was
// created. A RefKnown result with a cause has both a live object and a
// failure, and treating either half alone is how one of them gets dropped.
func (r StartResult) Failed() bool { return r.outcome != RefKnown || r.hasCause }

func (r StartResult) String() string {
	s := "surface start: " + r.outcome.String()
	if r.hasCause {
		s += " (" + r.cause.Class().String() + ")"
	}
	return s
}

func (r StartResult) GoString() string { return r.String() }

func (r StartResult) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("mutation", r.outcome.String())}
	if r.hasCause {
		attrs = append(attrs, slog.String("cause", r.cause.Class().String()))
	}
	if tag, ok := r.RecoveryTag(); ok {
		attrs = append(attrs, slog.String("tag", tag.String()))
	}
	return slog.GroupValue(attrs...)
}

// CloseState is the closed set of answers a Closer may give. There is no
// generic error return on Close for the same reason there is none on Start:
// the service has to decide from this value whether a rollback was satisfied,
// and an error string cannot answer that.
type CloseState uint8

const (
	// CloseStateUnspecified is the ineligible zero value.
	CloseStateUnspecified CloseState = iota

	// CloseClosed is the object confirmed gone because we closed it.
	CloseClosed
	// CloseAlreadyGone is the object confirmed absent before we acted.
	CloseAlreadyGone
	// CloseIdentityMismatch is a server that answered but is no longer the
	// incarnation the reference was bound to. We did not close anything, and
	// that is the point.
	CloseIdentityMismatch
	// CloseUnreadable is a server we could not inspect: permission, socket, or
	// protocol.
	CloseUnreadable
	// CloseFailed is a close that was attempted and refused.
	CloseFailed

	closeStateCount
)

var closeStateNames = [closeStateCount]string{
	CloseStateUnspecified: "unspecified",
	CloseClosed:           "closed",
	CloseAlreadyGone:      "already-gone",
	CloseIdentityMismatch: "identity-mismatch",
	CloseUnreadable:       "unreadable",
	CloseFailed:           "failed",
}

func (s CloseState) String() string {
	if s >= closeStateCount {
		return "invalid(" + strconv.Itoa(int(s)) + ")"
	}
	return closeStateNames[s]
}

// SatisfiesRollback reports whether this state discharges the obligation to
// clean up. Only confirmed absence does: an unreadable server or a refused
// close leaves an object behind, and reporting either as a completed rollback
// is the lie the whole typed matrix exists to make unwriteable.
func (s CloseState) SatisfiesRollback() bool {
	return s == CloseClosed || s == CloseAlreadyGone
}

// CloseResult is a Closer's answer.
type CloseResult struct {
	state CloseState
	cause StartCause
}

// NewCloseClosed reports a confirmed close.
func NewCloseClosed() CloseResult { return CloseResult{state: CloseClosed} }

// NewCloseAlreadyGone reports the object was absent before we acted.
func NewCloseAlreadyGone() CloseResult { return CloseResult{state: CloseAlreadyGone} }

// NewCloseIdentityMismatch reports the server is a different incarnation.
func NewCloseIdentityMismatch(cause StartCause) CloseResult {
	return CloseResult{state: CloseIdentityMismatch, cause: cause}
}

// NewCloseUnreadable reports a server we could not inspect.
func NewCloseUnreadable(cause StartCause) CloseResult {
	return CloseResult{state: CloseUnreadable, cause: cause}
}

// NewCloseFailed reports a close that was attempted and refused.
func NewCloseFailed(cause StartCause) CloseResult {
	return CloseResult{state: CloseFailed, cause: cause}
}

// Validate enforces the close matrix: the two success states carry no cause,
// and the three failure states require one.
func (r CloseResult) Validate() error {
	switch r.state {
	case CloseClosed, CloseAlreadyGone:
		if r.cause.set() {
			return fmt.Errorf("%w: a %s close carries a failure cause", ErrInvalidResult, r.state)
		}
	case CloseIdentityMismatch, CloseUnreadable, CloseFailed:
		if !r.cause.Valid() {
			return fmt.Errorf("%w: a %s close needs a classified cause", ErrInvalidResult, r.state)
		}
	default:
		return fmt.Errorf("%w: unknown close state %d", ErrInvalidResult, r.state)
	}
	return nil
}

// State returns the closed classification.
func (r CloseResult) State() CloseState { return r.state }

// Cause returns the classified failure, if any.
func (r CloseResult) Cause() (StartCause, bool) { return r.cause, r.cause.set() }

func (r CloseResult) String() string { return "surface close: " + r.state.String() }

func (r CloseResult) GoString() string { return r.String() }

func (r CloseResult) LogValue() slog.Value { return slog.StringValue(r.state.String()) }

// ProbeState is the closed set of answers a Prober may give.
type ProbeState uint8

const (
	// ProbeStateUnspecified is the ineligible zero value.
	ProbeStateUnspecified ProbeState = iota

	// ProbePresent is the object confirmed to exist on the matching
	// incarnation.
	ProbePresent
	// ProbeGone is the object confirmed absent on the matching incarnation.
	ProbeGone
	// ProbeIdentityMismatch is a server that is no longer the one the
	// reference names, which makes presence and absence both unanswerable.
	ProbeIdentityMismatch
	// ProbeUnreadable is a server we could not inspect.
	ProbeUnreadable

	probeStateCount
)

var probeStateNames = [probeStateCount]string{
	ProbeStateUnspecified: "unspecified",
	ProbePresent:          "present",
	ProbeGone:             "gone",
	ProbeIdentityMismatch: "identity-mismatch",
	ProbeUnreadable:       "unreadable",
}

func (s ProbeState) String() string {
	if s >= probeStateCount {
		return "invalid(" + strconv.Itoa(int(s)) + ")"
	}
	return probeStateNames[s]
}

// Conclusive reports whether this probe answered the existence question. Only
// present and gone do; a mismatched or unreadable server is a probe that ran
// and learned nothing, which must not be read as absence.
func (s ProbeState) Conclusive() bool { return s == ProbePresent || s == ProbeGone }

// ProbeResult is a Prober's answer.
type ProbeResult struct {
	state ProbeState
	cause StartCause
}

// NewProbePresent reports a confirmed existing object.
func NewProbePresent() ProbeResult { return ProbeResult{state: ProbePresent} }

// NewProbeGone reports a confirmed absent object.
func NewProbeGone() ProbeResult { return ProbeResult{state: ProbeGone} }

// NewProbeIdentityMismatch reports a different server incarnation.
func NewProbeIdentityMismatch(cause StartCause) ProbeResult {
	return ProbeResult{state: ProbeIdentityMismatch, cause: cause}
}

// NewProbeUnreadable reports a server we could not inspect.
func NewProbeUnreadable(cause StartCause) ProbeResult {
	return ProbeResult{state: ProbeUnreadable, cause: cause}
}

// Validate enforces the probe matrix, mirroring CloseResult.
func (r ProbeResult) Validate() error {
	switch r.state {
	case ProbePresent, ProbeGone:
		if r.cause.set() {
			return fmt.Errorf("%w: a %s probe carries a failure cause", ErrInvalidResult, r.state)
		}
	case ProbeIdentityMismatch, ProbeUnreadable:
		if !r.cause.Valid() {
			return fmt.Errorf("%w: a %s probe needs a classified cause", ErrInvalidResult, r.state)
		}
	default:
		return fmt.Errorf("%w: unknown probe state %d", ErrInvalidResult, r.state)
	}
	return nil
}

// State returns the closed classification.
func (r ProbeResult) State() ProbeState { return r.state }

// Cause returns the classified failure, if any.
func (r ProbeResult) Cause() (StartCause, bool) { return r.cause, r.cause.set() }

func (r ProbeResult) String() string { return "surface probe: " + r.state.String() }

func (r ProbeResult) GoString() string { return r.String() }

func (r ProbeResult) LogValue() slog.Value { return slog.StringValue(r.state.String()) }

// CleanupOutcome is what the service records about rollback. It is a separate
// vocabulary from CloseState because two of its values describe a close that
// correctly never happened, and folding those into a close state would mean
// inventing a close result for a call nobody made.
type CleanupOutcome uint8

const (
	// CleanupUnspecified is the ineligible zero value.
	CleanupUnspecified CleanupOutcome = iota

	// CleanupNotApplicable is a launch that failed before anything existed.
	CleanupNotApplicable
	// CleanupUnavailableUnknown is an ambiguous outcome with nothing safe to
	// close. This is the honest end state, not a failure to try harder.
	CleanupUnavailableUnknown
	// CleanupSkippedCommitted is a launch that succeeded: ownership moved to
	// the surface, and later caller cancellation must not close it.
	CleanupSkippedCommitted

	// CleanupClosed through CleanupFailed mirror an attempted close.
	CleanupClosed
	CleanupAlreadyGone
	CleanupIdentityMismatch
	CleanupUnreadable
	CleanupFailed

	cleanupOutcomeCount
)

var cleanupOutcomeNames = [cleanupOutcomeCount]string{
	CleanupUnspecified:        "unspecified",
	CleanupNotApplicable:      "no cleanup was needed",
	CleanupUnavailableUnknown: "cleanup was not possible: the outcome is unknown",
	CleanupSkippedCommitted:   "cleanup was not attempted: the surface is committed",
	CleanupClosed:             "the surface was closed",
	CleanupAlreadyGone:        "the surface was already gone",
	CleanupIdentityMismatch:   "cleanup was refused: the server is a different incarnation",
	CleanupUnreadable:         "cleanup could not read the server",
	CleanupFailed:             "cleanup failed",
}

func (o CleanupOutcome) String() string {
	if o >= cleanupOutcomeCount {
		return "invalid(" + strconv.Itoa(int(o)) + ")"
	}
	return cleanupOutcomeNames[o]
}

// Satisfied reports whether rollback left nothing behind.
//
// Four states qualify, for two different reasons. CleanupNotApplicable and
// CleanupSkippedCommitted describe a close that correctly never happened —
// nothing was created, or the launch succeeded and ownership moved to the
// surface. CleanupClosed and CleanupAlreadyGone describe confirmed absence.
// Everything else left an object behind, including CleanupUnavailableUnknown,
// which is the honest end state rather than a failure to try harder.
func (o CleanupOutcome) Satisfied() bool {
	switch o {
	case CleanupNotApplicable, CleanupSkippedCommitted, CleanupClosed, CleanupAlreadyGone:
		return true
	default:
		return false
	}
}

// cleanupOutcomeForClose maps an attempted close onto its record.
func cleanupOutcomeForClose(state CloseState) CleanupOutcome {
	switch state {
	case CloseClosed:
		return CleanupClosed
	case CloseAlreadyGone:
		return CleanupAlreadyGone
	case CloseIdentityMismatch:
		return CleanupIdentityMismatch
	case CloseUnreadable:
		return CleanupUnreadable
	case CloseFailed, CloseStateUnspecified:
		return CleanupFailed
	default:
		return CleanupFailed
	}
}

// CleanupOutcomeFor converts a validated close result into the outcome the
// service records. An invalid close result records a failure: an adapter that
// cannot describe what it did has not demonstrated that it cleaned up.
func CleanupOutcomeFor(res CloseResult) CleanupOutcome {
	if err := res.Validate(); err != nil {
		return CleanupFailed
	}
	return cleanupOutcomeForClose(res.State())
}

// CleanupPlan is the decision of whether rollback may touch the backend at
// all. It exists as a value so the decision is testable without a daemon, and
// so it is made once from the typed state rather than re-derived at each call
// site from an error.
type CleanupPlan struct {
	call    bool
	ref     Ref
	outcome CleanupOutcome
}

// PlanCleanup decides rollback from a start result and whether the launch
// committed. It is the whole of the cleanup matrix:
//
//	not-mutated             -> no call; nothing was created
//	outcome-unknown         -> no call; there is no target we are entitled to
//	ref-known, uncommitted  -> close exactly once
//	ref-known, committed    -> no call; ownership transferred at exec_started
//
// The committed case matters most: after the acknowledgement, caller
// cancellation must not reach into a running surface, so the decision is made
// here from state rather than left to a defer that races the commit.
// A validation failure still plans a close in one narrow case: the result
// claims RefKnown, is uncommitted, and carries a reference that validates on
// its own. That shape is reachable and it matters — an adapter that created a
// workspace, hit a later failure, and forgot to classify it produces exactly
// it, and refusing to plan the close would orphan a real object over a
// bookkeeping mistake. The error comes back alongside, so the caller records
// the malformed result *and* cleans up after it.
//
// The condition is on the claimed outcome, not merely on a reference being
// present. A contradictory result whose outcome says NotMutated or
// OutcomeUnknown is asserting that we do not know we created that object, and
// widening the fallback to close it anyway would hand back exactly the
// guessed-target authority the three-outcome closure exists to withhold.
func PlanCleanup(res StartResult, committed bool) (CleanupPlan, error) {
	if err := res.Validate(); err != nil {
		ref, ok := res.Ref()
		if ok && res.Outcome() == RefKnown && !committed && ref.Validate() == nil {
			return CleanupPlan{call: true, ref: ref}, err
		}
		return CleanupPlan{}, err
	}
	switch res.Outcome() {
	case NotMutated:
		return CleanupPlan{outcome: CleanupNotApplicable}, nil
	case OutcomeUnknown:
		return CleanupPlan{outcome: CleanupUnavailableUnknown}, nil
	case RefKnown:
		ref, _ := res.Ref()
		if committed {
			return CleanupPlan{outcome: CleanupSkippedCommitted}, nil
		}
		return CleanupPlan{call: true, ref: ref}, nil
	default:
		return CleanupPlan{}, fmt.Errorf("%w: unknown mutation outcome %d", ErrInvalidResult, res.Outcome())
	}
}

// Close returns the reference to close and whether to call the Closer at all.
func (p CleanupPlan) Close() (Ref, bool) { return p.ref, p.call }

func (p CleanupPlan) String() string {
	if p.call {
		return "surface cleanup: close " + p.ref.String()
	}
	return "surface cleanup: " + p.outcome.String()
}

func (p CleanupPlan) GoString() string { return p.String() }

func (p CleanupPlan) LogValue() slog.Value { return slog.StringValue(p.String()) }

// Outcome returns the recorded cleanup outcome for a plan that makes no call.
// When the plan does call Close, the outcome comes from CleanupOutcomeFor
// instead, so this returns the ineligible zero value.
func (p CleanupPlan) Outcome() CleanupOutcome { return p.outcome }
