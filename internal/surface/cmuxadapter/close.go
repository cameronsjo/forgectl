package cmuxadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Close removes exactly the workspace a reference names, after proving the
// server is still the incarnation the reference was bound to.
//
// The ordering is the guarantee. Re-resolve, re-fingerprint, compare, find the
// row by exact UUID, and only then close — by that same UUID, which is why the
// adapter insists on `--id-format uuids` everywhere. cmux's other handles are a
// positional index and a `workspace:N` ref, both of which name a DIFFERENT
// workspace once anything above them closes; a rollback holding one of those
// would destroy whatever slid into the slot.
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
		// Fall through to the close. The switch is written so that ONLY this
		// state reaches it — see the default below.
	default:
		// A state nobody has taught this switch about must not reach the close.
		//
		// Stated plainly because the distinction matters: this arm is a
		// CONSTRUCTION guard against a future locateState, not a tested control.
		// Every state locate can return is handled above, so nothing can drive
		// it and its mutation survives. What IS tested is the enum underneath
		// it — locateInvalid owning zero, so a zero-valued state reaches here
		// rather than `case locateFound:`. The tmux adapter had this exact bug:
		// with locateFound at zero, the guard added to keep an unknown state
		// away from the kill sat on the wrong side of the very value it named.
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureInternal,
			errors.New("the workspace lookup produced no usable state")))
	}

	// Re-validate at the point of USE. Labelled honestly, in the same terms as
	// the arm above: locate only ever returns an id that already satisfies this,
	// so it is defence in depth against a future locate that stops validating,
	// not a live gate. The difference from a purely inert check is that the
	// operand here is what reaches a command line — an empty or non-UUID value
	// arriving at `workspace close` is the shape that closes the wrong object.
	if _, err := backend.NewCMuxIdentity(workspace); err != nil {
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureMalformedResponse, err))
	}

	res, err := a.run.RunSensitive(ctx, a.command(exec.KindCmuxCleanup,
		exec.MustFixed("workspace"),
		exec.MustFixed("close"),
		exec.Opaque(workspace),
	))
	if err != nil {
		// A close that raced someone else's is a satisfied rollback, not a
		// failure: the obligation was that the object be gone, and it is.
		if a.serverGone() || workspaceNotFound(res.Stderr) {
			return backend.NewCloseAlreadyGone()
		}
		return backend.NewCloseFailed(a.classifyRunError(err, res.Stderr))
	}
	return backend.NewCloseClosed()
}

// Probe answers whether the referenced workspace still exists, under exactly the
// re-resolution and ownership rules Close uses. It runs no mutating command.
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
	// Reached only by locateInvalid or a state added without teaching this
	// switch. Unreadable, never Present: an unknown state answering "the
	// workspace is there" is the fail-OPEN direction, and Conclusive() would let
	// a caller act on it.
	//
	// Like Close's default arm, this is a CONSTRUCTION guard and not a tested
	// control — locate cannot return locateInvalid, so its mutation survives.
	// The tested part is the enum itself.
	return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureInternal,
		errors.New("the workspace lookup produced no usable state")))
}

// locateState is the shared outcome of "find this reference's workspace".
type locateState uint8

const (
	// locateInvalid is the ZERO value, and it is deliberately not a real
	// outcome. Without it locateFound would be zero, so a zero-valued
	// locateState — the exact "state nobody taught the switch about" that
	// Close's default arm exists to catch — would select `case locateFound:` and
	// fall through to the close, on the wrong side of its own guard. It would
	// also make Probe answer ProbePresent, which is the fail-OPEN direction.
	locateInvalid locateState = iota
	locateFound
	locateAbsent
	locateMismatch
	locateUnreadable
)

// locate is the single lookup Close and Probe share.
//
// Sharing it is not just deduplication: the contract requires a probe and a
// close to apply the SAME re-resolution, incarnation, and ownership rules, and
// two implementations of that would be two chances to let them drift — with the
// drift showing up as a probe that says present and a close that refuses, or
// worse, the reverse.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (string, locateState, backend.StartCause) {
	if ref.Kind() != backend.KindCmux {
		return "", locateUnreadable, backend.NewStartCause(backend.FailureInternal,
			backend.ErrRefKindMismatch)
	}
	// The selection CHAIN must match, not merely the resolved path. A reference
	// taken against an explicitly pinned CMUX_SOCKET_PATH must not be answered
	// by an adapter that resolved the default endpoint, even on a machine where
	// the two paths happen to coincide today.
	if ref.Source() != a.source {
		return "", locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the reference names a different server selection"))
	}
	identity, err := ref.CMuxIdentity()
	if err != nil {
		return "", locateUnreadable, backend.NewStartCause(backend.FailureInternal, err)
	}
	want := identity.Workspace()

	server, cause := a.readiness(ctx)
	if cause != nil {
		// An endpoint that is definitively absent means the workspace is
		// certainly gone. The incarnation question is moot: there is no
		// incarnation. Anything else — unreadable, not ours, incompatible,
		// unauthenticated — proves nothing about the workspace and must not
		// discharge a rollback obligation.
		if a.serverGone() {
			return "", locateAbsent, backend.StartCause{}
		}
		return "", locateUnreadable, *cause
	}

	// Prove the incarnation BEFORE reading the listing into a verdict. A
	// restarted cmux answers a listing perfectly well; what it cannot do is hold
	// the workspace this reference names, and a UUID it does not recognise would
	// otherwise read as an ordinary absence.
	if !ref.Server().Matches(server.incarnation) {
		return "", locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the cmux server restarted since this reference was taken"))
	}

	rows, scause := a.snapshot(ctx, exec.KindCmuxProbe)
	if scause != nil {
		// Neither a truncated listing nor an unreadable one can prove absence,
		// and reporting gone here would discharge a rollback obligation that is
		// still outstanding.
		return "", locateUnreadable, *scause
	}
	if row, ok := rows[foldID(want)]; ok {
		// The listing's spelling wins, not the reference's: the operand goes
		// back to cmux, so it should be the bytes cmux most recently used for
		// this object.
		return row.id, locateFound, backend.StartCause{}
	}
	// Absent by UUID, on an incarnation the reference was bound to. That is a
	// real absence: cmux UUIDs are not reused, so a workspace missing from a
	// complete listing of the same server is gone rather than renamed.
	return "", locateAbsent, backend.StartCause{}
}

// workspaceNotFound matches cmux's exact refusal for a workspace that has since
// disappeared. Measured on 0.64.22: "Error: not_found: Workspace not found",
// exit 1.
//
// Unlike absence of the SERVER — which is settled by a stat, because a
// diagnostic string is a poor basis for a claim that discharges a rollback —
// this one has no filesystem counterpart to ask instead. It is matched on the
// stable machine-readable half (`not_found:`) rather than the English prose
// after it, so a reworded message does not silently turn a satisfied rollback
// into a failed one.
func workspaceNotFound(out exec.BoundedOutput) bool {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return false
	}
	return strings.Contains(string(raw), "not_found:")
}
