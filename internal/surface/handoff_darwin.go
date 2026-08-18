//go:build darwin

package surface

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the connected peer's effective uid via LOCAL_PEERCRED.
//
// Darwin has no SO_PEERCRED. The equivalent is getsockopt(LOCAL_PEERCRED) at
// the SOL_LOCAL level, which fills an xucred; x/sys/unix exposes it as
// GetsockoptXucred. Like Linux's, the value is captured by the kernel rather
// than sent by the peer, which is the property that makes it worth checking.
//
// The xucred's uid field — cr_uid in the C struct, Uid in the Go binding — is
// the *effective* uid at connect time. That is the right field: it is what the
// kernel would use for a permission decision, and comparing against
// os.Geteuid() keeps both sides of the comparison in the same terms.
func peerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("%w: %w", ErrPeerIdentity, err)
	}

	var cred *unix.Xucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return -1, fmt.Errorf("%w: %w", ErrPeerIdentity, err)
	}
	if credErr != nil {
		return -1, fmt.Errorf("%w: %w", ErrPeerIdentity, credErr)
	}
	return int(cred.Uid), nil
}
