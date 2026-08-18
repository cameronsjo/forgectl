package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The trampoline: the inner half of `forgectl surface`.
//
// A terminal manager was asked to type `forgectl surface _exec …`, and this is
// what that command runs. It dials a private socket, proves it is the process
// forgectl asked for by presenting a rendezvous nonce, receives the harness
// invocation over that socket, execs it, and then gets out of the way.
//
// Two properties shape everything here.
//
// The invocation never appears in this process's argv, and it never appears in
// this process's diagnostics either. Both matter, and the second is the easier
// one to lose: this process's stderr is the manager's pane, so an error that
// names the harness path hands over by message what the socket withheld from
// argv. Every refusal below is therefore a category — the wire carries a closed
// enum, and the local errors are bare sentinels.
//
// What the mechanism actually buys is worth stating precisely, because the
// checks here can read as more than they are. The threat model grants a hostile
// manager the socket path and the nonce, both of which it typed, and the
// protocol authenticates in one direction only — the trampoline proves itself
// to the outer, not the reverse. Such a manager can dial the socket itself and
// receive the invocation without ever exec'ing anything. So the property is
// that the invocation is not exposed *through argv*, which defeats every
// passive same-uid observer: ps, process accounting, shell history, a scrollback
// buffer. It is not a defence against an active hostile manager, and the
// re-validation below is shape checking and defence in depth rather than an
// authorization boundary.
//
// And acknowledgement is exact. The outer process commits — stops being able to
// roll the surface back — only when it reads an exec_started frame, so this
// sends one only after the operating system reports that the child's directory
// change and exec both succeeded. Receiving the invocation is not commit;
// building the command is not commit; a live socket is not commit.
//
// After acknowledgement it is a transparent reaper and nothing more: it waits
// for the harness, mirrors its exit status, and performs no restart, health
// polling, or supervision of any kind. Staying alive long enough to do that is
// itself work — see the signal handling in Run.

var (
	// errSocketUnsafe reports a bootstrap socket this process will not dial.
	errSocketUnsafe = errors.New("forgectl: surface bootstrap socket is not safe to dial")

	// errHarnessUnusable reports a received invocation the trampoline refuses.
	errHarnessUnusable = errors.New("forgectl: surface invocation is not runnable")

	// errHarnessUnstartable reports a harness that could not be started. It
	// carries no detail for the same reason the wire category does not: the
	// operating system's message quotes the harness path, and this error
	// reaches the pane's stderr, which belongs to the terminal manager.
	errHarnessUnstartable = errors.New("forgectl: the harness could not be started")

	// errUnacknowledged reports a harness that ran without the launch ever
	// being acknowledged, so the outer is rolling the surface back.
	errUnacknowledged = errors.New("forgectl: surface launch was not acknowledged")

	// errHarnessExit carries the child's failure without describing it. The
	// harness has already written whatever it wanted to the inherited stderr;
	// re-reporting it here would be a second, worse copy — which is why Execute
	// suppresses the message for this sentinel and keeps only its exit code.
	errHarnessExit = errors.New("forgectl: harness exited non-zero")
)

// productionTrampoline is the real runtime behind the bootstrap classifier.
type productionTrampoline struct{}

