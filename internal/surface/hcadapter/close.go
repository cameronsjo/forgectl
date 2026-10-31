package hcadapter

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// locateState is what locate established about a reference.
type locateState uint8

const (
	locateInvalid locateState = iota
	locateFound
	locateAbsent
	locateMismatch
	locateUnreadable
)

// locate finds the terminal a reference names and proves it is forgectl's.
//
// Three checks, in order, and all three must hold before anything acts:
//
//  1. The reference's source is the one this process selects, and the core's
//     id fingerprints to the reference's server. A reference taken under
//     another HC_SESSION, or before the core's state was wiped, is a
//     mismatch.
//  2. `hc get <address>` finds the terminal. not_found on a matching core is
//     proof it is gone.
//  3. The terminal carries the reference's ownership marker in its
//     forgectl.surface label and was created at the reference's time. An
//     address hc handed out again after `hc rm` fails here.
func (a *Adapter) locate(ctx context.Context, ref backend.Ref) (terminal, locateState, backend.StartCause) {
	id, err := ref.HcIdentity()
	if err != nil {
		return terminal{}, locateUnreadable, backend.NewStartCause(backend.FailureInternal, err)
	}
	if ref.Source() != a.source {
		return terminal{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the reference names a different hc session selection"))
	}
	server, cause := a.readiness(ctx)
	if cause != nil {
		return terminal{}, locateUnreadable, *cause
	}
	if !ref.Server().Matches(server) {
		return terminal{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the hc core is not the one this reference was taken on"))
	}
	t, state, gcause := a.get(ctx, id.Terminal())
	switch state {
	case getAbsent:
		return terminal{}, locateAbsent, backend.StartCause{}
	case getUnreadable:
		c := backend.NewStartCause(backend.FailureUnavailable, errors.New("hc get failed"))
		if gcause != nil {
			c = *gcause
		}
		return terminal{}, locateUnreadable, c
	case getFound:
	}
	if t.Address != id.Terminal() || t.CreatedMS != id.CreatedMS() || t.Labels[ownerLabel] != ref.OwnershipName() {
		return terminal{}, locateMismatch, backend.NewStartCause(backend.FailureIdentityMismatch,
			errors.New("the terminal at this address is not the one forgectl started"))
	}
	return t, locateFound, backend.StartCause{}
}

// Close kills the worker's program, when it still runs, and removes its
// terminal. A retry after a failed rm finds a finished terminal and only
// removes it, so Close is safe to repeat.
func (a *Adapter) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	t, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewCloseIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewCloseUnreadable(cause)
	case locateAbsent:
		return backend.NewCloseAlreadyGone()
	case locateFound:
	case locateInvalid:
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureInternal,
			errors.New("the terminal lookup produced no usable state")))
	}
	if t.State == stateLive {
		res, err := a.run.RunSensitive(ctx, a.command(exec.KindHcCleanup,
			exec.MustFixed("kill"),
			exec.MustFixed("--json"),
			exec.EndOfOptions(),
			exec.Opaque(t.Address),
		))
		if err != nil {
			if errorKind(res.Stderr) == kindNotFound {
				return backend.NewCloseAlreadyGone()
			}
			return backend.NewCloseFailed(classifyRunError(err))
		}
	}
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindHcCleanup,
		exec.MustFixed("rm"),
		exec.MustFixed("--json"),
		exec.EndOfOptions(),
		exec.Opaque(t.Address),
	))
	if err != nil {
		switch errorKind(res.Stderr) {
		case kindNotFound:
			return backend.NewCloseClosed()
		case kindLive:
			// kill returned before the program finished exiting. A retry
			// finds it finished and only removes it.
			return backend.NewCloseFailed(backend.NewStartCause(backend.FailureTargetBusy,
				errors.New("the worker's program is still stopping; run close again")))
		}
		return backend.NewCloseFailed(classifyRunError(err))
	}
	return backend.NewCloseClosed()
}

// Probe reports whether the worker still runs. A finished terminal is gone:
// its program has exited, and close only removes the record.
func (a *Adapter) Probe(ctx context.Context, ref backend.Ref) backend.ProbeResult {
	t, state, cause := a.locate(ctx, ref)
	switch state {
	case locateMismatch:
		return backend.NewProbeIdentityMismatch(cause)
	case locateUnreadable:
		return backend.NewProbeUnreadable(cause)
	case locateAbsent:
		return backend.NewProbeGone()
	case locateFound:
		if t.State == stateFinished {
			return backend.NewProbeGone()
		}
		return backend.NewProbePresent()
	case locateInvalid:
	}
	return backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureInternal,
		errors.New("the terminal lookup produced no usable state")))
}
