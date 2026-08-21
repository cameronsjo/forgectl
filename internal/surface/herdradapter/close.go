package herdradapter

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Close removes exactly the workspace a reference names, after proving the
// session is still the incarnation the reference was bound to AND that the
// workspace is one we created.
func (a *Adapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	workspace, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewCloseIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewCloseUnreadable(cause)
	case locateAbsent:
		return backend.NewCloseAlreadyGone()
	case locateFound:
		// Fall through. The switch is written so ONLY this state reaches the
		// close — see the default below.
	default:
		// A state nobody has taught this switch about must not reach the close.
		// A CONSTRUCTION guard, not a tested control: every state locate can
		// return is handled above, so nothing drives it and its mutation
		// survives. What IS tested is the enum beneath it — locateInvalid owning
		// zero. The tmux adapter had exactly this bug with locateFound at zero,
		// which put the guard on the wrong side of the value it named.
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureInternal,
			errors.New("the workspace lookup produced no usable state")))
	}

	res, err := a.run.RunSensitive(ctx, a.command(exec.KindHerdrCleanup,
		exec.MustFixed("workspace"),
		exec.MustFixed("close"),
		// The workspace id is the operand and it is deliberately the only thing
		// close targets. herdr addresses tabs and panes by qualifying a
		// workspace with a colon, so a colon-bearing id here would widen what
		// this command reaches — which is why NewHerdrIdentity refuses one in
		// the workspace field.
		//
		// NO end-of-options separator: `herdr workspace close -- <id>` answers
		// `usage: herdr workspace close <workspace_id>` and closes nothing.
		// Measured, after a live rollback failed for exactly this reason. What
		// the separator was standing in for is still enforced — exec.Opaque
		// refuses a dash-leading operand — so an id herdr could misread as a
		// flag is refused before start instead of being passed with a separator
		// herdr ignores. validHerdrID deliberately leaves that rule to the seam.
		exec.Opaque(workspace),
	))
	if err != nil {
		// A close that raced another is a satisfied rollback: the obligation was
		// that the object be gone, and it is. Matched on herdr's structured
		// CODE rather than its prose, so a reworded message cannot turn a
		// satisfied rollback into a failed one.
		if errorCode(res.Stderr) == "workspace_not_found" {
			return backend.NewCloseAlreadyGone()
		}
		return backend.NewCloseFailed(a.classifyRunError(err, res))
	}
	return backend.NewCloseClosed()
}

// Probe answers whether the referenced workspace still exists, under exactly the
// re-resolution, incarnation, and ownership rules Close uses. It runs no
// mutating command.
func (a *Adapter) Probe(ctx context.Context, ref backend.Ref) backend.ProbeResult {
	_, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewProbeIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewProbeUnreadable(cause)
	case locateAbsent:
		return backend.NewProbeGone()
	case locateFound:
		return backend.NewProbePresent()
	case locateInvalid:
	}
	// Unreadable, never Present: an unknown state answering "the workspace is
	// there" is the fail-OPEN direction, and Conclusive() would let a caller act
	// on it. Construction guard, like Close's default; the enum is the tested
	// part.
	return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureInternal,
		errors.New("the workspace lookup produced no usable state")))
}

// locateState is the shared outcome of "find this reference's workspace".
type locateState uint8

const (
	// locateInvalid is the ZERO value and deliberately not a real outcome.
	// Without it locateFound would be zero, so a zero-valued state would select
	// `case locateFound:` and fall through to the close — on the wrong side of
	// the guard written to catch it — and would make Probe answer Present,
	// which is fail-open.
	locateInvalid locateState = iota
	locateFound
	locateAbsent
	locateMismatch
	locateUnreadable
)

// locate is the single lookup Close and Probe share, so the two cannot drift.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (string, locateState, backend.StartCause) {
	if ref.Kind() != backend.KindHerdr {
		return "", locateUnreadable, backend.NewStartCause(backend.FailureInternal,
			backend.ErrRefKindMismatch)
	}
	// The selection CHAIN must match, not merely the resolved name. A reference
	// taken against an explicitly named session must not be answered by an
	// adapter that resolved the default one.
	if ref.Source() != a.source {
		return "", locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the reference names a different server selection"))
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		return "", locateUnreadable, backend.NewStartCause(backend.FailureInternal, err)
	}
	want := identity.Workspace()

	server, cause := a.readiness(ctx)
	if cause != nil {
		// Absence is concluded from the failure CLASS and the stat together.
		// Either alone is a different claim: an incompatible or unauthenticated
		// server says nothing about whether the workspace exists, and a socket
		// missing at the moment we look is a race rather than a verdict.
		if cause.Class() == backend.FailureUnavailable && a.serverGone(server) {
			return "", locateAbsent, backend.StartCause{}
		}
		return "", locateUnreadable, *cause
	}

	// Prove the incarnation BEFORE reading the listing into a verdict. A
	// restarted herdr answers a listing perfectly well; what it cannot do is
	// hold the workspace this reference names, and an id it does not recognise
	// would otherwise read as an ordinary absence.
	if !ref.Server().Matches(server.incarnation) {
		return "", locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr server restarted since this reference was taken"))
	}

	rows, scause := a.snapshot(ctx, exec.KindHerdrProbe)
	if scause != nil {
		// Neither a truncated listing nor an unreadable one can prove absence,
		// and reporting gone would discharge an obligation still outstanding.
		return "", locateUnreadable, *scause
	}
	row, ok := rows[want]
	if !ok {
		// Absent by id on the incarnation the reference was bound to. herdr ids
		// are server-assigned and not reused within a session, so a workspace
		// missing from a complete listing is gone rather than renamed.
		return "", locateAbsent, backend.StartCause{}
	}
	// Ownership, and it is the check the Closer contract states as a MUST.
	//
	// A herdr workspace id is server-assigned and carries nothing of ours, so
	// Ref.Validate cannot bind it to the tag the way it can for a tmux session
	// name. A create response naming the wrong object — a raced reply, a stale
	// id after a restart — would otherwise yield a fully valid Ref pointing at
	// somebody else's workspace, and Close would destroy it and report success.
	// The cmux adapter shipped without this and it was the review's Critical.
	if row.marker != ref.OwnershipName() {
		return "", locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the workspace at this identifier does not carry our ownership marker"))
	}
	return row.id, locateFound, backend.StartCause{}
}
