package hcadapter

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Start creates the worker's terminal and returns what is known about whether
// hc changed state.
//
// The bootstrap is the one operand after `--`; hc runs a single string under
// `sh -c` (measured on 0.1.15). `--outlive-parent` keeps the worker running
// when the launcher's own terminal ends. The create reply has no creation
// time, so a follow-up `hc get` reads it, and checks the label at the same
// time.
func (a *Adapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	if err := spec.Validate(); err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	server, cause := a.readiness(ctx)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}
	owner := spec.OwnershipName()

	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHcCreate,
		exec.MustFixed("run"),
		exec.MustFixed("--bg"),
		exec.MustFixed("--json"),
		// A worker outlives the terminal that launched it (a coordinator's
		// own session ending must not kill its workers), and hc types no
		// notes about it into that terminal: the coordinator follows the
		// worker through forgectl, not through hc's notes.
		exec.MustFixed("--outlive-parent"),
		exec.MustFixed("--no-notify"),
		exec.MustFixed("--name"),
		spec.Name(),
		exec.MustFixed("--cwd"),
		spec.CWD(),
		exec.MustFixed("--label"),
		exec.Opaque(ownerLabel+"="+owner),
		exec.EndOfOptions(),
		spec.Bootstrap().SensitiveArg(),
	))
	if runErr != nil {
		// Only hc's own refusals before it spawned anything are definitive.
		// spawn_failed was measured to leave no terminal behind (a missing
		// cwd); any other failure may have created one.
		switch errorKind(res.Stderr) {
		case kindNameTaken:
			return backend.NewNotMutated(backend.NewStartCause(backend.FailureNameCollision, runErr))
		case kindInvalidParams:
			return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, runErr))
		case kindSpawnFailed:
			return backend.NewNotMutated(backend.NewStartCause(backend.FailureUnavailable, runErr))
		}
		return a.reconcile(ctx, spec.Tag(), server, classifyRunError(runErr))
	}

	var created struct {
		Address string `json:"address"`
	}
	if err := decodeStdout(res.Stdout, &created); err != nil || created.Address == "" {
		return a.reconcile(ctx, spec.Tag(), server, backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("hc run reported success without a readable address")))
	}
	t, state, cause := a.get(ctx, created.Address)
	if state != getFound || t.Labels[ownerLabel] != owner || t.Address != created.Address {
		if cause == nil {
			c := backend.NewStartCause(backend.FailureMalformedResponse,
				errors.New("the terminal hc reported does not carry this launch's ownership label"))
			cause = &c
		}
		return a.reconcile(ctx, spec.Tag(), server, *cause)
	}
	ref, err := a.reference(t, spec.Tag(), server)
	if err != nil {
		return a.reconcile(ctx, spec.Tag(), server, backend.NewStartCause(backend.FailureMalformedResponse, err))
	}
	return backend.NewRefKnown(ref)
}

// reconcile runs one labeled listing to settle whether an ambiguous create
// landed. One terminal carrying the label is RefKnown-with-cause, so the
// service closes it; none is NotMutated; anything else is OutcomeUnknown.
func (a *Adapter) reconcile(ctx context.Context, tag backend.RecoveryTag, server backend.ServerID, cause backend.StartCause) backend.StartResult {
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindHcReconcile,
		exec.MustFixed("ls"),
		exec.MustFixed("--all"),
		exec.MustFixed("--json"),
		exec.MustFixed("--label"),
		exec.Opaque(ownerLabel+"="+tag.OwnershipName()),
	))
	if err != nil {
		return backend.NewOutcomeUnknown(tag, cause)
	}
	var listing struct {
		Terminals []struct {
			Address string `json:"address"`
		} `json:"terminals"`
	}
	if derr := decodeStdout(res.Stdout, &listing); derr != nil || listing.Terminals == nil {
		return backend.NewOutcomeUnknown(tag, cause)
	}
	switch len(listing.Terminals) {
	case 0:
		return backend.NewNotMutated(cause)
	case 1:
		t, state, _ := a.get(ctx, listing.Terminals[0].Address)
		if state != getFound || t.Labels[ownerLabel] != tag.OwnershipName() {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		ref, rerr := a.reference(t, tag, server)
		if rerr != nil {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		return backend.NewRefKnownWithCause(ref, cause)
	}
	return backend.NewOutcomeUnknown(tag, cause)
}

// readiness runs `hc server status` and fingerprints the core it reports.
// The status call starts a core when none runs, which `hc run` would do
// anyway; forgectl never stops or restarts one.
func (a *Adapter) readiness(ctx context.Context) (backend.ServerID, *backend.StartCause) {
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindHcReadiness,
		exec.MustFixed("server"),
		exec.MustFixed("status"),
		exec.MustFixed("--json"),
	))
	if err != nil {
		c := classifyRunError(err)
		return backend.ServerID{}, &c
	}
	var st struct {
		Core struct {
			ID string `json:"id"`
		} `json:"core"`
	}
	if derr := decodeStdout(res.Stdout, &st); derr != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, derr)
		return backend.ServerID{}, &c
	}
	if st.Core.ID == "" {
		c := backend.NewStartCause(backend.FailureIncompatible, errors.New("hc server status reports no core id"))
		return backend.ServerID{}, &c
	}
	server, ferr := backend.FingerprintHcCore(a.session, st.Core.ID)
	if ferr != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, ferr)
		return backend.ServerID{}, &c
	}
	return server, nil
}

// reference binds a terminal read back from hc to this core.
func (a *Adapter) reference(t terminal, tag backend.RecoveryTag, server backend.ServerID) (backend.Ref, error) {
	id, err := backend.NewHcIdentity(t.Address, t.CreatedMS)
	if err != nil {
		return backend.Ref{}, err
	}
	return backend.NewHcRef(a.source, server, tag, id)
}

// getState is the outcome of one `hc get`.
type getState uint8

const (
	getUnreadable getState = iota
	getFound
	getAbsent
)

// get reads one terminal by address. not_found is absent; any other
// failure, or a reply that does not parse into a usable terminal, is
// unreadable.
func (a *Adapter) get(ctx context.Context, address string) (terminal, getState, *backend.StartCause) {
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindHcProbe,
		exec.MustFixed("get"),
		exec.MustFixed("--json"),
		exec.EndOfOptions(),
		exec.Opaque(address),
	))
	if err != nil {
		if errorKind(res.Stderr) == kindNotFound {
			return terminal{}, getAbsent, nil
		}
		c := classifyRunError(err)
		return terminal{}, getUnreadable, &c
	}
	var t terminal
	if derr := decodeStdout(res.Stdout, &t); derr != nil || !t.valid() {
		c := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("hc get returned no usable terminal"))
		return terminal{}, getUnreadable, &c
	}
	return t, getFound, nil
}
