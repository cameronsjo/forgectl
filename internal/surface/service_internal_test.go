package surface

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/fake"
)

// The commit path, which needs the socket and the nonce.
//
// Nothing outside this package can reach either — they travel to the trampoline
// inside an opaque bootstrap, and the external tests assert that. So these
// live in-package and take them through the observeBootstrap seam.

func internalFixture(t *testing.T) (*fake.Adapter, *Service) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("the surface service is Unix-only; this is %s", runtime.GOOS)
	}
	base, err := os.MkdirTemp("", "si")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	adapter := fake.New(backend.KindTmux)
	return adapter, NewService(adapter, Policy{}, base)
}

func internalRequest(t *testing.T) LaunchRequest {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return LaunchRequest{
		Name: "thing",
		Invocation: launch.Invocation{
			Harness: "claude",
			Binary:  launch.ResolvedBinary{Path: "/bin/sh", Source: launch.BinaryClaudeConfig},
			Args:    []string{"--model", "opus"},
			Env:     []string{"PATH=/usr/bin"},
			CWD:     t.TempDir(),
		},
		Self: self,
	}
}

func internalRef(t *testing.T, tag backend.RecoveryTag) backend.Ref {
	t.Helper()
	source := backend.TmuxDefaultServer()
	server, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/tmp/tmux-501/default",
		Version:  "3.5a",
		Inode:    42,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := backend.NewTmuxIdentity(tag.OwnershipName())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := backend.NewTmuxRef(source, server, tag, identity)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// observe installs the seam for one test and restores it afterwards.
func observe(t *testing.T) <-chan struct {
	socket string
	nonce  Nonce
} {
	t.Helper()
	seen := make(chan struct {
		socket string
		nonce  Nonce
	}, 1)

	original := observeBootstrap
	t.Cleanup(func() { observeBootstrap = original })
	observeBootstrap = func(socket string, nonce Nonce) {
		seen <- struct {
			socket string
			nonce  Nonce
		}{socket, nonce}
	}
	return seen
}

// TestService_CommitsAndDoesNotCloseTheSurface is the success path, and the
// assertion that matters is the negative one: nothing was closed.
//
// It also proves the socket is live before the adapter is called — the
// trampoline dials from inside StartFunc, which is where a real manager would
// type the command. If the service bound the socket after building the
// bootstrap, this dial would fail.
func TestService_CommitsAndDoesNotCloseTheSurface(t *testing.T) {
	adapter, service := internalFixture(t)
	seen := observe(t)

	var created backend.Ref
	trampolineErr := make(chan error, 1)

	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		created = internalRef(t, spec.Tag())
		bootstrap := <-seen
		go func() {
			trampolineErr <- runTrampoline(t, bootstrap.socket, bootstrap.nonce,
				func(b *Bootstrap) error { return b.Started() })
		}()
		return backend.NewRefKnown(created)
	}

	result, err := service.Launch(context.Background(), internalRequest(t))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.Ref() != created {
		t.Error("the committed result names a different surface than the adapter created")
	}
	if err := <-trampolineErr; err != nil {
		t.Errorf("the trampoline end failed: %v", err)
	}
	if closes := adapter.Closes(); len(closes) != 0 {
		t.Errorf("a committed launch closed the surface %d time(s); ownership had already moved", len(closes))
	}
}

// TestService_DoesNotCommitWhenTheTrampolineReportsAFailedExec keeps the commit
// signal exact from this side. A trampoline that received the invocation and
// could not start the harness must leave the launch rolled back.
func TestService_DoesNotCommitWhenTheTrampolineReportsAFailedExec(t *testing.T) {
	adapter, service := internalFixture(t)
	seen := observe(t)

	var created backend.Ref
	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		created = internalRef(t, spec.Tag())
		bootstrap := <-seen
		go func() {
			_ = runTrampoline(t, bootstrap.socket, bootstrap.nonce,
				func(b *Bootstrap) error { return b.Failed(FailExec) })
		}()
		return backend.NewRefKnown(created)
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		return backend.NewCloseClosed()
	}

	_, err := service.Launch(context.Background(), internalRequest(t))
	if err == nil {
		t.Fatal("the service committed on a harness that never started")
	}

	var launchErr *LaunchError
	if !errors.As(err, &launchErr) {
		t.Fatalf("err = %T, want *LaunchError", err)
	}
	if launchErr.Phase != PhaseCommit {
		t.Errorf("phase = %v, want PhaseCommit", launchErr.Phase)
	}
	closes := adapter.Closes()
	if len(closes) != 1 || closes[0] != created {
		t.Errorf("the surface was not rolled back: %d close(s)", len(closes))
	}
}

