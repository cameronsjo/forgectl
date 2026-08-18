package surface

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// The outer half of a surface launch: the process that owns the invocation.
//
// Launch is a state machine, and the states are worth naming because the whole
// design is about which of them can still be rolled back:
//
//	resolved -> listening -> started -> authenticated -> sent -> COMMITTED
//
// Everything before COMMITTED is ours to undo. At COMMITTED the surface belongs
// to the terminal manager and the harness is running inside it, so nothing here
// may close it — including a cancelled context, which is the case most likely
// to be got wrong, because cancellation normally means "undo".
//
// The ordering of the setup steps is not arrangement. The socket is bound
// before the bootstrap command is built, because the bootstrap carries the
// socket path and the manager is handed that command: if the path were merely
// *chosen* and not yet occupied, a same-uid process reading it could bind there
// first and become the invocation server. Binding first means the name is taken
// before anyone else learns it.

// Launch phases, used to say where a failure happened without describing it.
type Phase uint8

const (
	// PhaseSetup covers everything before the manager is asked to do anything:
	// validation, the run directory, the listener, the nonce.
	PhaseSetup Phase = iota
	// PhaseStart is the adapter's create call — the first mutating step.
	PhaseStart
	// PhaseHandshake covers accept, authentication, and delivery.
	PhaseHandshake
	// PhaseCommit covers waiting for the exact acknowledgement.
	PhaseCommit
)

func (p Phase) String() string {
	switch p {
	case PhaseSetup:
		return "setup"
	case PhaseStart:
		return "start"
	case PhaseHandshake:
		return "handshake"
	case PhaseCommit:
		return "commit"
	default:
		return "unknown"
	}
}

var (
	// ErrLaunch is the category every launch failure carries.
	ErrLaunch = errors.New("surface: launch failed")

	// ErrCapabilities reports an adapter that cannot roll back what it creates.
	ErrCapabilities = fmt.Errorf("%w: adapter cannot close or probe what it creates", ErrLaunch)
)

// LaunchError reports a failed launch in terms an operator can act on: which
// phase, what happened to the manager's state, and whether the rollback worked.
//
// It deliberately carries no adapter output and no invocation detail. The
// mutation outcome is the field that matters — a launch that failed *without*
// mutating anything and one that may have left a container behind need
// different responses, and collapsing them into "failed" loses that.
type LaunchError struct {
	Phase   Phase
	Outcome backend.MutationOutcome
	Cleanup backend.CleanupOutcome

	// Recovery names the container an operator may need to remove by hand,
	// present only when the outcome is unknown and a tag was issued.
	Recovery string

	err error
}

func (e *LaunchError) Error() string {
	msg := fmt.Sprintf("surface: launch failed in %s (%s", e.Phase, e.Outcome)
	if e.Cleanup != backend.CleanupUnspecified && e.Cleanup != backend.CleanupNotApplicable {
		msg += ", cleanup " + e.Cleanup.String()
	}
	if e.Recovery != "" {
		msg += ", recovery tag " + e.Recovery
	}
	return msg + ")"
}

func (e *LaunchError) Unwrap() error { return e.err }

// Is lets callers match the category without reaching for the struct.
func (e *LaunchError) Is(target error) bool { return target == ErrLaunch }

// Result is a committed launch.
type Result struct {
	ref backend.Ref
}

// Ref is the manager-native handle to the surface that was created.
func (r Result) Ref() backend.Ref { return r.ref }

// LaunchRequest is one launch.
type LaunchRequest struct {
	// Name is the display name the manager gives the workspace. The manager
	// necessarily sees it, so it is not sensitive.
	Name string

	// Invocation is what the harness will run. It is the secret this whole
	// package exists to keep off the manager's command line.
	Invocation launch.Invocation

	// Self is this forgectl executable, which the trampoline re-enters.
	Self string
}

// Service launches a harness inside a terminal manager.
type Service struct {
	adapter backend.Adapter
	policy  Policy
	base    string
}

// NewService wires a service to one adapter.
//
// base is the directory the private run directory is created under; an empty
// base uses the system temp directory. It is a field rather than a constant so
// a test can point it somewhere short — macOS caps a socket path near 104 bytes
// and the default temp path is most of that already.
func NewService(adapter backend.Adapter, policy Policy, base string) *Service {
	return &Service{adapter: adapter, policy: policy, base: base}
}

