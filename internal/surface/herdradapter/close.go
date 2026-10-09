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
	workspace, server, state, cause := a.locate(ctx, ref)
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
		// Two ways a failed close is still a satisfied rollback: herdr says the
		// workspace is gone, or the endpoint itself has vanished — a herdr that
		// quit mid-rollback. The second half needs the socket locate resolved,
		// which is why it travels back. cmux has both; this had only the first,
		// so a server quitting mid-rollback yielded CloseFailed for a workspace
		// that could not possibly still exist.
		if errorCode(res.Stderr) == "workspace_not_found" || a.serverGone(server) {
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
	return a.probeIn(ctx, ref, liveView{a})
}

// Prober returns a Probe that reads herdr's readiness and workspace listing
// at most once, on its first call, and judges every reference against that
// one read under the same rules Probe uses. `surface prune` asks about many
// ledger rows at once; one listing per prune keeps it from making two herdr
// calls per row. The answers are as of that one read, so it is for a single
// pass, not for a long-lived caller.
func (a *Adapter) Prober() func(context.Context, backend.Ref) backend.ProbeResult {
	view := &onceView{a: a}
	return func(ctx context.Context, ref backend.Ref) backend.ProbeResult {
		return a.probeIn(ctx, ref, view)
	}
}

func (a *Adapter) probeIn(ctx context.Context, ref backend.Ref, view workspaceView) backend.ProbeResult {
	_, _, state, cause := a.locateIn(ctx, ref, view)
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

// mayHold reports whether a reference taken through source could name this
// adapter's session. It compares the SESSION each side resolved, not the way it
// was found.
//
// The default chain and a session named "default" select one server: the
// drain pins HERDR_SESSION=default, so its references say named-session, and a
// plain CLI resolving the default chain must still read and close them
// (forgectl#1187). Comparing the labels for equality refused that as an
// identity mismatch.
//
//   - A default-session reference was taken on the session named "default",
//     so it is refused by an adapter pinned to any other name.
//   - A named-session reference does not record its name, so the label cannot
//     refuse it. The incarnation check in locate does: the ServerID digests
//     the socket path the roster maps the session to, so a reference from
//     another session never matches this one's server.
func (a *Adapter) mayHold(source backend.ServerSource) bool {
	switch source {
	case backend.HerdrDefaultSessionServer():
		return a.session == defaultSession
	case backend.HerdrNamedSessionServer():
		return true
	default:
		return false
	}
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

// workspaceView is where locate reads the server's readiness and the
// workspace listing: fresh on every call (liveView), or once for a pass
// (onceView, Prober).
type workspaceView interface {
	readiness(ctx context.Context) (serverInfo, *backend.StartCause)
	snapshot(ctx context.Context) (map[string]workspaceRow, *backend.StartCause)
}

// liveView reads herdr on every call.
type liveView struct{ a *Adapter }

func (v liveView) readiness(ctx context.Context) (serverInfo, *backend.StartCause) {
	return v.a.readiness(ctx)
}

func (v liveView) snapshot(ctx context.Context) (map[string]workspaceRow, *backend.StartCause) {
	return v.a.snapshot(ctx, exec.KindHerdrProbe)
}

// onceView reads each of readiness and the listing at most once and answers
// every later call with that result, failures included.
type onceView struct {
	a           *Adapter
	ready       bool
	server      serverInfo
	serverCause *backend.StartCause
	listed      bool
	rows        map[string]workspaceRow
	rowsCause   *backend.StartCause
}

func (v *onceView) readiness(ctx context.Context) (serverInfo, *backend.StartCause) {
	if !v.ready {
		v.server, v.serverCause = v.a.readiness(ctx)
		v.ready = true
	}
	return v.server, v.serverCause
}

func (v *onceView) snapshot(ctx context.Context) (map[string]workspaceRow, *backend.StartCause) {
	if !v.listed {
		v.rows, v.rowsCause = v.a.snapshot(ctx, exec.KindHerdrProbe)
		v.listed = true
	}
	return v.rows, v.rowsCause
}

// locate is the single lookup Close and Probe share, so the two cannot drift.
// The serverInfo comes back so Close can reuse it: a close that FAILS while the
// endpoint has vanished is a satisfied rollback, and answering that needs the
// socket locate already resolved. cmux's adapter does the same with a socket it
// knows at construction; herdr's comes from the roster, so it has to travel.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (string, serverInfo, locateState, backend.StartCause) {
	return a.locateIn(ctx, ref, liveView{a})
}

// locateIn is locate reading the server and listing through view.
func (a *Adapter) locateIn(ctx context.Context, ref backend.Ref, view workspaceView) (string, serverInfo, locateState, backend.StartCause) {
	if ref.Kind() != backend.KindHerdr {
		return "", serverInfo{}, locateUnreadable, backend.NewStartCause(backend.FailureInternal,
			backend.ErrRefKindMismatch)
	}
	// The reference must name a session this adapter's pin could be. See
	// mayHold: the label is compared for what it proves about the session, and
	// the incarnation check below decides the server.
	if !a.mayHold(ref.Source()) {
		return "", serverInfo{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the reference was taken in the default session, and this adapter is pinned to another"))
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		return "", serverInfo{}, locateUnreadable, backend.NewStartCause(backend.FailureInternal, err)
	}
	want := identity.Workspace()

	server, cause := view.readiness(ctx)
	if cause != nil {
		// Absence is concluded from the failure CLASS and the stat together.
		// Either alone is a different claim: an incompatible or unauthenticated
		// server says nothing about whether the workspace exists, and a socket
		// missing at the moment we look is a race rather than a verdict.
		if cause.Class() == backend.FailureUnavailable && a.serverGone(server) {
			return "", server, locateAbsent, backend.StartCause{}
		}
		return "", server, locateUnreadable, *cause
	}

	// Prove the incarnation BEFORE reading the listing into a verdict. A
	// restarted herdr answers a listing perfectly well; what it cannot do is
	// hold the workspace this reference names, and an id it does not recognise
	// would otherwise read as an ordinary absence.
	if !ref.Server().Matches(server.incarnation) {
		return "", server, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr server is not the one this reference was taken on: it restarted, or the reference is from another session"))
	}

	rows, scause := view.snapshot(ctx)
	if scause != nil {
		// Neither a truncated listing nor an unreadable one can prove absence,
		// and reporting gone would discharge an obligation still outstanding.
		return "", server, locateUnreadable, *scause
	}
	row, ok := rows[want]
	if !ok {
		// Absent by id on the incarnation the reference was bound to. herdr ids
		// are server-assigned and not reused within a session, so a workspace
		// missing from a complete listing is gone rather than renamed.
		return "", server, locateAbsent, backend.StartCause{}
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
		return "", server, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the workspace at this identifier does not carry our ownership marker"))
	}
	return row.id, server, locateFound, backend.StartCause{}
}