// TestService_RefusesASecondConnection is the nonce's single-use property.
//
// The service accepts exactly one connection, so a second peer presenting the
// same nonce — a replay, or a stale trampoline from an earlier launch — finds
// nothing listening. An accept loop would make the nonce re-presentable.
func TestService_RefusesASecondConnection(t *testing.T) {
	adapter, service := internalFixture(t)
	seen := observe(t)

	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		bootstrap := <-seen
		go func() {
			_ = runTrampoline(t, bootstrap.socket, bootstrap.nonce,
				func(b *Bootstrap) error { return b.Started() })
		}()
		// Hold the socket path so the assertion below runs after the launch.
		t.Cleanup(func() {
			conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", bootstrap.socket)
			if err == nil {
				_ = conn.Close()
				t.Error("a second connection was accepted after the launch committed; " +
					"the nonce is re-presentable")
			}
		})
		return backend.NewRefKnown(internalRef(t, spec.Tag()))
	}

	if _, err := service.Launch(context.Background(), internalRequest(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}

// TestService_CancellationUnblocksAnAuthenticatedStall is the cancellation
// property past the point where a context stops being consulted.
//
// Handoff.Send and AwaitStart take no context — they are bounded by the
// handshake deadline alone — so a peer that authenticates and then goes quiet
// would hold the launch for that whole budget after the caller has given up.
// Closing the handoff is what unblocks them.
func TestService_CancellationUnblocksAnAuthenticatedStall(t *testing.T) {
	adapter, service := internalFixture(t)
	seen := observe(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		bootstrap := <-seen
		//nolint:gosec // G118: this goroutine is the peer, not the request; it must outlive the cancelled ctx to keep the stall alive
		go func() {
			// Authenticate, then stall: never read the invocation, never report.
			conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", bootstrap.socket)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			ex, err := newExchange(conn, HandshakeTimeout)
			if err != nil {
				return
			}
			_ = ex.write(helloFrame{
				Kind: kindHello, Version: ProtocolVersion, Nonce: bootstrap.nonce.String(),
			})
			// And nothing further. The caller cancels below.
			<-ctx.Done()
		}()
		go func() {
			time.Sleep(150 * time.Millisecond)
			cancel()
		}()
		return backend.NewRefKnown(internalRef(t, spec.Tag()))
	}
	adapter.CloseFunc = func(context.Context, backend.Ref) backend.CloseResult {
		return backend.NewCloseClosed()
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.Launch(ctx, internalRequest(t))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled launch reported success")
		}
	case <-time.After(HandshakeTimeout / 2):
		t.Fatal("cancellation did not unblock the authenticated exchange; the launch " +
			"waited on the handshake deadline instead")
	}

	// The surface was still rolled back — cancellation is not a reason to
	// leave a container behind.
	if closes := adapter.Closes(); len(closes) != 1 {
		t.Errorf("a cancelled launch closed the surface %d time(s), want 1", len(closes))
	}
}

// TestService_StopsListeningAtTheFirstPeer pins when the socket stops being an
// entry point.
//
// Only one connection is ever served, so a later dial would learn nothing
// either way — but "sits unanswered in the backlog" is a weaker guarantee than
// "refused", and it depends on nobody adding a second Accept later. This
// asserts the stronger one, during the commit window rather than after it.
func TestService_StopsListeningAtTheFirstPeer(t *testing.T) {
	adapter, service := internalFixture(t)
	seen := observe(t)

	secondDial := make(chan error, 1)

	adapter.StartFunc = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		bootstrap := <-seen
		//nolint:gosec // G118: this goroutine plays the peer, not the request — it deliberately does not share the launch's context
		go func() {
			conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", bootstrap.socket)
			if err != nil {
				secondDial <- err
				return
			}
			boot, _, err := Dial(conn, bootstrap.nonce)
			if err != nil {
				secondDial <- err
				return
			}
			defer func() { _ = boot.Close() }()

			// A second peer, attempted while the first still holds the
			// exchange — before Started, so squarely inside the window.
			second, dialErr := (&net.Dialer{}).DialContext(context.Background(), "unix", bootstrap.socket)
			if dialErr == nil {
				_ = second.Close()
			}
			secondDial <- dialErr

			_ = boot.Started()
		}()
		return backend.NewRefKnown(internalRef(t, spec.Tag()))
	}

	if _, err := service.Launch(context.Background(), internalRequest(t)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	select {
	case err := <-secondDial:
		if err == nil {
			t.Error("a second connection was accepted while the first exchange was still " +
				"in flight; the socket outlives its one peer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the trampoline goroutine never reported")
	}
}

// runTrampoline plays the inner half of the exchange.
func runTrampoline(t *testing.T, socket string, nonce Nonce, report func(*Bootstrap) error) error {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", socket)
	if err != nil {
		return err
	}
	boot, _, err := Dial(conn, nonce)
	if err != nil {
		return err
	}
	defer func() { _ = boot.Close() }()
	return report(boot)
}
