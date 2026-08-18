package surface

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
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
// It carries no adapter output, and nothing the operator running the command
// does not already have. Its cause can name the harness path — a policy refusal
// does — and that is fine here: this error surfaces in the terminal where the
// operator typed the command, not in the manager's pane. The trampoline is the
// half whose stderr belongs to the manager, and its errors are bare sentinels
// for exactly that reason. What never appears here either way is the argv or
// the environment.
//
// The mutation outcome is the field that matters — a launch that failed *without*
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
		msg += ", " + e.Cleanup.String()
	}
	if e.Recovery != "" {
		msg += ", recovery tag " + e.Recovery
	}
	msg += ")"

	// The cause is appended, not left to Unwrap. This string is what the CLI
	// prints, and without it every setup failure renders identically —
	// a relative working directory, a refused binary, and a socket path over
	// the platform limit all become "launch failed in setup (not-mutated)".
	// The last of those has a remedy attached to it ("set TMPDIR to a shorter
	// path") that the operator would never see.
	//
	// The causes are safe to render: the invocation never reaches one. They are
	// the run directory's, the policy's, and the adapter's category errors,
	// each already built to be printed.
	if e.err != nil {
		msg += ": " + e.err.Error()
	}
	return msg
}

func (e *LaunchError) String() string { return e.Error() }

// LogValue keeps a structured logger on the same rendering as %v. Every other
// value in this package carries one; an error that logged its fields instead
// would print the wrapped cause through a path none of them audit.
func (e *LaunchError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

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
//
// The invocation is held behind a pointer rather than as a plain field, and
// that is the containment mechanism rather than a style choice. fmt reaches a
// value in an *unexported* struct field by reflection — CanInterface is false
// there, so Format and String are never consulted — and a plain
// launch.Invocation would then print its argv and environment in full under
// %+v of any struct holding a request in an unexported field, which is the
// natural shape for a command struct. A pointer prints as an address at every
// depth, so reflection has nothing to reach.
//
// This is the third type in this package to need that treatment; exec.Arg uses
// a closure and StartCause uses a pointer box for exactly the same reason.
type LaunchRequest struct {
	// Name is the display name the manager gives the workspace. The manager
	// necessarily sees it, so it is not sensitive.
	Name string

	// Self is this forgectl executable, which the trampoline re-enters.
	//
	// Empty means "resolve it": os.Executable is what every caller would pass,
	// and a caller that gets it wrong does not get a compile error — they get a
	// relative argv[0] or a symlink, and a trampoline that re-enters the wrong
	// binary. Keeping the field lets a test override it.
	Self string

	invocation *launch.Invocation
}

// NewLaunchRequest builds a request around an invocation.
//
// It is a constructor rather than a struct literal because the invocation must
// not be an exported field: see the type's comment. The value is copied, so a
// caller mutating theirs afterwards cannot change what this launch delivers.
func NewLaunchRequest(name string, inv launch.Invocation) LaunchRequest {
	copied := inv
	return LaunchRequest{Name: name, invocation: &copied}
}

// Invocation returns what the harness will run, and reports whether one was
// supplied. A zero-value request carries none, and refusing beats returning an
// empty invocation that would look like a launch with no arguments.
func (r LaunchRequest) Invocation() (launch.Invocation, bool) {
	if r.invocation == nil {
		return launch.Invocation{}, false
	}
	return *r.invocation, true
}

// The redaction methods below cover the direct-print case; the pointer field
// covers the reflective one. Value receivers so a pointer is covered too.
func (LaunchRequest) String() string   { return "surface launch request " + exec.Redacted }
func (LaunchRequest) GoString() string { return "surface launch request " + exec.Redacted }

func (r LaunchRequest) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(r.String()))
		return
	}
	_, _ = io.WriteString(f, r.String())
}

func (r LaunchRequest) LogValue() slog.Value { return slog.StringValue(r.String()) }

func (r LaunchRequest) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(r.String())), nil
}

func (r LaunchRequest) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// Service launches a harness inside a terminal manager.
type Service struct {
	adapter backend.Adapter
	policy  Policy
	base    string
}

