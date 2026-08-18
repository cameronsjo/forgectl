package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// These run the trampoline against a real service on a real Unix socket, and
// the harness it starts is a real process.
//
// Nothing smaller would test the property that matters. The acknowledgement is
// exact — the outer commits only when the child has crossed the fork/exec
// boundary — and a fake cannot cross it. Two of the tests below assert on what
// a *started* process did, and one asserts on a process that could never start.

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("the surface trampoline is Unix-only; this is %s", runtime.GOOS)
	}
}

// bootstrapFor builds the request the classifier would have produced.
func bootstrapFor(t *testing.T, socket string, nonce surface.Nonce) bootstrapRequest {
	t.Helper()
	raw := nonce.String()
	return bootstrapRequest{
		revealSocket: func() string { return socket },
		revealNonce:  func() string { return raw },
	}
}

// serviceOn accepts one bootstrap connection and plays the outer half.
//
// It returns a channel carrying the commit verdict: nil means an exec_started
// frame arrived and the launch committed.
func serviceOn(t *testing.T, listener net.Listener, nonce surface.Nonce, inv launch.Invocation) <-chan error {
	t.Helper()
	committed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			committed <- err
			return
		}
		handoff, err := surface.Accept(conn, nonce)
		if err != nil {
			committed <- err
			return
		}
		defer func() { _ = handoff.Close() }()
		if err := handoff.Send(inv); err != nil {
			committed <- err
			return
		}
		committed <- handoff.AwaitStart()
	}()
	return committed
}

// listenerAt binds a socket under a short base — macOS caps sun_path near 104
// bytes and t.TempDir() embeds the test name.
func listenerAt(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, socket
}

// TestTrampoline_StartsTheHarnessAndCommits is the whole flow: dial,
// handshake, receive, exec, acknowledge, reap.
func TestTrampoline_StartsTheHarnessAndCommits(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	// The harness writes a file, so a passing test proves a real process ran
	// with the directory and environment the invocation carried — not merely
	// that Start returned nil.
	workdir, err := os.MkdirTemp("", "tw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })

	inv := launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", `printf '%s' "$FORGECTL_PROBE" > proof.txt`},
		Env:     []string{"FORGECTL_PROBE=delivered"},
		CWD:     workdir,
	}

	committed := serviceOn(t, listener, nonce, inv)

	if err := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce)); err != nil {
		t.Fatalf("the trampoline failed: %v", err)
	}

	select {
	case err := <-committed:
		if err != nil {
			t.Fatalf("the service did not commit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service never saw an exec_started frame")
	}

	// cmd.Dir was honoured — the file is relative to the invocation's cwd.
	//nolint:gosec // G304: reading back a file this test just created in its own temp dir
	proof, err := os.ReadFile(filepath.Join(workdir, "proof.txt"))
	if err != nil {
		t.Fatalf("the harness did not run in the invocation's directory: %v", err)
	}
	// cmd.Env was honoured, and exactly: the variable came from the invocation.
	if string(proof) != "delivered" {
		t.Errorf("the harness saw %q, want the invocation's environment", proof)
	}
}

// TestTrampoline_MirrorsTheHarnessExitStatus keeps the trampoline transparent.
// Whatever ran the surface must see the code the harness produced, not one this
// process invented.
func TestTrampoline_MirrorsTheHarnessExitStatus(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	inv := launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", "exit 42"},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     "/",
	}

	committed := serviceOn(t, listener, nonce, inv)

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if runErr == nil {
		t.Fatal("a harness exiting 42 produced no error")
	}
	if got := ExitCode(runErr); got != 42 {
		t.Errorf("exit code = %d, want 42", got)
	}

	// The launch still committed. A non-zero harness exit is the harness's
	// business; the surface was created and handed over regardless.
	select {
	case err := <-committed:
		if err != nil {
			t.Errorf("the service did not commit before the harness exited: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service never saw an exec_started frame")
	}
}

// TestTrampoline_ReportsAFailedExecWithoutCommitting is the fail-closed half.
// A harness that cannot start must leave the outer able to roll back, and the
// reason it sends must be a category rather than the operating system's error —
// which quotes the path.
func TestTrampoline_ReportsAFailedExecWithoutCommitting(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	// A real, existing, non-executable file: it passes the wire validator's
	// absoluteness check and fails the trampoline's own usability check.
	dir, err := os.MkdirTemp("", "tn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	notExecutable := filepath.Join(dir, "harness")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inv := launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: notExecutable, Source: launch.BinaryClaudeConfig},
		Args:    []string{},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     "/",
	}

	committed := serviceOn(t, listener, nonce, inv)

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if !errors.Is(runErr, errHarnessUnusable) {
		t.Fatalf("the trampoline error = %v, want errHarnessUnusable", runErr)
	}

	select {
	case err := <-committed:
		if err == nil {
			t.Fatal("the service committed on a harness that never started")
		}
		// The category crossed, and the operating system's message did not.
		if !strings.Contains(err.Error(), string(surface.FailInvocation)) {
			t.Errorf("the service saw %q, want the %q category", err, surface.FailInvocation)
		}
		if strings.Contains(err.Error(), notExecutable) {
			t.Error("the failure reason carried the harness path across the wire")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service never received a result frame")
	}
}

// TestTrampoline_RefusesASocketItDoesNotOwn is the check with no counterpart on
// the service side: the trampoline holds only a path a terminal manager typed,
// so it must establish that what is there is forgectl's socket.
func TestTrampoline_RefusesASocketItDoesNotOwn(t *testing.T) {
	requireUnix(t)

	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	// A regular file where a socket should be. Ownership is ours, so this is
	// the type check specifically — the case where something was substituted
	// for the socket rather than planted by another account.
	regular := filepath.Join(dir, "notasocket")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, regular, nonce))
	if !errors.Is(err, errSocketUnsafe) {
		t.Errorf("a regular file at the socket path = %v, want errSocketUnsafe", err)
	}

	// An absent path refuses too, rather than falling through to a dial whose
	// error would name it.
	err = (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, filepath.Join(dir, "absent"), nonce))
	if !errors.Is(err, errSocketUnsafe) {
		t.Errorf("an absent socket = %v, want errSocketUnsafe", err)
	}

	// The control: a real socket at a real path passes this check and fails
	// later, at the handshake. Without it the two refusals above would be
	// consistent with a guard that refuses everything.
	listener, socket := listenerAt(t)
	_ = listener
	if err := checkSocketOwner(socket); err != nil {
		t.Errorf("checkSocketOwner refused forgectl's own socket: %v", err)
	}
}

// TestTrampoline_RefusesAServiceWithTheWrongNonce covers the rendezvous from
// this side. A stale trampoline from a previous launch reaching a live socket
// must fail closed rather than receive an invocation meant for someone else.
func TestTrampoline_RefusesAServiceWithTheWrongNonce(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	serviceNonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	staleNonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	committed := serviceOn(t, listener, serviceNonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", "exit 0"},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     "/",
	})

	if err := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, staleNonce)); err == nil {
		t.Fatal("the trampoline accepted an invocation from a service it could not authenticate to")
	}

	select {
	case err := <-committed:
		if !errors.Is(err, surface.ErrNonceMismatch) {
			t.Errorf("the service saw %v, want ErrNonceMismatch", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service never refused the connection")
	}
}
