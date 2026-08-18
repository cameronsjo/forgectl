//go:build !darwin && !linux

package surface

import "net"

// peerUID is unavailable off Darwin and Linux.
//
// It refuses rather than returning the current uid, which would make every
// connection look local and turn the one check carrying same-user exclusion
// into a rubber stamp. forgectl's release targets are Darwin and Linux; a
// platform without a peer-credential mechanism does not get a surface.
func peerUID(*net.UnixConn) (int, error) { return -1, ErrPeerUnsupported }