// NewService wires a service to one adapter.
//
// base is the directory the private run directory is created under; empty uses
// the system temp directory.
//
// It is an operator-visible knob rather than a constant because the socket path
// has a hard platform ceiling — macOS caps sun_path near 104 bytes, and a long
// TMPDIR is most of that before forgectl adds anything. Somewhere to point it is
// the remedy RunDir names when it refuses. Tests use it for the same reason.
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
// It is a var rather than a const so a test can shorten it. Left as a const it
// is untestable at any value, including a mistaken zero: a mutation deleting the
// deadline entirely leaves the suite green and sixty seconds slower.
var acceptTimeout = 60 * time.Second

// Launch runs the whole state machine.
//
// On success the surface exists, the harness is running in it, and this process
// has released it. On any failure before the acknowledgement, the surface is
// rolled back where the adapter's own report says that is possible, and the
// error says what it could not undo.
func (s *Service) Launch(ctx context.Context, req LaunchRequest) (Result, error) {
	inv, ok := req.Invocation()
	if !ok {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: errors.New("no invocation")}
	}
	if err := s.validate(req); err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	// Both capabilities are required *before* anything is created. An adapter
	// that can create but not close would leave a launch with no rollback, and
	// discovering that after the create is discovering it too late.
	// The returned capabilities are threaded into rollback rather than looked
	// up again there. A second call could not fail — same adapter, nothing
	// mutates it in between — so its error branch would be dead code that a
	// reader has to work out is unreachable.
	closer, err := backend.RequireCapabilities(s.adapter)
	if err != nil {
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
	bound, err := lc.Listen(ctx, "unix", runDir.SocketPath())
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}
	// Asserted once, here, where a failure is a refusal rather than a skipped
	// control — handshake then takes the concrete type and cannot be handed a
	// listener it will not bound.
	listener, ok := bound.(*net.UnixListener)
	if !ok {
		_ = bound.Close()
		return Result{}, &LaunchError{Phase: PhaseSetup,
			err: fmt.Errorf("a unix listener is %T, not *net.UnixListener", bound)}
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

	self, err := resolveSelf(req.Self)
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	bootstrap, err := s.bootstrapCommand(self, runDir.SocketPath(), nonce)
	if err != nil {
		return Result{}, &LaunchError{Phase: PhaseSetup, err: err}
	}

	spec, err := backend.NewStartSpec(inv.CWD, req.Name, tag, bootstrap)
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
		return Result{}, s.rollback(ctx, PhaseStart, result, closer, err)
	}
	if result.Failed() {
		return Result{}, s.rollback(ctx, PhaseStart, result, closer, errors.New("adapter did not create the surface"))
	}

	ref, ok := result.Ref()
	if !ok {
		// Unreachable through the constructors — a successful result carries a
		// ref by construction — and refused rather than assumed, because the
		// alternative is committing to a surface we cannot name.
		return Result{}, s.rollback(ctx, PhaseStart, result, closer, errors.New("adapter reported success without a reference"))
	}

	handoff, err := s.handshake(ctx, listener, nonce)
	if err != nil {
		return Result{}, s.rollback(ctx, PhaseHandshake, result, closer, err)
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

	if err := handoff.Send(inv); err != nil {
		return Result{}, s.rollback(ctx, PhaseHandshake, result, closer, err)
	}

	// The commit point. A nil return here means a complete, authenticated
	// exec_started frame arrived, which means the harness crossed the
	// fork/exec boundary. Nothing weaker counts.
	if err := handoff.AwaitStart(); err != nil {
		return Result{}, s.rollback(ctx, PhaseCommit, result, closer, err)
	}

	// Committed. The surface is the manager's now, and this function must not
	// close it — note that no cleanup path is reachable past this line, which
	// is the point: a cancelled ctx from here on is a caller changing their
	// mind about waiting, not about the session that is already running.
	return Result{ref: ref}, nil
}

// validate checks what can be checked before anything is created.
func (s *Service) validate(req LaunchRequest) error {
	inv, ok := req.Invocation()
	if !ok {
		return errors.New("no invocation")
	}
	if s.adapter == nil {
		return errors.New("no adapter")
	}
	if req.Name == "" {
		return errors.New("no display name")
	}
	if inv.CWD == "" || !filepath.IsAbs(inv.CWD) {
		return errors.New("target directory must be absolute")
	}
	if inv.Binary.Path == "" {
		return errors.New("no harness binary")
	}
	// The policy's self-loop check needs the running executable, and refuses
	// rather than skipping when it cannot have one — so resolution happens
	// here too rather than being left to the later call.
	self, err := resolveSelf(req.Self)
	if err != nil {
		return err
	}
	// Absolute, for the same reason CWD is. This word becomes argv[0] of a
	// command a terminal manager types into a shell, so a relative value would
	// resolve against that shell's PATH and cwd rather than against this
	// process. It would also silently disarm the self-loop guard: AcceptBinary
	// stats this path and, by design, warns-and-admits when it cannot.
	if !filepath.IsAbs(self) {
		return errors.New("the forgectl executable to re-enter must be an absolute path")
	}
	return s.policy.AcceptBinary(inv.Binary, self)
}

