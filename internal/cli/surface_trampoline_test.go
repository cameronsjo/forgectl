package cli

import (
	"context"
	"errors"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

// TestTrampoline_NeverLetsTheHarnessInheritThisEnvironment is a privacy
// property wearing a correctness bug's clothes.
//
// cmd.Env == nil does not mean "no environment" to os/exec; it means inherit
// the parent's. The parent here is a process a terminal manager started, so an
// invocation that carried no environment would hand the harness the *manager's*
// — the exact copy this whole design exists to avoid making.
func TestTrampoline_NeverLetsTheHarnessInheritThisEnvironment(t *testing.T) {
	requireUnix(t)

	t.Setenv("FORGECTL_TRAMPOLINE_ONLY", "leaked")

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	workdir, err := os.MkdirTemp("", "te")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })

	// Env is nil, which is what slices.Clone of an absent environment yields.
	inv := launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", `printf '%s' "${FORGECTL_TRAMPOLINE_ONLY:-<absent>}" > seen.txt`},
		Env:     nil,
		CWD:     workdir,
	}

	committed := serviceOn(t, listener, nonce, inv)

	if err := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce)); err != nil {
		t.Fatalf("the trampoline failed: %v", err)
	}
	if err := <-committed; err != nil {
		t.Fatalf("the service did not commit: %v", err)
	}

	//nolint:gosec // G304: reading back a file this test just created in its own temp dir
	seen, err := os.ReadFile(filepath.Join(workdir, "seen.txt"))
	if err != nil {
		t.Fatalf("the harness did not run: %v", err)
	}
	if string(seen) != "<absent>" {
		t.Errorf("the harness saw FORGECTL_TRAMPOLINE_ONLY=%q; a nil invocation "+
			"environment leaked the trampoline's own", seen)
	}
}

// TestStartFailureCategory_SeparatesDirectoryFromExec pins the classifier's
// ordering, which is the whole of its correctness.
//
// A missing Cmd.Dir is a *os.PathError with Op "chdir" that ALSO satisfies
// errors.Is(err, os.ErrNotExist), so a generic not-exist test placed first
// reports FailExec and sends the operator to look at the harness binary when
// the directory is what is gone. A Cmd.Dir that is a regular file fails
// differently again — Op "fork/exec" wrapping ENOTDIR — so the Op check alone
// does not cover it either. Both are produced here by os/exec rather than
// hand-built, so the test cannot drift from what the runtime actually returns.
func TestStartFailureCategory_SeparatesDirectoryFromExec(t *testing.T) {
	requireUnix(t)

	dir, err := os.MkdirTemp("", "tc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	regularFile := filepath.Join(dir, "afile")
	if err := os.WriteFile(regularFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		dir  string
		path string
		want surface.ExecFailure
	}{
		"missing working directory": {
			dir: filepath.Join(dir, "absent"), path: "/bin/echo", want: surface.FailChdir,
		},
		"working directory is a regular file": {
			dir: regularFile, path: "/bin/echo", want: surface.FailChdir,
		},
		"harness does not exist": {
			dir: dir, path: filepath.Join(dir, "no-such-harness"), want: surface.FailExec,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			//nolint:gosec // G204: fixed paths, constructed by this test to provoke a real start failure
			cmd := osexec.CommandContext(t.Context(), tc.path)
			cmd.Dir = tc.dir
			startErr := cmd.Start()
			if startErr == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("the fixture started successfully; it provokes no failure to classify")
			}
			if got := startFailureCategory(startErr, tc.dir); got != tc.want {
				t.Errorf("startFailureCategory(%v) = %q, want %q", startErr, got, tc.want)
			}
		})
	}
}

// TestBootstrapRequest_ZeroValueRefusesRatherThanPanics covers the closures.
// parseBootstrap always sets them, but a zero-value request is a legal Go value
// and a nil closure call is a panic where the surrounding code produces
// refusals.
func TestBootstrapRequest_ZeroValueRefusesRatherThanPanics(t *testing.T) {
	var zero bootstrapRequest
	if got := zero.socketPath(); got != "" {
		t.Errorf("socketPath on a zero request = %q, want empty", got)
	}
	if got := zero.nonceValue(); got != "" {
		t.Errorf("nonceValue on a zero request = %q, want empty", got)
	}

	err := (productionTrampoline{}).Run(context.Background(), zero)
	if err == nil {
		t.Fatal("a zero-value bootstrap request ran without error")
	}
	if !errors.Is(err, errSocketUnsafe) {
		t.Errorf("a zero-value request = %v, want errSocketUnsafe", err)
	}
}

