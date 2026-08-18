package surface_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// These exercise the two ends against each other over a real Unix socket.
//
// A fake would not do: Accept and Dial both run the peer-credential check, and
// that credential is a kernel property of an actual socket. Running both halves
// also means the wire format is tested by the only thing that has to agree with
// it — the other end.

func socketPair(t *testing.T) (listener net.Listener, addr string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("peer credentials are unavailable on %s", runtime.GOOS)
	}

	// macOS caps sun_path near 104 bytes and t.TempDir() embeds the test name.
	dir, err := os.MkdirTemp("", "hx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	addr = filepath.Join(dir, "s")
	var lc net.ListenConfig
	listener, err = lc.Listen(t.Context(), "unix", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, addr
}

func sampleInvocation() launch.Invocation {
	return launch.Invocation{
		Harness: "claude",
		Binary: launch.ResolvedBinary{
			Path:   "/opt/homebrew/bin/claude",
			Source: launch.BinaryClaudeConfig,
		},
		Args: []string{"--model", "opus", "write a haiku about sockets"},
		Env:  []string{"PATH=/usr/bin", "HOME=/Users/x"},
		CWD:  "/Users/x/Projects/thing",
	}
}

// TestBootstrapExchange_DeliversTheInvocationAndCommits is the happy path, and
// it is the only place the whole protocol is proven to agree with itself.
func TestBootstrapExchange_DeliversTheInvocationAndCommits(t *testing.T) {
	listener, addr := socketPair(t)

	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	inv := sampleInvocation()

	type outcome struct {
		received surface.Invocation
		err      error
	}
	trampoline := make(chan outcome, 1)
	go func() {
		var d net.Dialer
		conn, dialErr := d.DialContext(t.Context(), "unix", addr)
		if dialErr != nil {
			trampoline <- outcome{err: dialErr}
			return
		}
		boot, received, bootErr := surface.Dial(conn, nonce)
		if bootErr != nil {
			trampoline <- outcome{err: bootErr}
			return
		}
		// The child would be started here; reporting Started without one is the
		// point — this test is the protocol, not the process management.
		trampoline <- outcome{received: received, err: boot.Started()}
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	handoff, err := surface.Accept(conn, nonce)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	t.Cleanup(func() { _ = handoff.Close() })

	if err := handoff.Send(inv); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := handoff.AwaitStart(); err != nil {
		t.Fatalf("AwaitStart: %v", err)
	}

	got := <-trampoline
	if got.err != nil {
		t.Fatalf("the trampoline end failed: %v", got.err)
	}
	if got.received.Path != inv.Binary.Path {
		t.Errorf("path = %q, want %q", got.received.Path, inv.Binary.Path)
	}
	if !slices.Equal(got.received.Args, inv.Args) {
		t.Errorf("args = %q, want %q", got.received.Args, inv.Args)
	}
	if !slices.Equal(got.received.Env, inv.Env) {
		t.Errorf("env = %q, want %q", got.received.Env, inv.Env)
	}
	if got.received.CWD != inv.CWD {
		t.Errorf("cwd = %q, want %q", got.received.CWD, inv.CWD)
	}
}

// TestAccept_RefusesTheWrongNonce is the rendezvous half. Without it the
// exchange above would pass against an Accept that never compared anything.
func TestAccept_RefusesTheWrongNonce(t *testing.T) {
	listener, addr := socketPair(t)

	expected, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	other, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	go func() {
		var d net.Dialer
		conn, dialErr := d.DialContext(t.Context(), "unix", addr)
		if dialErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// A peer that knows the protocol but not the nonce — the stale
		// trampoline from a previous launch, or a second forgectl racing.
		_, _, _ = surface.Dial(conn, other)
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := surface.Accept(conn, expected); !errors.Is(err, surface.ErrNonceMismatch) {
		t.Errorf("Accept with the wrong nonce = %v, want ErrNonceMismatch", err)
	}
}

// TestAccept_RefusesAnUngeneratedNonce covers the zero-value service. It is the
// same fail-open Nonce.Equal guards against, one layer up where the mistake is
// likelier: a struct field nobody filled in.
func TestAccept_RefusesAnUngeneratedNonce(t *testing.T) {
	listener, addr := socketPair(t)

	go func() {
		var d net.Dialer
		conn, dialErr := d.DialContext(t.Context(), "unix", addr)
		if dialErr == nil {
			_ = conn.Close()
		}
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var unset surface.Nonce
	if _, err := surface.Accept(conn, unset); !errors.Is(err, surface.ErrNonceMismatch) {
		t.Errorf("Accept with an ungenerated nonce = %v, want ErrNonceMismatch", err)
	}
}

// TestAccept_HangsUpOnARefusedPeer is a latency property, and it reads as a
// slow test rather than a bug when it regresses.
//
// A refused peer that is left connected blocks on its own read until the
// handshake deadline expires — thirty seconds of a launch already decided
// against. It cannot close the connection itself, because from its side nothing
// has happened yet. Caught when this exact case took 30.00s to pass.
func TestAccept_HangsUpOnARefusedPeer(t *testing.T) {
	listener, addr := socketPair(t)

	expected, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	other, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		var d net.Dialer
		conn, dialErr := d.DialContext(t.Context(), "unix", addr)
		if dialErr != nil {
			return
		}
		// Dial blocks reading the invocation that will never come. It returns
		// only because the service hung up.
		_, _, _ = surface.Dial(conn, other)
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := surface.Accept(conn, expected); !errors.Is(err, surface.ErrNonceMismatch) {
		t.Fatalf("Accept with the wrong nonce = %v, want ErrNonceMismatch", err)
	}

	select {
	case <-peerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("a refused peer was left connected; it will block until the handshake " +
			"deadline instead of being hung up on")
	}
}

// TestDial_HangsUpWhenItRefuses is the mirror of the Accept case above.
//
// Same change, same PR, and the same failure: a service left waiting on a
// trampoline that has already decided against it blocks until its own deadline.
// The two ends got one test between them.
func TestDial_HangsUpWhenItRefuses(t *testing.T) {
	listener, addr := socketPair(t)

	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	serviceDone := make(chan struct{})
	go func() {
		defer close(serviceDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		handoff, acceptErr := surface.Accept(conn, nonce)
		if acceptErr != nil {
			return
		}
		defer func() { _ = handoff.Close() }()
		if handoff.Send(sampleInvocation()) != nil {
			return
		}
		// Blocks reading a result the trampoline will never send. It returns
		// only because the trampoline hung up.
		_ = handoff.AwaitStart()
	}()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// A zero-value nonce is refused before anything is written, which is the
	// earliest refusal Dial has — so the connection is closed on the path with
	// the least chance of an incidental close.
	var unset surface.Nonce
	if _, _, err := surface.Dial(conn, unset); !errors.Is(err, surface.ErrInvalidNonce) {
		t.Fatalf("Dial with an unset nonce = %v, want ErrInvalidNonce", err)
	}

	select {
	case <-serviceDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the service was left connected after the trampoline refused; it will " +
			"block until the handshake deadline instead of being hung up on")
	}
}

// TestAwaitStart_DoesNotCommitOnAFailedExec keeps the commit signal exact.
// Receipt of the invocation is not commit, and neither is a connection that
// stayed open — only a complete exec_started frame is.
func TestAwaitStart_DoesNotCommitOnAFailedExec(t *testing.T) {
	listener, addr := socketPair(t)

	nonce, err := surface.NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}

	go func() {
		var d net.Dialer
		conn, dialErr := d.DialContext(t.Context(), "unix", addr)
		if dialErr != nil {
			return
		}
		boot, _, bootErr := surface.Dial(conn, nonce)
		if bootErr != nil {
			return
		}
		_ = boot.Failed(surface.FailChdir)
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	handoff, err := surface.Accept(conn, nonce)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	t.Cleanup(func() { _ = handoff.Close() })

	if err := handoff.Send(sampleInvocation()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := handoff.AwaitStart(); err == nil {
		t.Error("AwaitStart committed on a failed exec")
	}
}

// TestInvocationMapping_CarriesEveryField is a build-time-ish guard against a
// silent drop.
//
// launch.Invocation is a struct someone will add a field to. The mapping onto
// the wire lives in one function, and a field added there but not carried is
// invisible: the child simply runs with less than it was configured with, and
// nothing errors. This fails when that happens, and the fix is either to carry
// the field or to name it below as deliberately outer-only.
func TestInvocationMapping_CarriesEveryField(t *testing.T) {
	// Fields the trampoline deliberately does not receive. Harness and the
	// binary's provenance inform policy decisions the outer process already
	// made before anything reached the socket; sending them would put facts on
	// the wire the receiving end has no use for.
	intentionallyNotSent := map[string]string{
		"Harness": "policy input, consumed before the handshake",
		"Binary":  "only Binary.Path crosses; Source is provenance the outer already acted on",
	}
	carried := map[string]bool{"Args": true, "Env": true, "CWD": true}

	typ := reflect.TypeOf(launch.Invocation{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if carried[name] {
			continue
		}
		if _, ok := intentionallyNotSent[name]; ok {
			continue
		}
		t.Errorf("launch.Invocation.%s is neither carried onto the wire nor listed as "+
			"intentionally outer-only — a new field is being silently dropped", name)
	}
}
