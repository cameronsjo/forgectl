package herdradapter

import (
	"context"
	"errors"
	"os"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Close removes exactly the workspace a reference names, after proving the
// session is still the incarnation the reference was bound to AND that the
// workspace is one we created.
//
// herdr closes by workspace id alone: `workspace close` takes no label and
// no precondition. And herdr does reuse ids across a restart. Its id counter
// is per process and starts again at 1; a session restore keeps the saved
// ids and moves the counter past them, so only a workspace that was not
// restored leaves its id free to name a new one (forgectl#1045). So the
// incarnation is proved again on both sides of the close: immediately before
// it, from a fresh stat of the socket, so a restart after the lookup refuses
// instead of closing whatever now holds the id; and after it, so a restart
// during the close is not reported as a clean close. What remains is the time
// one herdr command takes to start, which nothing on this side can remove.
func (a *Adapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	workspace, server, state, cause := a.locateIn(ctx, ref, liveView{a}, a.sessionBound)
	switch state {
	case locateMismatch:
		return backend.NewCloseIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewCloseUnreadable(cause)
	case locateAbsent:
		return backend.NewCloseAlreadyGone()
	case locateFound, locateRestored:
		// Fall through. The switch is written so ONLY these states reach the
		// close — see the default below. A restored workspace is ours and is
		// closed like any other.
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

	switch a.turnover(server) {
	case turnoverGone:
		// The endpoint vanished after the lookup. Not concluded gone: absence
		// needs a failed herdr call AND the stat (see locateIn), and here
		// herdr was never asked. The obligation is kept, nothing is closed,
		// and the next close settles it.
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureUnavailable,
			errors.New("the herdr socket vanished between the lookup and the close; nothing was closed")))
	case turnoverChanged:
		return backend.NewCloseIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr server restarted between the lookup and the close; nothing was closed")))
	case turnoverNone:
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
	if a.turnover(server) == turnoverChanged {
		// herdr answered, but the server behind the socket changed while it
		// did, so which server took the close, and so which workspace it
		// closed, is not known. Not reported closed: the caller keeps its
		// obligation, and the next lookup settles it on the new server.
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr server restarted while the close ran; which workspace it reached is not known")))
	}
	return backend.NewCloseClosed()
}

// turnoverState is what a fresh stat of the socket says about the server
// locate proved.
type turnoverState uint8

const (
	turnoverNone turnoverState = iota
	turnoverChanged
	turnoverGone
)

// turnover stats the socket again and compares its fingerprint with the
// incarnation locate proved. No herdr command runs, so it is cheap enough to
// bracket a close. A stat that fails for any reason but absence, or a socket
// that no longer fingerprints, counts as changed: it cannot prove the server
// is the one locate saw.
func (a *Adapter) turnover(server serverInfo) turnoverState {
	if server.socket == "" {
		return turnoverChanged
	}
	info, err := a.lstat(server.socket)
	if errors.Is(err, os.ErrNotExist) {
		return turnoverGone
	}
	if err != nil {
		return turnoverChanged
	}
	now, err := a.fingerprint(server.socket, info, server.version)
	if err != nil || !server.incarnation.Matches(now) {
		return turnoverChanged
	}
	return turnoverNone
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
	_, _, state, cause := a.locateIn(ctx, ref, view, a.sessionBound)
	switch state {
	case locateMismatch:
		return backend.NewProbeIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewProbeUnreadable(cause)
	case locateAbsent:
		return backend.NewProbeGone()
	case locateFound:
		return backend.NewProbePresent()
	case locateRestored:
		return backend.NewProbePresentRestored()
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
	// locateRestored is locateFound for a workspace a restarted server holds
	// under the reference's id and marker (locateAfterTurnover): ours, but
	// the worker that ran in it died with the old server.
	locateRestored
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
//
// locate is the strict form, for the pane readers: a changed incarnation is
// always a mismatch.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (string, serverInfo, locateState, backend.StartCause) {
	return a.locateIn(ctx, ref, liveView{a}, false)
}

// locateIn is locate reading the server and listing through view. With
// acrossRestart, a changed incarnation is settled by locateAfterTurnover
// instead of refused; only a ledger-bound adapter's Close and Probe pass it.
func (a *Adapter) locateIn(ctx context.Context, ref backend.Ref, view workspaceView, acrossRestart bool) (string, serverInfo, locateState, backend.StartCause) {
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
		if acrossRestart {
			return a.locateAfterTurnover(ctx, ref, want, server, view)
		}
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

// locateAfterTurnover answers locate for a reference whose server incarnation
// changed, when the caller vouches that the reference was taken in this
// session (ForSessionLedger) and the reference records the same socket path
// the session resolves to now. The server behind that path turned over: it
// restarted, and its panes, the worker's harness among them, went with the
// old one (forgectl#1077). A change in the socket's change time alone, with
// no restart, lands here too.
//
// What can remain is the workspace OBJECT: herdr's session restore brings a
// workspace back under its saved id and label, with fresh shells. So the
// answer comes from the ownership marker, which carries a random tag and
// cannot be on anybody else's workspace, read from a complete listing:
//
//   - no workspace carries the marker: gone. The new server does not hold it.
//   - exactly one does, at the reference's id: locateRestored. It is ours,
//     and Close closes it.
//   - anything else (the marker at another id, or on two): a mismatch,
//     because neither absence nor this id is proved.
//
// The listing cannot catch a restore half done: herdr binds its API socket
// before it restores the saved session, but answers a listing only from its
// event loop, which starts after the restore (herdr's
// src/server/headless/bootstrap.rs).
//
// One gap is shared with serverGone: a server still running after its socket
// path was taken over by another still holds the workspace, and nothing on
// this side can see it.
func (a *Adapter) locateAfterTurnover(ctx context.Context, ref backend.Ref, want string, server serverInfo, view workspaceView) (string, serverInfo, locateState, backend.StartCause) {
	// Same session NAME is not same server. herdr derives the socket from its
	// config directory ($XDG_CONFIG_HOME, and a debug build's own app name),
	// so a reference taken from another environment can reach a different
	// server that is still running and still holds the worker. Only a
	// turnover at the SAME socket path is a restart; a reference that does
	// not record its path keeps the plain mismatch.
	if taken, ok := ref.Endpoint(); !ok || !taken.Matches(backend.EndpointOf(server.socket)) {
		return "", server, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the herdr server is not the one this reference was taken on, and it is not a restart at the same socket"))
	}
	rows, scause := view.snapshot(ctx)
	if scause != nil {
		return "", server, locateUnreadable, *scause
	}
	marker := ref.OwnershipName()
	var held []string
	for _, row := range rows {
		if row.marker == marker {
			held = append(held, row.id)
		}
	}
	switch {
	case len(held) == 0:
		return "", server, locateAbsent, backend.StartCause{}
	case len(held) == 1 && held[0] == want:
		return want, server, locateRestored, backend.StartCause{}
	}
	return "", server, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
		errors.New("the herdr server restarted, and forgectl's marker is on a workspace other than the one the reference names"))
}