// Run performs the whole trampoline lifecycle.
func (productionTrampoline) Run(ctx context.Context, req bootstrapRequest) error {
	// Clean before use. The path came from argv a terminal manager typed, and
	// while forgectl produced it, the process that typed it is exactly the one
	// the threat model does not trust.
	socket := filepath.Clean(req.socketPath())
	if err := checkSocketOwner(socket); err != nil {
		return err
	}

	nonce, err := surface.ParseNonce(req.nonceValue())
	if err != nil {
		// The classifier already validated the encoding, so this is
		// unreachable in practice — and it refuses rather than proceeding with
		// a zero-value nonce, which would fail open at the far end.
		return errBootstrapMalformed
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		// Bare, like every other refusal here: the dial error names the socket
		// path, and this goes to the manager's pane.
		return errSocketUnsafe
	}
	defer func() { _ = conn.Close() }()

	boot, invocation, err := surface.Dial(conn, nonce)
	if err != nil {
		return err
	}

	cmd, err := harnessCommand(invocation)
	if err != nil {
		// The outer is waiting on a result frame. Telling it the invocation was
		// refused lets it roll back immediately instead of timing out — and the
		// category is all it gets, because our real error names the path.
		_ = boot.Failed(surface.FailInvocation)
		return err
	}

	// Terminal signals belong to the harness, not to its reaper.
	//
	// The child stays in the pane's foreground process group so Ctrl-C reaches
	// it — but the trampoline is in that group too, and Go's default
	// disposition for SIGINT kills it while it sits in Wait. Reproduced: the
	// harness traps SIGINT and keeps running, the reaper dies, and the session
	// is orphaned on the tty with the shell's prompt back and the exit status
	// this file exists to mirror thrown away.
	//
	// Notify rather than Ignore, and the ordering is not interchangeable.
	// signal.Ignore installs a real SIG_IGN, and an ignored disposition is the
	// one thing execve *preserves* into the child — probed, and the child then
	// never sees Ctrl-C at all. Notify changes only this process's handling, so
	// the child's dispositions still reset to default at exec.
	//
	// SIGTSTP is deliberately not caught: it stops rather than kills, and a
	// reaper suspending alongside the session it is waiting on is correct.
	// SIGTERM is not caught either — a kill aimed at this process should work.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(signals)
	go func() {
		for range signals { //nolint:revive // draining is the whole behaviour
		}
	}()

	if err := cmd.Start(); err != nil {
		_ = boot.Failed(startFailureCategory(err, cmd.Dir))
		// The error is a category, not a description. os/exec's message quotes
		// the harness path, and this error is printed to the pane's stderr —
		// which is the terminal manager's, the party the socket exists to keep
		// the invocation from.
		return errHarnessUnstartable
	}

	// The child exists. On Darwin and Linux a successful Start means the fork,
	// the directory change, and the exec all succeeded — the runtime reports a
	// failure in any of them through its error pipe rather than returning nil —
	// which is what makes this acknowledgement exact rather than optimistic.
	if err := boot.Started(); err != nil {
		// The harness is running and the outer will never commit. It still owns
		// the surface and will close it, which may terminate this child; that
		// is the correct trade, because the alternative is a committed success
		// the outer never agreed to. Reaping continues either way so the child
		// is not orphaned in the meantime.
		_ = boot.Close()
		if reapErr := reap(cmd); reapErr != nil {
			return reapErr
		}
		// The harness exited cleanly, but this launch was never acknowledged
		// and the outer is rolling it back. Returning nil here would report
		// success for a launch that did not happen.
		return errUnacknowledged
	}

	// Done being a protocol peer. The socket closes here rather than at return,
	// so the outer's read completes promptly instead of waiting on a harness
	// session that may run for hours.
	_ = boot.Close()

	return reap(cmd)
}

