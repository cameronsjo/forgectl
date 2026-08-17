package backend

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// StartFailureClass is the closed vocabulary a backend failure is allowed to
// speak. It exists because the alternative — deciding rollback from an error
// string — means a backend's stderr wording becomes forgectl's control flow,
// and a wording change becomes a silent behavior change.
type StartFailureClass uint8

const (
	// FailureUnspecified is the ineligible zero value.
	FailureUnspecified StartFailureClass = iota

	// FailureUnavailable is a backend that is not running or not installed.
	FailureUnavailable
	// FailureIncompatible is a backend whose version or protocol this build
	// does not speak.
	FailureIncompatible
	// FailurePermissionDenied is a socket, config, or workspace the current
	// user may not touch.
	FailurePermissionDenied
	// FailureAuthentication is a backend that refused our credentials.
	FailureAuthentication
	// FailureNameCollision is a definitive pre-mutation refusal to reuse a
	// name. It is the only class that may be retried, and only because it is
	// the only one a backend reports *before* creating anything.
	FailureNameCollision
	// FailureMalformedResponse is a reply this build cannot parse. It is not
	// evidence that nothing was created.
	FailureMalformedResponse
	// FailureTimeout is a bounded wait that elapsed.
	FailureTimeout
	// FailureCanceled is the caller's context ending the attempt.
	FailureCanceled
	// FailureIdentityMismatch is a server that answered but is no longer the
	// incarnation we fingerprinted.
	FailureIdentityMismatch
	// FailureInternal is a defect on our side: a validation we should have
	// run earlier, an impossible state, a programming error.
	FailureInternal

	failureClassCount
)

var failureClassNames = [failureClassCount]string{
	FailureUnspecified:       "unspecified",
	FailureUnavailable:       "backend-unavailable",
	FailureIncompatible:      "backend-incompatible",
	FailurePermissionDenied:  "permission-denied",
	FailureAuthentication:    "authentication-failed",
	FailureNameCollision:     "name-collision",
	FailureMalformedResponse: "malformed-response",
	FailureTimeout:           "timeout",
	FailureCanceled:          "canceled",
	FailureIdentityMismatch:  "identity-mismatch",
	FailureInternal:          "internal",
}

// Valid reports whether c names a real class. The zero value does not.
func (c StartFailureClass) Valid() bool {
	return c > FailureUnspecified && c < failureClassCount
}

func (c StartFailureClass) String() string {
	if c >= failureClassCount {
		return "invalid(" + strconv.Itoa(int(c)) + ")"
	}
	return failureClassNames[c]
}

// Retryable reports whether a mutation may be attempted again after this
// class. Only a definitive pre-mutation refusal qualifies: every other class
// leaves open the possibility that the first request already mutated the
// daemon, and a retry would then create a second workspace nobody owns.
func (c StartFailureClass) Retryable() bool { return c == FailureNameCollision }

// StartCause is a backend failure reduced to its class, carrying the original
// error where only the package can reach it.
//
// The original is deliberately unreachable from outside. There is no Unwrap
// and no As, so errors.As cannot walk to it and no caller can obtain the value
// in order to render it — which matters because the errors this wraps come
// from manager CLIs whose stderr can contain the socket path, the cwd, or an
// echo of the command line. Error, Format, LogValue, and the marshalers all
// render the class alone.
//
// What the original is still good for is classification, and that is exposed
// through Is: errors.Is(cause, context.DeadlineExceeded) works and answers a
// question, without ever handing over a value that could be printed.
type StartCause struct {
	class StartFailureClass
	err   error
}

// NewStartCause records a classified failure. A cause built with an invalid
// class does not validate, so a caller that forgets to classify cannot produce
// a result the service will act on.
func NewStartCause(class StartFailureClass, err error) StartCause {
	return StartCause{class: class, err: err}
}

// Class returns the closed classification.
func (c StartCause) Class() StartFailureClass { return c.class }

// set reports whether this cause was constructed at all. It is distinct from
// validity: a cause with an unknown class is set but invalid.
func (c StartCause) set() bool { return c.class != FailureUnspecified || c.err != nil }

// Valid reports whether this cause carries a real class.
func (c StartCause) Valid() bool { return c.class.Valid() }

// Error renders the class and nothing else. This is the whole containment
// mechanism for a cause that reaches a terminal or a log.
func (c StartCause) Error() string { return "surface: " + c.class.String() }

func (c StartCause) String() string   { return c.Error() }
func (c StartCause) GoString() string { return c.Error() }