// observeBootstrap is nil in production and set by an in-package test.
//
// The socket path and the nonce are not reachable from outside this package by
// construction: they travel to the trampoline inside an opaque bootstrap that
// no accessor unwraps, and that inaccessibility is the property most of the
// tests here assert. Which leaves the commit-path test needing the one thing it
// is not allowed to have, so it takes it through a seam that exists only for
// that — the same shape as peerUIDFn in handoff.go and lstatFn in the CLI's
// socket check.
var observeBootstrap func(socket string, nonce Nonce)

// acceptTimeout bounds the wait for the trampoline to appear.
//
// It starts when the adapter reports the surface created, so it covers a
// manager spawning a pane, a shell starting, and forgectl re-execing. Generous,
// because the cost of it being short is a rolled-back launch on a loaded
// machine; bounded, because the alternative is a hung command when the manager
// silently never types the command it was given.
const acceptTimeout = 60 * time.Second

// Launch runs the whole state machine.
//
// On success the surface exists, the harness is running in it, and this process
// has released it. On any failure before the acknowledgement, the surface is
// rolled back where the adapter's own report says that is possible, and the
// error says what it could not undo.
func (s *Service) Launch(ctx context.Context, req LaunchRequest) (Result, error) {
	if err := s.validate(req); err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	// Both capabilities are required *before* anything is created. An adapter
	// that can create but not close would leave a launch with no rollback, and
	// discovering that after the create is discovering it too late.
	if _, err := backend.RequireCapabilities(s.adapter); err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: fmt.Errorf("%w: %w", ErrCapabilities, err)}
	}

	runDir, err := NewRunDir(s.base)
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}
	defer func() { _ = runDir.Close() }()

	// Bind before the bootstrap exists. The bootstrap carries this path and is
	// handed to a manager the threat model does not trust; a path that is
	// chosen but not yet occupied can be taken by anyone who reads it.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", runDir.SocketPath())
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}
	defer func() { _ = listener.Close() }()

	nonce, err := NewNonce()
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	tag, err := backend.NewRecoveryTag()
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	bootstrap, err := s.bootstrapCommand(req.Self, runDir.SocketPath(), nonce)
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	spec, err := backend.NewStartSpec(req.Invocation.CWD, req.Name, tag, bootstrap)
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	if observeBootstrap != nil {
		observeBootstrap(runDir.SocketPath(), nonce)
	}

	// From here the manager may have been mutated, so every exit runs through
	// the cleanup decision rather than returning directly.
	result := s.adapter.Start(ctx, spec)
	if err := result.Validate(); err != nil {
		return Result{}, s.rollback(ctx, PhaseStart, result, err)
	}
	if result.Failed() {
		return Result{}, s.rollback(ctx, PhaseStart, result, errors.New("adapter did not create the surface"))
	}

	ref, ok := result.Ref()
	if !ok {
		// Unreachable through the constructors — a successful result carries a
		// ref by construction — and refused rather than assumed, because the
		// alternative is committing to a surface we cannot name.
		return Result{}, s.rollback(ctx, PhaseStart, result, errors.New("adapter reported success without a reference"))
	}

	handoff, err := s.handshake(ctx, listener, nonce)
	if err != nil {
		return Result{}, s.rollback(ctx, PhaseHandshake, result, err)
	}
	defer func() { _ = handoff.Close() }()

	// Cancellation has to reach the two calls below, and neither takes a
	// context: they are bounded only by the handshake deadline, so a caller
	// who gave up would otherwise wait out that whole budget before the
	// surface is rolled back.
	//
	// Closing the handoff is what unblocks them, and it is safe on both sides
	// of the commit — a handoff is a socket, not the surface. Closing it after
	// commit costs a connection the trampoline has already finished with;
	// closing the *surface* after commit is the thing that must never happen,
	// and that is prevented by there being no cleanup path past this block at
	// all rather than by this watcher's timing.
	settled := make(chan struct{})
	defer close(settled)
	go func() {
		select {
		case <-ctx.Done():
			_ = handoff.Close()
		case <-settled:
		}
	}()

	if err := handoff.Send(req.Invocation); err != nil {
		return Result{}, s.rollback(ctx, PhaseHandshake, result, err)
	}

	// The commit point. A nil return here means a complete, authenticated
	// exec_started frame arrived, which means the harness crossed the
	// fork/exec boundary. Nothing weaker counts.
	if err := handoff.AwaitStart(); err != nil {
		return Result{}, s.rollback(ctx, PhaseCommit, result, err)
	}

	// Committed. The surface is the manager's now, and this function must not
	// close it — note that no cleanup path is reachable past this line, which
	// is the point: a cancelled ctx from here on is a caller changing their
	// mind about waiting, not about the session that is already running.
	return Result{ref: ref}, nil
}