// harnessCommand validates a received invocation and builds the command.
//
// The checks repeat work the outer process already did, deliberately. The
// trampoline cannot tell a genuine outer from something that reached the socket
// first, so it verifies what it holds rather than trusting the sender it
// authenticated. They are also cheap relative to what follows: an exec.
// Every refusal here is the bare sentinel. Naming which check failed, or which
// path failed it, would put the harness path and the target directory on the
// pane's stderr — the manager's, and the one place this design is keeping the
// invocation away from. The outer learns the category over the wire; an
// operator debugging a genuinely broken configuration has the outer's own
// diagnostics, which run on the trusted side.
func harnessCommand(inv surface.Invocation) (*osexec.Cmd, error) {
	info, err := os.Stat(inv.Path)
	if err != nil {
		return nil, errHarnessUnusable
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errHarnessUnusable
	}

	dir, err := os.Stat(inv.CWD)
	if err != nil || !dir.IsDir() {
		return nil, errHarnessUnusable
	}

	// Deliberately not CommandContext. Binding the harness to this context
	// would let a cancellation upstream of the trampoline kill a session the
	// outer has already committed to — the opposite of the ownership transfer
	// that acknowledgement performs. After commit, the harness's lifetime is
	// the surface's business.
	//
	// No shell, either: argv goes across as a slice and stays one.
	//
	// gosec's G204 fires on the whole construct, correctly and unavoidably:
	// this function's entire purpose is to execute a path it was handed. What
	// makes it acceptable is not that the input is untainted but that it is
	// bounded — the path arrived over a socket whose peer was checked against
	// this uid, in a 0700 directory, after presenting a nonce, and it has been
	// validated absolute, regular, and executable above. Args go across as a
	// slice and reach no shell.
	//nolint:gosec,noctx // G204: executing a validated, authenticated invocation is this function's job; noctx: the child must outlive this context
	cmd := osexec.Command(inv.Path, inv.Args...)
	cmd.Dir = inv.CWD

	// A nil Env is not "no environment" to os/exec — it means *inherit the
	// parent's*, and the parent here is a process a terminal manager started.
	// So the one case where the invocation carried no environment is exactly
	// the case where the harness would silently receive the manager's instead
	// of the one forgectl built. Verified: with cmd.Env nil the child reads the
	// trampoline's variables; with a non-nil empty slice it reads none.
	//
	// Empty is the honest reading of an empty invocation. Inheriting is never
	// one of the options.
	cmd.Env = inv.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}

	// The harness is an interactive session and this process is standing in for
	// it inside the manager's pane, so it inherits the terminal wholesale.
	// SysProcAttr is left unset on purpose: a new process group would detach
	// the harness from the pane's job control, so Ctrl-C would stop reaching it.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd, nil
}

// startFailureCategory maps a start failure onto the closed wire category.
//
// The real error is not forwarded and this is the reason the field is an enum:
// os/exec's message quotes the harness path, and on some failures the argv too.
// startFailureCategory maps a start failure onto the closed wire category.
//
// Probed against os/exec rather than reasoned about, because the error alone
// cannot carry this distinction:
//
//	directory absent      Op "chdir"      and satisfies os.ErrNotExist
//	directory unsearchable Op "fork/exec"  permission denied
//	directory is a file   Op "fork/exec"  not a directory
//	harness absent        Op "fork/exec"  and satisfies os.ErrNotExist
//	harness not executable Op "fork/exec" permission denied
//
// Two consequences. An errno test placed first claims the absent-directory case
// for FailExec and sends an operator to inspect a binary that is fine. And
// beyond that one case, Go relabels everything the child reports through its
// error pipe as "fork/exec" — including a chdir that failed there — so three of
// the five rows above are indistinguishable by error alone.
//
// So the directory is consulted directly. That is a second look at something
// already checked, and it is only a diagnostic: this runs after the launch has
// failed, and it picks which of two words the outer is told.
func startFailureCategory(err error, cwd string) surface.ExecFailure {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "chdir" {
		return surface.FailChdir
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return surface.FailChdir
	}
	return surface.FailExec
}

// reap waits for the harness and mirrors its exit status.
//
// Mirroring rather than translating is what keeps the trampoline transparent:
// whatever ran the surface sees the exit code the harness produced, not one
// this process invented. A signal death maps to the shell's 128+n convention,
// because that is what a caller reading an exit status expects.
func reap(cmd *osexec.Cmd) error {
	err := cmd.Wait()
	if err == nil {
		return nil
	}

	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("forgectl: wait for harness: %w", termsafe.Error(err))
	}

	code := exitErr.ExitCode()
	if code < 0 {
		// Killed by a signal: ExitCode reports -1 and the number is in the
		// wait status.
		code = signalExitCode(exitErr)
	}
	return WithExitCode(errHarnessExit, code)
}

// productionTrampolineRuntime returns the runtime Execute hands the classifier.
func productionTrampolineRuntime() trampolineRuntime { return productionTrampoline{} }