func (c StartCause) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(c.Error()))
		return
	}
	_, _ = io.WriteString(f, c.Error())
}

func (c StartCause) LogValue() slog.Value { return slog.StringValue(c.class.String()) }

func (c StartCause) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(c.class.String())), nil
}

func (c StartCause) MarshalText() ([]byte, error) { return []byte(c.Error()), nil }

// Is lets errors.Is reach the original cause's identity without letting any
// caller reach its value. errors.Is consults this method; errors.As does not,
// and the absence of Unwrap stops the chain here, so classification is
// available and rendering is not.
func (c StartCause) Is(target error) bool {
	if c.err == nil {
		return false
	}
	return errors.Is(c.err, target)
}

// LaunchError is the single error type a surface launch returns. It keeps the
// four things a caller needs to act on separate, because collapsing them is
// how "cleanup failed" gets reported as "launch failed" and an orphaned
// workspace goes unmentioned.
type LaunchError struct {
	// Phase names where the launch stopped.
	Phase Phase
	// Backend is the manager involved, or KindUnspecified before one is
	// selected.
	Backend Kind
	// Mutation is what we know about whether the daemon changed state.
	Mutation MutationOutcome
	// Cause is the primary failure, class-only.
	Cause StartCause
	// Cleanup is what rollback did, or why it was not attempted. It is
	// deliberately beside the cause rather than replacing it: a failed close
	// after a failed create is two facts, not one.
	Cleanup CleanupOutcome
	// Recovery carries the ownership tag for an outcome the daemon left
	// ambiguous, so an operator can find the object natively. It is zero
	// unless Mutation is OutcomeUnknown.
	Recovery RecoveryTag
}

func (e *LaunchError) Error() string {
	msg := "surface: launch failed in " + e.Phase.String() + ": " + e.Cause.Class().String()
	if e.Backend.Valid() {
		msg = "surface: " + e.Backend.String() + " launch failed in " + e.Phase.String() + ": " + e.Cause.Class().String()
	}
	switch e.Mutation {
	case OutcomeUnknown:
		msg += "; the backend may have created a workspace and did not say so"
		if e.Recovery.Valid() {
			msg += " (look for " + e.Recovery.OwnershipName() + ")"
		}
	case RefKnown:
		msg += "; " + e.Cleanup.String()
	case NotMutated:
	}
	return msg
}

func (e *LaunchError) GoString() string { return e.Error() }

// Unwrap exposes the class-only cause, not the backend original. errors.Is
// against a StartCause therefore works, and the chain still stops before
// anything renderable.
func (e *LaunchError) Unwrap() error { return e.Cause }

func (e *LaunchError) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("phase", e.Phase.String()),
		slog.String("backend", e.Backend.String()),
		slog.String("mutation", e.Mutation.String()),
		slog.String("cause", e.Cause.Class().String()),
		slog.String("cleanup", e.Cleanup.String()),
	)
}

// Phase names the point a launch reached. It is recorded in logs, so like
// every other value here it is a closed enum rather than free text.
type Phase uint8

const (
	// PhaseUnspecified is the ineligible zero value.
	PhaseUnspecified Phase = iota

	// PhaseResolve is invocation, policy, and target validation. Nothing
	// outside forgectl has been touched.
	PhaseResolve
	// PhasePreflight is backend readiness and capability checks.
	PhasePreflight
	// PhaseListen is private directory and socket setup.
	PhaseListen
	// PhaseCreate is the mutating call to the manager.
	PhaseCreate
	// PhaseReconcile is the one bounded read-only pass that follows a lost or
	// malformed create response.
	PhaseReconcile
	// PhaseHandshake is peer authentication and nonce verification.
	PhaseHandshake
	// PhaseDeliver is sending the invocation frame.
	PhaseDeliver
	// PhaseCommit is awaiting the exec_started acknowledgement.
	PhaseCommit

	phaseCount
)

var phaseNames = [phaseCount]string{
	PhaseUnspecified: "unspecified",
	PhaseResolve:     "resolve",
	PhasePreflight:   "preflight",
	PhaseListen:      "listen",
	PhaseCreate:      "create",
	PhaseReconcile:   "reconcile",
	PhaseHandshake:   "handshake",
	PhaseDeliver:     "deliver",
	PhaseCommit:      "commit",
}

// Valid reports whether p names a real phase. The zero value does not.
func (p Phase) Valid() bool { return p > PhaseUnspecified && p < phaseCount }

func (p Phase) String() string {
	if p >= phaseCount {
		return "invalid(" + strconv.Itoa(int(p)) + ")"
	}
	return phaseNames[p]
}
