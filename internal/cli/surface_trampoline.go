package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
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
// The invocation never appears in this process's argv. That is the entire point
// of the socket, and it means the trampoline must not print, log, or forward
// what it receives — its diagnostics are categories, and the logger the
// classifier hands it discards by default.
//
// And acknowledgement is exact. The outer process commits — stops being able to
// roll the surface back — only when it reads an exec_started frame, so this
// sends one only after the operating system reports that the child's directory
// change and exec both succeeded. Receiving the invocation is not commit;
// building the command is not commit; a live socket is not commit.
//
// After acknowledgement it is a transparent reaper and nothing more: it waits
// for the harness, mirrors its exit status, and performs no restart, health
// polling, or supervision of any kind.

var (
	// errSocketUnsafe reports a bootstrap socket this process will not dial.
	errSocketUnsafe = errors.New("forgectl: surface bootstrap socket is not safe to dial")

	// errHarnessUnusable reports a received invocation the trampoline refuses.
	errHarnessUnusable = errors.New("forgectl: surface invocation is not runnable")

	// errHarnessExit carries the child's failure without describing it. The
	// harness has already written whatever it wanted to the inherited stderr;
	// re-reporting it here would be a second, worse copy.
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
		return fmt.Errorf("forgectl: dial surface bootstrap socket: %w", termsafe.Error(err))
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

	if err := cmd.Start(); err != nil {
		_ = boot.Failed(startFailureCategory(err))
		return fmt.Errorf("forgectl: start harness: %w", termsafe.Error(err))
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
		return reap(cmd)
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
func harnessCommand(inv surface.Invocation) (*osexec.Cmd, error) {
	info, err := os.Stat(inv.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errHarnessUnusable, termsafe.Error(err))
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file",
			errHarnessUnusable, termsafe.QuotePath(inv.Path))
	}
	if info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("%w: %s is not executable",
			errHarnessUnusable, termsafe.QuotePath(inv.Path))
	}

	dir, err := os.Stat(inv.CWD)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errHarnessUnusable, termsafe.Error(err))
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory",
			errHarnessUnusable, termsafe.QuotePath(inv.CWD))
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
// The directory cases are tested first, and the order is the whole correctness
// of this function. A missing Cmd.Dir surfaces as a *os.PathError with Op
// "chdir" that ALSO satisfies errors.Is(err, os.ErrNotExist), so a generic
// not-exist test placed first swallows it and reports FailExec — the operator
// then goes looking at the harness binary when the directory is what is gone.
//
// The Op check alone is not enough either: a Cmd.Dir that exists but is a
// regular file fails as Op "fork/exec" wrapping ENOTDIR, with ErrNotExist
// false. Both were confirmed against os/exec rather than assumed.
func startFailureCategory(err error) surface.ExecFailure {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "chdir" {
		return surface.FailChdir
	}
	if errors.Is(err, syscall.ENOTDIR) {
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
