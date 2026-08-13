package docs

import (
	"errors"
	"net"
	"net/netip"
)

// Fixed advertised-address failures. A server that hits one of these keeps
// serving; it just never becomes discoverable, and it never constructs a
// generation source or an identity endpoint it could not honestly answer for.
var (
	errAdvertisedAddrType = errors.New("the listener does not expose a numeric TCP address")
	errAdvertisedAddrPort = errors.New("the listener resolved to port zero")
	errAdvertisedAddrZone = errors.New("scoped IPv6 addresses are not published for discovery")
)

// AdvertisedDocsAddr derives the address a discovery record should publish from
// the address the listener actually bound.
//
// It deliberately does not consult the operator's bind string. Token policy
// still examines that (binding to 0.0.0.0 is an exposure decision and stays
// one), but discovery needs an address a reader on this machine can connect
// to, and the bind string is exactly the wrong source for that: "0.0.0.0:0"
// and ":3590" name no reachable endpoint, and a hostname bind names one only
// via a resolver that may answer differently for the reader.
//
// So an unspecified bind advertises the loopback address of the same family
// plus the port the kernel assigned. That narrows what discovery advertises
// relative to what the server accepts, which is the safe direction: a reader
// steered to loopback reaches the same server, and a record naming a routable
// address the reader cannot reach would be worse than no record.
func AdvertisedDocsAddr(listenerAddr net.Addr) (string, error) {
	tcp, ok := listenerAddr.(*net.TCPAddr)
	if !ok {
		return "", errAdvertisedAddrType
	}
	if tcp.Port <= 0 || tcp.Port > 65535 {
		return "", errAdvertisedAddrPort
	}
	if tcp.Zone != "" {
		return "", errAdvertisedAddrZone
	}

	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		// A nil IP is what a listener bound to a bare ":port" reports.
		ip = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	ip = ip.Unmap()
	if ip.Zone() != "" {
		return "", errAdvertisedAddrZone
	}
	if ip.IsUnspecified() {
		if ip.Is4() {
			ip = netip.AddrFrom4([4]byte{127, 0, 0, 1})
		} else {
			ip = netip.IPv6Loopback()
		}
	}
	return netip.AddrPortFrom(ip, uint16(tcp.Port)).String(), nil
}

// validateAdvertisedAddr is the reader-side half of the same contract: an
// address is usable only when it parses as numeric host:port, names a nonzero
// port, and points at this machine.
//
// Parsing with netip.ParseAddrPort rather than net.SplitHostPort is what keeps
// DNS out of records. A record naming a hostname would hand the resolver a say
// in which server `docs open` steers to, and the resolver is not part of the
// trust boundary this directory establishes.
func validateAdvertisedAddr(addr string, localIP func(netip.Addr) bool) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return errInvalidAddr
	}
	if ap.Port() == 0 {
		return errInvalidAddr
	}
	ip := ap.Addr()
	if ip.Zone() != "" {
		return errAdvertisedAddrZone
	}
	if ip.Is4In6() {
		// One address with two spellings would be one server with two
		// identities; the writer never emits this form, so a reader seeing it
		// is reading something the writer did not produce.
		return errInvalidAddr
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return errInvalidAddr
	}
	if ip.IsLoopback() {
		return nil
	}
	if localIP == nil || !localIP(ip) {
		return errInvalidAddr
	}
	return nil
}

// isLocallyAssignedIP reports whether ip is currently assigned to an interface
// on this machine.
//
// This is the check that stops a record — which any process running as this
// user can write — from steering `docs open` at a remote host. Without it, a
// record naming an external address would turn discovery into an outbound
// request carrying the stored bearer token.
func isLocallyAssignedIP(ip netip.Addr) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		var candidate net.IP
		switch v := a.(type) {
		case *net.IPNet:
			candidate = v.IP
		case *net.IPAddr:
			candidate = v.IP
		default:
			continue
		}
		got, ok := netip.AddrFromSlice(candidate)
		if !ok {
			continue
		}
		if got.Unmap() == ip {
			return true
		}
	}
	return false
}
