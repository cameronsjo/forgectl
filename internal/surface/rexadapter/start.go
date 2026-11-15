// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

// Decision: how a worker's environment and bootstrap reach its Rex block
// (forgectl#1411, design problem 1).
//
// `rex run` and `rex new` have no environment flag (measured: FOO=x rex run
// -- sh -c 'echo $FOO' printed an empty value), so the question was how to
// give a worker FORGECTL_DRAIN_WORKER and the rest. The answer is that Rex
// does not need to carry them. A worker's environment is part of the sealed
// launch.Invocation, and the coordinator sends that invocation to the
// trampoline over the bootstrap socket after the handshake
// (internal/surface/handshake.go). The trampoline runs the harness with
// exactly that environment and never inherits the block's
// (internal/cli/surface_trampoline.go). The one thing a manager has to carry
// is the bootstrap command line: forgectl's path, `surface _exec`, the
// protocol version, the socket path and the one-time nonce.
//
// That line goes in a launch script, not in rex's argv. The options were:
//
//   - The line in rex's argv (`rex new -- /bin/sh -c <line>`), which is what
//     hcadapter does with hc. Rex keeps the command a block runs and reports
//     what runs in a block (process_changed events, with argv0, path and the
//     script being run) to every connected client, and a Rex server can
//     accept clients over a tailnet. Refused.
//   - `rex api call session.new_window` with initial_input, which types the
//     line into the block's shell: it lands on the screen, in the scrollback
//     `rex capture` returns and in the shell's history, and the call's
//     parameters are rex's argv anyway. Refused.
//   - An `env K=V prog` wrapper: puts values in argv and in `block call
//     process`. Refused, and not needed (see above).
//   - A launch script (chosen). exec.NewLaunchScript writes the line into a
//     file it creates exclusively, mode 0600, in a fresh directory of mode
//     0700 under os.TempDir(), and Rex is asked to run `/bin/sh <path>`. The
//     script's first line deletes it, so the file is gone as soon as sh has it
//     open; Start waits for that (awaitConsumed) and then removes the
//     directory, and removes both on every failure path. What Rex records and
//     reports is /bin/sh and a random path.
//
// The script is written by internal/exec, not here: the bootstrap is opaque to
// this package by construction (backend.BootstrapCommand), and NewLaunchScript
// is the seam's one other reveal besides starting a process
// (sealed.WriteNewFile, pinned to that one caller).
//
// Residual exposure, stated plainly:
//
//   - Any process running as the operator's uid can read the script in the
//     window between its creation and sh opening it (milliseconds, at most
//     consumeTimeout), and can read the trampoline's argv, which holds the
//     same line for as long as the worker runs. The second is true of every
//     backend (internal/exec's sensitive seam closes logs, not the process
//     table). The nonce is single-use and the socket checks the peer's uid,
//     so neither buys another uid anything.
//   - A forgectl killed between writing the script and removing it leaves the
//     file behind, mode 0600, holding a nonce whose listener is gone.
//   - The cwd and the display name do reach rex's argv (-c, --window), as they
//     reach every manager's: a manager has to know where to start and what to
//     show. They are sealed against logs, not against the manager.
//
// Rex is started with --shell=none, so the user's login shell and its profile
// do not run in a worker's block; the trampoline sets the harness's
// environment itself.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// Start creates the worker's session and block and returns what is known
// about whether Rex changed state.
//
// The order: identity (whoami) and the cwd check before anything is created;
// the launch script; `rex new`; the rename that puts the owner marker on the
// block; a read-back that the block carries it; then the wait for the script
// to be consumed. Any failure after `rex new` was attempted reconciles by the
// session label, which `rex new` sets atomically with creation.
func (a *Adapter) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	if err := spec.Validate(); err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	srv, cause := a.readiness(ctx, true)
	if cause != nil {
		return backend.NewNotMutated(*cause)
	}
	if cause := a.checkDir(ctx, spec.CWD()); cause != nil {
		return backend.NewNotMutated(*cause)
	}
	script, err := a.newScript(a.scriptDir, spec.Bootstrap().SensitiveArg())
	if err != nil {
		return backend.NewNotMutated(backend.NewStartCause(backend.FailureInternal, err))
	}
	tag := spec.Tag()

	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindRexCreate,
		exec.MustFixed("new"),
		exec.MustFixed("--json"),
		// A finished block stays listed with its exit code, so Probe can tell
		// finished from never-existed (forgectl#1411, design problem 2).
		exec.MustFixed("--keep-open"),
		exec.MustFixed("--shell=none"),
		exec.MustFixed("--cwd"),
		spec.CWD(),
		exec.MustFixed("--window"),
		spec.Name(),
		exec.Opaque(a.sessionLabel(tag)),
		exec.EndOfOptions(),
		exec.MustFixed(shellPath),
		exec.Opaque(script.Path()),
	))
	if runErr != nil {
		_ = script.Remove()
		return a.reconcile(ctx, tag, srv, classifyRunError(runErr))
	}
	var c created
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		_ = script.Remove()
		return a.reconcile(ctx, tag, srv, backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("rex new reported success with a truncated reply")))
	}
	if c, err = parseNew(raw); err != nil {
		_ = script.Remove()
		return a.reconcile(ctx, tag, srv, backend.NewStartCause(backend.FailureMalformedResponse,
			errors.New("rex new reported success without one readable session and block")))
	}

	if _, lerr := a.run.RunSensitive(ctx, a.command(exec.KindRexLabel,
		exec.MustFixed("block"),
		exec.MustFixed("rename"),
		exec.MustFixed("--session"),
		exec.Opaque(c.Session),
		exec.Opaque(c.Block),
		exec.Opaque(ownerLabel(tag)),
	)); lerr != nil {
		_ = script.Remove()
		return a.reconcile(ctx, tag, srv, classifyRunError(lerr))
	}
	blocks, state, bcause := a.listBlocks(ctx, exec.KindRexProbe, c.Session)
	if state != listFound || !ownsOnly(blocks, c.Block, ownerLabel(tag)) {
		_ = script.Remove()
		if bcause == nil {
			cc := backend.NewStartCause(backend.FailureMalformedResponse,
				errors.New("the block rex created does not carry this launch's owner label"))
			bcause = &cc
		}
		return a.reconcile(ctx, tag, srv, *bcause)
	}
	ref, err := a.reference(c, tag, srv)
	if err != nil {
		_ = script.Remove()
		return a.reconcile(ctx, tag, srv, backend.NewStartCause(backend.FailureMalformedResponse, err))
	}
	if cause := a.awaitConsumed(ctx, script); cause != nil {
		// The block exists and is ours; the service closes it.
		return backend.NewRefKnownWithCause(ref, *cause)
	}
	return backend.NewRefKnown(ref)
}

