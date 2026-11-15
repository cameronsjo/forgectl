// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// listState is the outcome of one listing.
type listState uint8

const (
	listUnreadable listState = iota
	listFound
	listSessionGone
)

// listBlocks runs `rex block ls --session S --json`. Rex's own not-found for
// exactly this session is listSessionGone; any other failure, or a reply the
// parser cannot read, is unreadable.
func (a *Adapter) listBlocks(ctx context.Context, kind exec.CommandKind, sessionID string) ([]block, listState, *backend.StartCause) {
	res, err := a.run.RunSensitive(ctx, a.command(kind,
		exec.MustFixed("block"),
		exec.MustFixed("ls"),
		exec.MustFixed("--session"),
		exec.Opaque(sessionID),
		exec.MustFixed("--json"),
	))
	if err != nil {
		if notFound(res, targetSession, sessionID) {
			return nil, listSessionGone, nil
		}
		c := classifyRunError(err)
		return nil, listUnreadable, &c
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		c := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("rex block ls output was truncated"))
		return nil, listUnreadable, &c
	}
	blocks, perr := parseBlocks(raw)
	if perr != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, perr)
		return nil, listUnreadable, &c
	}
	return blocks, listFound, nil
}

// listSessions runs `rex ls --json`, and reports false for any failure or
// unreadable reply.
func (a *Adapter) listSessions(ctx context.Context, kind exec.CommandKind) ([]session, bool) {
	res, err := a.run.RunSensitive(ctx, a.command(kind, exec.MustFixed("ls"), exec.MustFixed("--json")))
	if err != nil {
		return nil, false
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return nil, false
	}
	sessions, perr := parseSessions(raw)
	if perr != nil {
		return nil, false
	}
	return sessions, true
}

// locateState is what locate established about a reference.
type locateState uint8

const (
	locateInvalid locateState = iota
	locateFound
	locateAbsent
	locateMismatch
	locateUnreadable
)

// located is the block a reference names, once locate has proven it.
type located struct {
	id backend.RexIdentity
}

// locate finds the block a reference names and proves it is forgectl's.
//
// Three checks, in order, and all three must hold before anything acts:
//
//  1. The reference's source is the one this adapter selects, and the server
//     `rex whoami` reports (API revision 14, local, this instance_id and
//     host_id; the tailnet refusal applies to Start only) fingerprints to the reference's server. A restarted server, or
//     another one, is a mismatch.
//  2. `rex block ls` in the reference's session lists its full block id. Rex
//     reporting that exact session, or that block, not found on a matching
//     server is proof the block is gone; a session that lists without it is
//     too.
//  3. The listed block carries forgectl.surface=<the reference's ownership
//     name> as its label, and the creation time its id embeds is the
//     reference's.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (located, locateState, backend.StartCause) {
	id, err := ref.RexIdentity()
	if err != nil {
		return located{}, locateUnreadable, backend.NewStartCause(backend.FailureInternal, err)
	}
	if ref.Source() != a.source {
		return located{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the reference names a different rex server selection"))
	}
	srv, cause := a.readiness(ctx, false)
	if cause != nil {
		return located{}, locateUnreadable, *cause
	}
	if !ref.Server().Matches(srv) {
		return located{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the rex server is not the one this reference was taken on"))
	}
	blocks, state, bcause := a.listBlocks(ctx, exec.KindRexProbe, id.Session())
	switch state {
	case listSessionGone:
		return located{}, locateAbsent, backend.StartCause{}
	case listUnreadable:
		c := backend.NewStartCause(backend.FailureUnavailable, errors.New("rex block ls failed"))
		if bcause != nil {
			c = *bcause
		}
		return located{}, locateUnreadable, c
	case listFound:
	}
	for _, b := range blocks {
		if b.ID != id.Block() {
			continue
		}
		created, ok := backend.RexBlockCreatedMS(b.ID)
		if !ok || created != id.CreatedMS() || b.Label != ownerLabel(ref.Tag()) {
			return located{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
				errors.New("the rex block with this id does not carry forgectl's owner label for this reference"))
		}
		return located{id: id}, locateFound, backend.StartCause{}
	}
	return located{}, locateAbsent, backend.StartCause{}
}

// Close ends the worker's block and, when it was the last block of the
// session Start created, the session. A repeat finds the block gone and
// reports so, so Close is safe to retry.
func (a *Adapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	loc, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewCloseIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewCloseUnreadable(cause)
	case locateAbsent:
		a.retireSession(ctx, ref)
		return backend.NewCloseAlreadyGone()
	case locateFound:
	case locateInvalid:
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureInternal,
			errors.New("the block lookup produced no usable state")))
	}
	// debt: locate checks identity in one rex call and block close acts on
	// the id in the next; a full block id is never reused, so the window is
	// a relabel in between, not another block. Rex 0.1.0-preview.19 has no
	// expected-label guard on close. Close the window when Rex adds one.
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindRexCleanup,
		exec.MustFixed("block"),
		exec.MustFixed("close"),
		exec.MustFixed("--session"),
		exec.Opaque(loc.id.Session()),
		exec.Opaque(loc.id.Block()),
	))
	if err != nil {
		if notFound(res, targetBlock, loc.id.Block()) || notFound(res, targetSession, loc.id.Session()) {
			a.retireSession(ctx, ref)
			return backend.NewCloseAlreadyGone()
		}
		return backend.NewCloseFailed(classifyRunError(err))
	}
	// Rex confirmed the close; read it back, so a block Rex still lists is not
	// reported closed.
	blocks, lstate, _ := a.listBlocks(ctx, exec.KindRexProbe, loc.id.Session())
	if lstate == listFound {
		for _, b := range blocks {
			if b.ID == loc.id.Block() {
				return backend.NewCloseFailed(backend.NewStartCause(backend.FailureTargetBusy,
					errors.New("rex still lists the worker's block after closing it; run close again")))
			}
		}
	}
	a.retireSession(ctx, ref)
	return backend.NewCloseClosed()
}