// TestTrampoline_AFailedStartDoesNotCommit is the headline property, and until
// this existed nothing asserted it.
//
// Every other failure test refuses inside harnessCommand, which returns before
// cmd.Start is reached — so moving boot.Started() above cmd.Start(), turning the
// exact acknowledgement into an optimistic one, left the whole suite green.
//
// The fixture is a harness that passes every check harnessCommand makes and
// still cannot be started: regular, executable, present, and not an image the
// kernel can exec. That is the only shape that reaches a failing Start.
func TestTrampoline_AFailedStartDoesNotCommit(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	dir, err := os.MkdirTemp("", "tx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	unstartable := filepath.Join(dir, "harness")
	if err := os.WriteFile(unstartable, []byte("\x00\x01not an executable image\n"), 0o755); err != nil { //nolint:gosec // G306: the executable bit is the point
		t.Fatal(err)
	}

	committed := serviceOn(t, listener, nonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: unstartable, Source: launch.BinaryClaudeConfig},
		Args:    []string{},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     dir,
	})

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if runErr == nil {
		t.Fatal("a harness that cannot be exec'd produced no error")
	}
	if !errors.Is(runErr, errHarnessUnstartable) {
		t.Errorf("the trampoline error = %v, want errHarnessUnstartable", runErr)
	}

	select {
	case err := <-committed:
		if err == nil {
			t.Fatal("the service COMMITTED on a harness that never started — " +
				"the acknowledgement is optimistic, not exact")
		}
		if !strings.Contains(err.Error(), string(surface.FailExec)) {
			t.Errorf("the service saw %q, want the %q category", err, surface.FailExec)
		}
		if strings.Contains(err.Error(), unstartable) {
			t.Error("the failure reason carried the harness path across the wire")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service never received a result frame")
	}
}

// TestTrampoline_AnUnusableWorkingDirectoryIsRefusedBeforeStart records where
// the boundary actually falls, which is not where the classifier suggests.
//
// harnessCommand stats the working directory, so a directory that is missing or
// is not a directory never reaches cmd.Start — the outer is told the
// *invocation* was refused, not that chdir failed. FailChdir is therefore
// reachable only when the directory becomes unusable in the window between that
// stat and the exec, a race no test can produce deterministically.
//
// The classifier's mapping is pinned by TestStartFailureCategory_SeparatesDirectoryFromExec,
// which drives it with errors os/exec really returned. This test pins the
// reachable path, so that a future change moving validation around cannot
// silently alter which category an operator sees without one of the two failing.
func TestTrampoline_AnUnusableWorkingDirectoryIsRefusedBeforeStart(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	dir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	notADirectory := filepath.Join(dir, "afile")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	committed := serviceOn(t, listener, nonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", "exit 0"},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     notADirectory,
	})

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if !errors.Is(runErr, errHarnessUnusable) {
		t.Fatalf("the trampoline error = %v, want errHarnessUnusable", runErr)
	}

	select {
	case err := <-committed:
		if err == nil {
			t.Fatal("the service committed on a harness that never started")
		}
		if !strings.Contains(err.Error(), string(surface.FailInvocation)) {
			t.Errorf("the service saw %q, want the %q category", err, surface.FailInvocation)
		}
		if strings.Contains(err.Error(), notADirectory) {
			t.Error("the failure reason carried the working directory across the wire")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the service never received a result frame")
	}
}

// TestTrampoline_MirrorsASignalledHarness covers the 128+n convention. The
// implementation is small and the mistake it guards against — reporting a
// signalled session as a plain failure — is invisible without an assertion.
func TestTrampoline_MirrorsASignalledHarness(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	committed := serviceOn(t, listener, nonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", `kill -TERM $$; sleep 5`},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     "/",
	})

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if runErr == nil {
		t.Fatal("a signalled harness produced no error")
	}
	// SIGTERM is 15, so the shell's convention puts this at 143.
	if got := ExitCode(runErr); got != 143 {
		t.Errorf("exit code = %d, want 143 (128 + SIGTERM)", got)
	}
	if err := <-committed; err != nil {
		t.Errorf("the service did not commit before the harness was signalled: %v", err)
	}
}

