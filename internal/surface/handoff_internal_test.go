package surface

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The uid comparison is the whole of same-user exclusion, and until these tests
// existed the suite was indifferent to it: deleting the comparison left every
// other test in the package green. The refusal cannot be provoked honestly — a
// connection from another account needs another account — so the two operands
// are indirected and driven from here.

// unixSocketPair returns a connected pair over a real Unix socket. macOS caps
// sun_path near 104 bytes and t.TempDir() embeds the test name, so the base is
// deliberately short.
func unixSocketPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("peer credentials are unavailable on %s", runtime.GOOS)
	}

	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
		close(accepted)
	}()

	var d net.Dialer
	client, err = d.DialContext(t.Context(), "unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	server = <-accepted
	if server == nil {
		t.Fatal("accept produced no connection")
	}
	t.Cleanup(func() { _ = server.Close() })
	return server, client
}

// TestVerifyPeer_RefusesAnotherAccount is the assertion the whole guard exists
// for. Nothing else in the package goes red when the comparison is removed.
func TestVerifyPeer_RefusesAnotherAccount(t *testing.T) {
	server, _ := unixSocketPair(t)

	// The control first: unstubbed, this connection is ours and is accepted. If
	// it were not, the refusal below would prove nothing — a guard that refuses
	// everything refuses another account too.
	if err := VerifyPeer(server); err != nil {
		t.Fatalf("VerifyPeer refused this user's own connection: %v", err)
	}

	original := peerUIDFn
	t.Cleanup(func() { peerUIDFn = original })
	peerUIDFn = func(*net.UnixConn) (int, error) { return os.Geteuid() + 1, nil }

	if err := VerifyPeer(server); !errors.Is(err, ErrPeerIdentity) {
		t.Errorf("VerifyPeer on a connection from another uid = %v, want ErrPeerIdentity", err)
	}
}

// TestVerifyPeer_ComparesAgainstThisProcess pins the other operand. A guard
// that read the peer correctly and compared it against a constant would pass
// the test above and still admit the wrong account.
func TestVerifyPeer_ComparesAgainstThisProcess(t *testing.T) {
	server, _ := unixSocketPair(t)

	originalSelf := selfUID
	t.Cleanup(func() { selfUID = originalSelf })
	selfUID = func() int { return os.Geteuid() + 1 }

	// The peer is genuinely us; only our own notion of "us" moved. The guard
	// must refuse, which proves it compares against the running process rather
	// than against something baked in.
	if err := VerifyPeer(server); !errors.Is(err, ErrPeerIdentity) {
		t.Errorf("VerifyPeer with a shifted self-uid = %v, want ErrPeerIdentity", err)
	}
}

// TestVerifyPeer_FailsClosedOnAnUnreadableCredential covers the error path. A
// guard that cannot determine the peer has not established that the peer is us.
func TestVerifyPeer_FailsClosedOnAnUnreadableCredential(t *testing.T) {
	server, _ := unixSocketPair(t)

	original := peerUIDFn
	t.Cleanup(func() { peerUIDFn = original })
	peerUIDFn = func(*net.UnixConn) (int, error) {
		return -1, errors.New("credential unavailable")
	}

	if err := VerifyPeer(server); !errors.Is(err, ErrPeerIdentity) {
		t.Errorf("VerifyPeer on an unreadable credential = %v, want ErrPeerIdentity", err)
	}
}