// retireSession kills the reference's session when it is provably the one
// Start created and nothing else lives in it: `rex ls` lists it under the
// ownership name, and `rex block ls` lists no block in it. Anything else
// leaves it alone: a session the adapter did not create is never killed, and
// one a person has added a block to keeps that block. It is best effort; the
// block, which is what a reference names, is already settled when it runs.
func (a *Adapter) retireSession(ctx context.Context, ref backend.Ref) {
	id, err := ref.RexIdentity()
	if err != nil {
		return
	}
	sessions, ok := a.listSessions(ctx, exec.KindRexProbe)
	if !ok {
		return
	}
	ours := false
	for _, s := range sessions {
		if s.ID == id.Session() && s.Label == a.sessionLabel(ref.Tag()) {
			ours = true
		}
	}
	if !ours {
		return
	}
	blocks, state, _ := a.listBlocks(ctx, exec.KindRexProbe, id.Session())
	if state != listFound || len(blocks) != 0 {
		return
	}
	// debt: a block added between the listing above and this kill would die
	// with the session; rex kill has no expected-empty guard. The session is
	// labeled with forgectl's ownership name, so it is not one a person opens
	// work in by accident.
	_, _ = a.run.RunSensitive(ctx, a.command(exec.KindRexCleanup,
		exec.MustFixed("kill"),
		exec.Opaque(id.Session()),
	))
}

// Probe reports whether the worker still runs. A finished block reports gone
// with its exit code (backend.ProbeResult.Finished): its program has exited,
// and close only removes the block.
func (a *Adapter) Probe(ctx context.Context, ref backend.Ref) backend.ProbeResult {
	loc, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewProbeIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewProbeUnreadable(cause)
	case locateAbsent:
		return backend.NewProbeGone()
	case locateFound:
		return a.probeProcess(ctx, loc.id)
	case locateInvalid:
	}
	return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureInternal,
		errors.New("the block lookup produced no usable state")))
}

// probeProcess reads the located block's program state with `rex block call
// process`.
func (a *Adapter) probeProcess(ctx context.Context, id backend.RexIdentity) backend.ProbeResult {
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindRexProbe,
		exec.MustFixed("block"),
		exec.MustFixed("call"),
		exec.MustFixed("--session"),
		exec.Opaque(id.Session()),
		exec.MustFixed("--block"),
		exec.Opaque(id.Block()),
		exec.MustFixed("process"),
	))
	if err != nil {
		if notFound(res, targetBlock, id.Block()) || notFound(res, targetSession, id.Session()) {
			return backend.NewProbeGone()
		}
		return backend.NewProbeUnreadable(classifyRunError(err))
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("rex block call process output was truncated")))
	}
	p, perr := parseProcess(raw)
	if perr != nil {
		return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureMalformedResponse, perr))
	}
	if p.Running {
		return backend.NewProbePresent()
	}
	return backend.NewProbeFinished(p.ExitCode)
}
