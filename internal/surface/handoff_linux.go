//go:build linux

package surface

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the connected peer's effective uid via SO_PEERCRED.
//
// The credentials are captured by the kernel at connect time, not read from
// anything the peer sends, which is what makes them trustworthy. They are also
// a snapshot: the peer could exec something else afterwards, which is a
// same-UID concern and out of scope by the same argument as the nonce.
func peerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, fmt.Errorf("%w: %w", ErrPeerIdentity, err)
	}

	var ucred *unix.Ucred
	var credErr error
	// Control runs the closure with the descriptor guaranteed valid for its
	// duration; the error it returns is about obtaining that guarantee, and is
	// separate from the syscall's own error.
	if err := raw.Control(func(fd uintptr) {
		ucred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return -1, fmt.Errorf("%w: %w", ErrPeerIdentity, err)
	}
	if credErr != nil {
		return -1, fmt.Errorf("%w: %w", ErrPeerIdentity, credErr)
	}
	return int(ucred.Uid), nil
}