// selfPathFn is the seam for resolving this executable, matching peerUIDFn and
// the CLI's lstatFn. A test that could not move it would be asserting against
// whatever binary `go test` happened to build.
var selfPathFn = SelfPath

// resolveSelf prefers an explicit path and otherwise asks the runtime.
//
// Defaulting rather than requiring is the safer shape: os.Executable is what
// every caller would pass, and a caller that gets it wrong does not get a
// compile error — they get a relative argv[0] or an unresolved symlink, and a
// trampoline that re-enters something other than this binary.
func resolveSelf(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return selfPathFn()
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
// The parameter is the concrete listener, not net.Listener, so the deadline is
// unconditional. Widened, it needed a type assertion — and a failed assertion
// silently yields no timeout at all, so wrapping the listener in a decorator
// later would remove the only bound without a compile error.
func (s *Service) handshake(ctx context.Context, listener *net.UnixListener, nonce Nonce) (*Handoff, error) {
	if err := listener.SetDeadline(time.Now().Add(acceptTimeout)); err != nil {
		return nil, err
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
//
// Note what the matrix is NOT holding: committed is always false here, because
// every call site is before AwaitStart returns nil. The never-close-after-commit
// rule is therefore enforced by control flow — no rollback call appears past the
// commit point — rather than by the typed matrix that is usually credited with
// it. Both are true; only one of them is load-bearing in this file.
func (s *Service) rollback(ctx context.Context, phase Phase, result backend.StartResult, closer backend.Capabilities, cause error) error {
	// An outcome is only worth reporting if the result it came from is valid.
	//
	// NotMutated is the zero MutationOutcome, so reading Outcome() off a result
	// that failed Validate turns a malformed adapter reply into the *positive*
	// claim that nothing was created — which is precisely what the backend
	// package refuses to allow ("there is no 'an error came back so presumably
	// nothing happened'"). PlanCleanup gets this right on its side; reading the
	// field directly here would flatten it back out.
	outcome := result.Outcome()
	if result.Validate() != nil {
		outcome = backend.OutcomeUnknown
	}
	launchErr := &LaunchError{Phase: phase, Outcome: outcome, err: cause}

	plan, _ := backend.PlanCleanup(result, false)
	ref, shouldClose := plan.Close()
	if !shouldClose {
		launchErr.Cleanup = plan.Outcome()
		// Nothing was closed, so anything the adapter may have left is still
		// there and the tag is the only handle on it.
		if !launchErr.Cleanup.Satisfied() {
			if tag, ok := result.RecoveryTag(); ok {
				launchErr.Recovery = tag.String()
			}
		}
		return launchErr
	}

	// Cleanup gets its own context. The caller's may be the reason we are here
	// — a cancelled launch still has to undo what it did, and inheriting the
	// cancellation would skip exactly the work cancellation created.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	closeResult := closer.Close(cleanupCtx, ref)
	launchErr.Cleanup = backend.CleanupOutcomeFor(closeResult)

	// The recovery tag is set here rather than up front, and only when the
	// surface may still be there. Its field doc promises exactly that, and
	// setting it unconditionally broke the promise in the ordinary case:
	// RecoveryTag() answers for a RefKnown result too, so a launch that failed
	// and then closed cleanly told the operator to go hunting for a container
	// that had just been removed.
	if !launchErr.Cleanup.Satisfied() {
		if tag, ok := result.RecoveryTag(); ok {
			launchErr.Recovery = tag.String()
		}
	}
	return launchErr
}

// cleanupTimeout bounds the rollback. It is short: the surface was created
// moments ago, and an operator waiting on a failed launch should not also wait
// on a manager that has stopped answering.
const cleanupTimeout = 15 * time.Second
