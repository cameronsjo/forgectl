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
	//
	// It wraps ErrPeerIdentity so the general refusal predicate covers it. A
	// consumer writing `if errors.Is(err, ErrPeerIdentity) { refuse }` against
	// two flat sentinels would fall through on exactly the case that must never
	// proceed: the platform where the identity check does not exist at all.
	ErrPeerUnsupported = fmt.Errorf("%w: peer credentials are unavailable on this platform", ErrPeerIdentity)
)

// Both operands of the uid comparison are indirected so a test can drive them.
//
// This is not decoration. The comparison below is the entire mechanism of
// same-user exclusion, and its refusal path cannot be reached from a unit test
// by honest means: planting a connection from another account needs a second
// account and privileges the test process does not have. Without a seam, the
// suite is *indifferent* to whether the comparison exists — deleting it leaves
// every test green, which is the worst possible state for the one control the
// package's own documentation calls load-bearing.
//
// They follow the closeFD convention already used in rundir_unix.go.
var (
	peerUIDFn = peerUID
	selfUID   = os.Geteuid
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
	// The nil check is not redundant with the assertion. A net.Conn interface
	// can hold a *typed* nil *net.UnixConn, which the assertion accepts — and
	// the very next call dereferences it and panics. Both spellings of "no
	// connection" have to land on the same refusal.
	unixConn, ok := conn.(*net.UnixConn)
	if !ok || unixConn == nil {
		return fmt.Errorf("%w: connection is not a Unix socket", ErrPeerIdentity)
	}

	uid, err := peerUIDFn(unixConn)
	if err != nil {
		// The sentinel is attached here rather than trusted from each platform
		// implementation. Three files returning it is three chances to forget,
		// and the consequence of forgetting is silent: the error still says
		// something went wrong, but a caller matching on ErrPeerIdentity — the
		// documented fail-closed contract — stops seeing it. The guarantee is
		// this function's, so the classification is too.
		if !errors.Is(err, ErrPeerIdentity) {
			return fmt.Errorf("%w: %w", ErrPeerIdentity, err)
		}
		return err
	}
	if uid != selfUID() {
		// The uid is not named in the message. It is not a secret, but a
		// refusal that reports which account tried is a refusal that helps an
		// attacker confirm they reached us.
		return fmt.Errorf("%w: connection came from a different account", ErrPeerIdentity)
	}
	return nil
}