// ownsOnly reports whether blocks is exactly the one block id, labeled label.
// A fresh session holds the one block `rex new` made; a second one means the
// listing is not of the session Start just created.
func ownsOnly(blocks []block, id, label string) bool {
	return len(blocks) == 1 && blocks[0].ID == id && blocks[0].Label == label
}

// awaitConsumed waits until the launch script has removed itself, which it
// does once sh has opened it, then removes its directory. A script still
// there after consumeTimeout, a stat that cannot say, or a context that ends
// first removes the script and reports the cause: the worker then cannot
// start from it, and the launch fails closed.
func (a *Adapter) awaitConsumed(ctx context.Context, s launchScript) *backend.StartCause {
	deadline := time.Now().Add(a.consumeTimeout)
	for {
		done, err := s.Consumed()
		switch {
		case err != nil:
			_ = s.Remove()
			c := backend.NewStartCause(backend.FailureInternal, err)
			return &c
		case done:
			if rerr := s.Remove(); rerr != nil {
				c := backend.NewStartCause(backend.FailureInternal, rerr)
				return &c
			}
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			_ = s.Remove()
			class := backend.FailureCanceled
			if errors.Is(cerr, context.DeadlineExceeded) {
				class = backend.FailureTimeout
			}
			c := backend.NewStartCause(class, cerr)
			return &c
		}
		if !time.Now().Before(deadline) {
			_ = s.Remove()
			c := backend.NewStartCause(backend.FailureTimeout,
				errors.New("the worker's block did not read its launch script in time"))
			return &c
		}
		a.sleep(a.consumePoll)
	}
}