// TestTrampoline_SurvivesATerminalSignalAndStillReaps is the Ctrl-C property.
//
// The harness stays in the pane's foreground process group so Ctrl-C reaches
// it, but the trampoline is in that group too — and with no handling installed,
// Go's default disposition kills it mid-Wait. The session is then orphaned on
// the tty and the exit status this file exists to mirror is discarded.
//
// The signal is delivered to this process, which is what a terminal does to the
// whole group. If the handling is removed, this test does not fail politely:
// the test binary itself dies. That is the correct signal — it is exactly what
// happens to the trampoline in a real pane.
func TestTrampoline_SurvivesATerminalSignalAndStillReaps(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	// The harness ignores SIGINT and exits 7 — a claude session traps it and
	// carries on, so the reaper must outlive the signal to see the real status.
	committed := serviceOn(t, listener, nonce, launch.Invocation{
		Harness: "test",
		Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
		Args:    []string{"-c", `trap '' INT; sleep 0.4; exit 7`},
		Env:     []string{"PATH=/usr/bin:/bin"},
		CWD:     "/",
	})

	go func() {
		// Late enough that Run has installed its handling and is in Wait.
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if runErr == nil {
		t.Fatal("the harness exited 7 but the trampoline reported success")
	}
	if got := ExitCode(runErr); got != 7 {
		t.Errorf("exit code = %d, want 7 — the reaper did not survive the signal "+
			"to observe the harness's real status", got)
	}
	if err := <-committed; err != nil {
		t.Errorf("the service did not commit: %v", err)
	}
}

// TestTrampoline_AnUnacknowledgedLaunchIsNotSuccess covers the branch where the
// child started but the acknowledgement could not be delivered.
//
// The outer never commits and will roll the surface back, so a trampoline that
// exited 0 here would report success for a launch that did not happen. The
// harness exits cleanly on purpose: that is the only case where the exit status
// would otherwise say nothing went wrong.
func TestTrampoline_AnUnacknowledgedLaunchIsNotSuccess(t *testing.T) {
	requireUnix(t)

	listener, socket := listenerAt(t)
	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	// Shut down the service's read half BEFORE sending the invocation. The
	// trampoline cannot attempt exec_started until Send gives it the invocation,
	// so this orders the refusal before the acknowledgement instead of racing a
	// full close against bytes already accepted by the socket buffer.
	serviceErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serviceErr <- acceptErr
			return
		}
		handoff, acceptErr := surface.Accept(conn, nonce)
		if acceptErr != nil {
			serviceErr <- acceptErr
			return
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			serviceErr <- errors.New("accepted Unix listener connection is not *net.UnixConn")
			return
		}
		if closeErr := unixConn.CloseRead(); closeErr != nil {
			serviceErr <- closeErr
			return
		}
		if sendErr := handoff.Send(launch.Invocation{
			Harness: "test",
			Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
			Args:    []string{"-c", "exit 0"},
			Env:     []string{"PATH=/usr/bin:/bin"},
			CWD:     "/",
		}); sendErr != nil {
			serviceErr <- sendErr
			return
		}
		serviceErr <- handoff.Close()
	}()

	runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
	if err := <-serviceErr; err != nil {
		t.Fatalf("fake service setup: %v", err)
	}
	if runErr == nil {
		t.Fatal("an unacknowledged launch reported success; the outer is rolling it back")
	}
	if !errors.Is(runErr, errUnacknowledged) {
		t.Errorf("the trampoline error = %v, want errUnacknowledged", runErr)
	}
}

// TestTrampoline_ErrorsNeverNameTheInvocation guards the local channel.
//
// The wire reason is a closed enum, which is well covered — but this process's
// stderr is the manager's pane, and an error naming the harness path hands over
// by message exactly what the socket withheld from argv. Every refusal is a
// bare sentinel for that reason, and nothing else asserts it.
func TestTrampoline_ErrorsNeverNameTheInvocation(t *testing.T) {
	requireUnix(t)

	dir, err := os.MkdirTemp("", "tp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Distinctive enough that a substring check cannot pass by accident.
	secretHarness := filepath.Join(dir, "harness-SENTINEL-9f3a")
	secretCWD := filepath.Join(dir, "cwd-SENTINEL-9f3a")

	cases := map[string]struct {
		path, cwd string
		setup     func(t *testing.T)
	}{
		"harness is not executable": {
			path: secretHarness, cwd: dir,
			setup: func(t *testing.T) {
				t.Helper()
				if err := os.WriteFile(secretHarness, []byte("#!/bin/sh\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		"harness cannot be exec'd": {
			path: secretHarness, cwd: dir,
			setup: func(t *testing.T) {
				t.Helper()
				//nolint:gosec // G306: the executable bit is what carries this case past validation
				if err := os.WriteFile(secretHarness, []byte("\x00\x01"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		"working directory is missing": {
			path: "/bin/sh", cwd: secretCWD,
			setup: func(*testing.T) {},
		},
		// Reaches the stat-failed branch rather than the shape checks — a
		// different return, and one an os error would happily annotate with the
		// path it could not find.
		"harness is missing": {
			path: secretHarness, cwd: dir,
			setup: func(*testing.T) {},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(secretHarness)
			tc.setup(t)

			listener, socket := listenerAt(t)
			nonce, err := surface.NewNonce()
			if err != nil {
				t.Fatalf("NewNonce: %v", err)
			}
			committed := serviceOn(t, listener, nonce, launch.Invocation{
				Harness: "test",
				Binary:  launch.ResolvedBinary{Path: tc.path, Source: launch.BinaryClaudeConfig},
				Args:    []string{"--prompt", "SENTINEL-9f3a-argument"},
				Env:     []string{"SECRET=SENTINEL-9f3a-env"},
				CWD:     tc.cwd,
			})

			runErr := (productionTrampoline{}).Run(context.Background(), bootstrapFor(t, socket, nonce))
			if runErr == nil {
				t.Fatal("the fixture produced no error, so there is nothing to inspect")
			}
			if strings.Contains(runErr.Error(), "SENTINEL-9f3a") {
				t.Errorf("the trampoline's error reaches the manager's pane and names the "+
					"invocation: %v", runErr)
			}
			<-committed
		})
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