// validate checks what can be checked before anything is created.
func (s *Service) validate(req LaunchRequest) error {
	if s.adapter == nil {
		return errors.New("no adapter")
	}
	if req.Name == "" {
		return errors.New("no display name")
	}
	if req.Self == "" {
		return errors.New("no forgectl executable to re-enter")
	}
	if req.Invocation.CWD == "" || !filepath.IsAbs(req.Invocation.CWD) {
		return errors.New("target directory must be absolute")
	}
	if req.Invocation.Binary.Path == "" {
		return errors.New("no harness binary")
	}
	return s.policy.AcceptBinary(req.Invocation.Binary, req.Self)
}

// bootstrapCommand builds the command the manager will be asked to type.
//
// It is quoted here and opaque from here: the returned value renders redacted
// through every formatting verb, and the only way back to the text is the
// production runner unwrapping it immediately before exec. The manager sees the
// command — it has to, it types it — but nothing on the way there can log it.
func (s *Service) bootstrapCommand(self, socket string, nonce Nonce) (backend.BootstrapCommand, error) {
	line, err := QuoteCommand([]string{
		self, "surface", "_exec",
		"--protocol", fmt.Sprintf("%d", ProtocolVersion),
		"--socket", socket,
		"--nonce", nonce.String(),
	})
	if err != nil {
		return backend.BootstrapCommand{}, fmt.Errorf("quote bootstrap command: %w", err)
	}
	return backend.NewBootstrapCommand(exec.Opaque(line))
}

// handshake accepts exactly one connection and authenticates it.
//
// One, not a loop. The nonce is single-use, and an accept loop would make it
// re-presentable: a peer that guessed wrong could try again, and a peer that
// authenticated could be followed by another. The listener closes as soon as
// this returns, so the socket stops existing as an entry point the moment it
// has served its purpose.
func (s *Service) handshake(ctx context.Context, listener net.Listener, nonce Nonce) (*Handoff, error) {
	deadline := time.Now().Add(acceptTimeout)
	if unixListener, ok := listener.(*net.UnixListener); ok {
		if err := unixListener.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	// A cancelled context has to interrupt the accept, or a caller who gave up
	// waits the full timeout. Closing the listener is what unblocks it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()

	conn, err := listener.Accept()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	// Stop listening the moment there is a peer, rather than at return.
	//
	// Only one connection is ever served either way — there is no second
	// Accept — so a later dial would sit unanswered in the backlog and learn
	// nothing. But "unanswered" is a weaker statement than "refused", and it
	// only holds as long as nobody adds a second Accept later. Closing here
	// makes the socket stop existing as an entry point at the first peer,
	// which is when it has finished its job.
	if err := listener.Close(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// Accept closes conn itself on refusal, so there is no leak on this path.
	return Accept(conn, nonce)
}

// rollback decides what to do about a manager that may have been mutated, and
// reports the whole outcome rather than just the original failure.
//
// The decision is not made here. backend.PlanCleanup owns the matrix — which
// outcomes are safe to close, which must never be guessed at — and this wires
// it rather than re-deriving it, because a second implementation of that matrix
// is a second chance to get "outcome unknown" wrong.
func (s *Service) rollback(ctx context.Context, phase Phase, result backend.StartResult, cause error) error {
	launchErr := &LaunchError{Phase: phase, Outcome: result.Outcome(), err: cause}
	if tag, ok := result.RecoveryTag(); ok {
		launchErr.Recovery = tag.String()
	}

	plan, _ := backend.PlanCleanup(result, false)
	ref, shouldClose := plan.Close()
	if !shouldClose {
		launchErr.Cleanup = plan.Outcome()
		return launchErr
	}

	// Cleanup gets its own context. The caller's may be the reason we are here
	// — a cancelled launch still has to undo what it did, and inheriting the
	// cancellation would skip exactly the work cancellation created.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	closer, err := backend.RequireCapabilities(s.adapter)
	if err != nil {
		launchErr.Cleanup = backend.CleanupFailed
		return launchErr
	}

	closeResult := closer.Close(cleanupCtx, ref)
	launchErr.Cleanup = backend.CleanupOutcomeFor(closeResult)
	return launchErr
}

// cleanupTimeout bounds the rollback. It is short: the surface was created
// moments ago, and an operator waiting on a failed launch should not also wait
// on a manager that has stopped answering.
const cleanupTimeout = 15 * time.Second
