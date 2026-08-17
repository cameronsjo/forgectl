package surface

import (
	"errors"
	"fmt"
	"net"
	"os"
)

var (
	// ErrPeerIdentity reports a connection from another user, or one whose
	// credentials could not be read.
	ErrPeerIdentity = errors.New("surface: bootstrap peer is not this user")

	// ErrPeerUnsupported reports a platform with no peer-credential mechanism.
	ErrPeerUnsupported = errors.New("surface: peer credentials are unavailable on this platform")
)

// VerifyPeer refuses a connection that does not come from this user.
//
// This is the check that actually carries same-user exclusion. The nonce does
// not: a process running as this user can read our argv and our environment, so
// it can learn the nonce, and the threat model says a hostile same-UID process
// is out of scope precisely because nothing at this layer can exclude it. What
// the kernel *can* tell us is the uid on the other end of the socket, and that
// is what keeps another account off the invocation.
//
// The socket's 0700 parent directory already makes this hard to reach, so this
// is the second of two independent barriers rather than the only one. Both are
// kept: the directory check happens at creation and this one at accept, and a
// filesystem misconfiguration that defeats the first — see RunDir's base check
// — leaves this one standing.
//
// It fails closed on an unreadable credential. A guard that cannot determine
// the peer has not established that the peer is us.
func VerifyPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%w: connection is not a Unix socket", ErrPeerIdentity)
	}

	uid, err := peerUID(unixConn)
	if err != nil {
		return err
	}
	if uid != os.Geteuid() {
		// The uid is not named in the message. It is not a secret, but a
		// refusal that reports which account tried is a refusal that helps an
		// attacker confirm they reached us.
		return fmt.Errorf("%w: connection came from a different account", ErrPeerIdentity)
	}
	return nil
}