// reconcile settles an ambiguous create by the session label, which `rex new`
// set to the ownership name when it created the session. No such session is
// NotMutated; one, holding exactly one block that carries the owner label, is
// RefKnown-with-cause, so the service closes it; anything else is
// OutcomeUnknown, and the operator finds the session by its label.
func (a *Adapter) reconcile(ctx context.Context, tag backend.RecoveryTag, srv backend.ServerID, cause backend.StartCause) backend.StartResult {
	sessions, ok := a.listSessions(ctx, exec.KindRexReconcile)
	if !ok {
		return backend.NewOutcomeUnknown(tag, cause)
	}
	var mine []session
	for _, s := range sessions {
		if s.Label == a.sessionLabel(tag) {
			mine = append(mine, s)
		}
	}
	switch len(mine) {
	case 0:
		return backend.NewNotMutated(cause)
	case 1:
		blocks, state, _ := a.listBlocks(ctx, exec.KindRexReconcile, mine[0].ID)
		if state != listFound || len(blocks) != 1 || blocks[0].Label != ownerLabel(tag) {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		ref, err := a.reference(created{Session: mine[0].ID, Block: blocks[0].ID}, tag, srv)
		if err != nil {
			return backend.NewOutcomeUnknown(tag, cause)
		}
		return backend.NewRefKnownWithCause(ref, cause)
	}
	return backend.NewOutcomeUnknown(tag, cause)
}

// readiness runs `rex whoami --json` and fingerprints the server it reports.
// It refuses, as incompatible, any API revision but APIRevision and any
// endpoint that is not a local unix socket. With launch set (Start only) it
// also refuses a server shared with tailnet members, or one that does not say
// it is not: a launch would put a worker's cwd and label where other machines
// can read them. Close and Probe pass launch=false, because closing a block is
// the remedy for sharing that was switched on after a launch, and the calls
// carry only ids.
func (a *Adapter) readiness(ctx context.Context, launch bool) (backend.ServerID, *backend.StartCause) {
	res, err := a.run.RunSensitive(ctx, a.command(exec.KindRexReadiness,
		exec.MustFixed("whoami"),
		exec.MustFixed("--json"),
	))
	if err != nil {
		c := classifyRunError(err)
		return backend.ServerID{}, &c
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		c := backend.NewStartCause(backend.FailureMalformedResponse, errors.New("rex whoami output was truncated"))
		return backend.ServerID{}, &c
	}
	s, perr := parseWhoami(raw)
	if perr != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, perr)
		return backend.ServerID{}, &c
	}
	if s.Revision != APIRevision {
		c := backend.NewStartCause(backend.FailureIncompatible,
			errors.New("the rex server speaks an API revision this build was not measured on (it needs revision 14)"))
		return backend.ServerID{}, &c
	}
	if !localEndpoint(s.Endpoint) {
		c := backend.NewStartCause(backend.FailureIncompatible,
			errors.New("the rex server is not a local unix-socket server; forgectl launches workers on this machine only"))
		return backend.ServerID{}, &c
	}
	if launch && s.TailnetOpen {
		c := backend.NewStartCause(backend.FailureIncompatible,
			errors.New("the rex server shares sessions with tailnet members (or does not report that it does not); forgectl launches workers only on a server no other machine can see"))
		return backend.ServerID{}, &c
	}
	id, ferr := backend.FingerprintRexServer(s.Endpoint, s.HostID, s.InstanceID)
	if ferr != nil {
		c := backend.NewStartCause(backend.FailureMalformedResponse, ferr)
		return backend.ServerID{}, &c
	}
	return id, nil
}

// localEndpoint reports whether endpoint is a unix-socket address with a
// path after the scheme.
func localEndpoint(endpoint string) bool {
	path, ok := strings.CutPrefix(endpoint, localEndpointPrefix)
	return ok && path != ""
}

// checkDir proves the target directory exists before Rex is asked to start
// in it: Rex starts a block whose -c names a missing directory in $HOME and
// reports success (measured). It runs /bin/test -d through the sensitive
// runner, the cwd staying sealed. It is a check at one moment, not a lock:
// the trampoline's own chdir is what finally refuses a directory that went
// away since.
func (a *Adapter) checkDir(ctx context.Context, cwd exec.Arg) *backend.StartCause {
	res, err := a.run.RunSensitive(ctx, exec.SensitiveCommand{
		Kind:      exec.KindRexPreflight,
		Path:      exec.Secret(testPath),
		Args:      []exec.Arg{exec.MustFixed("-d"), cwd},
		Env:       []exec.EnvMutation{exec.UnsetLauncherSessionHandles()},
		StdoutCap: stderrCap,
		StderrCap: stderrCap,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, exec.ErrNonzeroExit) && res.ExitCode == 1:
		c := backend.NewStartCause(backend.FailureInternal,
			errors.New("the target directory does not exist, and rex would start the worker in $HOME instead"))
		return &c
	}
	c := classifyRunError(err)
	return &c
}

// reference binds a created block to this server.
func (a *Adapter) reference(c created, tag backend.RecoveryTag, srv backend.ServerID) (backend.Ref, error) {
	id, err := backend.NewRexIdentity(c.Session, c.Block)
	if err != nil {
		return backend.Ref{}, err
	}
	return backend.NewRexRef(a.source, srv, tag, id)
}
