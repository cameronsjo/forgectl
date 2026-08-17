package surface_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface"
)

// TestVerifyPeer_AcceptsThisUserOverARealSocket exercises the kernel path
// rather than a fake.
//
// The credential is captured by the kernel at connect time, so a mock would be
// testing the mock. What cannot be tested here is the *refusal* — planting a
// connection from another uid needs a second account and privileges a unit test
// does not have — so this proves the accepting half and
// TestVerifyPeer_RefusesWhatItCannotIdentify proves the guard fails closed
// when it cannot read a credential at all.
func TestVerifyPeer_AcceptsThisUserOverARealSocket(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("peer credentials are unavailable on %s", runtime.GOOS)
	}

	// A short base: macOS caps a socket path near 104 bytes and t.TempDir()
	// embeds the test name.
	dir, err := os.MkdirTemp("", "hs")
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

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	var d net.Dialer
	client, err := d.DialContext(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case server := <-accepted:
		t.Cleanup(func() { _ = server.Close() })
		if err := surface.VerifyPeer(server); err != nil {
			t.Errorf("VerifyPeer refused this user's own connection: %v", err)
		}
		// And the client side of the same pair is equally us, which confirms
		// the check reads the peer rather than something about the listener.
		if err := surface.VerifyPeer(client); err != nil {
			t.Errorf("VerifyPeer refused the client end: %v", err)
		}
	}
}

// TestVerifyPeer_RefusesWhatItCannotIdentify is the fail-closed half. A guard
// that cannot determine the peer has not established that the peer is us, so
// anything other than a Unix socket is refused rather than waved through.
func TestVerifyPeer_RefusesWhatItCannotIdentify(t *testing.T) {
	// A TCP connection carries no peer credential. Loopback only, and closed
	// immediately — this is about the type, not the transport.
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback TCP available: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Skipf("cannot dial loopback: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := surface.VerifyPeer(conn); !errors.Is(err, surface.ErrPeerIdentity) {
		t.Errorf("VerifyPeer on a TCP connection = %v, want ErrPeerIdentity", err)
	}

	// A nil connection must also refuse rather than panic: it is the shape a
	// failed accept produces on a path that forgot to check the error.
	if err := surface.VerifyPeer(nil); !errors.Is(err, surface.ErrPeerIdentity) {
		t.Errorf("VerifyPeer(nil) = %v, want ErrPeerIdentity", err)
	}
}
