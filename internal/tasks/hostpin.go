package tasks

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// HomelabGateway is the homelab LAN's default gateway address. An RFC1918
// resolution is trusted only when this machine's own default gateway equals
// this value — the corroboration that stops a corporate or coffee-shop LAN's
// split-horizon DNS from silently handing the bearer token to a stranger's
// device at the same private address.
const HomelabGateway = "192.168.1.1"

// ErrHostRefused reports that the resolved host is not one this client will
// send a bearer token to.
var ErrHostRefused = fmt.Errorf("tasks: refusing to send the bearer token to this host")

var gatewayRe = regexp.MustCompile(`gateway:\s*(\S+)`)

// classifyIP reports whether ip is a safe destination for the bearer token,
// and why. gateway is this machine's own default gateway address (empty if
// undetermined). Pure — no DNS, no process execution — so the policy is
// tested directly against synthetic inputs.
//
//   - loopback, link-local, unspecified, or multicast: always refused.
//     Nothing legitimate answers a Vikunja request from any of them, and
//     0.0.0.0 in particular is dialed as localhost by most stacks.
//   - private (10/8, 172.16/12, 192.168/16, and IPv6 fc00::/7): accepted
//     ONLY when gateway equals HomelabGateway — otherwise this could be
//     someone else's device answering the same private address on a
//     different network.
//   - 100.64.0.0/10 (Tailscale's CGNAT range): always accepted — the
//     tailnet name is the sanctioned off-LAN path.
//   - anything else (a public address): accepted. This client's own
//     transport (TLS) is the control past that point.
//
// The private-range test is net.IP.IsPrivate, not a hand-rolled IPv4 CIDR
// list. The hand-rolled version tested only the three IPv4 blocks, so an
// IPv6 unique-local address (fd00::1) fell past every refusal arm and was
// accepted as "public" — the gateway corroboration this whole control is
// built around was simply never reached on the IPv6 path.
func classifyIP(ip net.IP, gateway string) (allowed bool, reason string) {
	if ip == nil {
		return false, "did not resolve to a usable address"
	}
	if ip.IsUnspecified() {
		return false, fmt.Sprintf("resolves to %s (unspecified)", ip)
	}
	if ip.IsLoopback() {
		return false, fmt.Sprintf("resolves to %s (loopback)", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false, fmt.Sprintf("resolves to %s (link-local)", ip)
	}
	if ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false, fmt.Sprintf("resolves to %s (multicast)", ip)
	}
	if isTailscaleCGNAT(ip) {
		return true, fmt.Sprintf("resolves to %s (tailnet)", ip)
	}
	if ip.IsPrivate() {
		if gateway == HomelabGateway {
			return true, fmt.Sprintf("resolves to %s (homelab LAN, gateway %s)", ip, gateway)
		}
		return false, fmt.Sprintf(
			"resolves to private-range %s but the default gateway is %q, not the homelab's %s — use the tailnet name off-LAN",
			ip, orNone(gateway), HomelabGateway)
	}
	return true, fmt.Sprintf("resolves to %s (public)", ip)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// isTailscaleCGNAT reports whether ip is in 100.64.0.0/10.
//
// Accepted with NO gateway corroboration, unlike the private-range arm — and
// that asymmetry is deliberate but load-bearing, so it is stated here rather
// than left to be rediscovered. RFC 6598 is SHARED carrier-grade NAT space,
// not Tailscale's: mobile carriers, many ISPs, and some campus networks hand
// out addresses in it, so a hostile resolver on such a network can steer the
// hostname into an always-allowed range. For this range the pin is therefore
// not the control — TLS certificate verification is, and a wrong server fails
// the handshake rather than receiving the token. Weakening TLS on this path
// removes the only thing standing here. See docs/adr/0009 §3b.
func isTailscaleCGNAT(ip net.IP) bool {
	_, block, err := net.ParseCIDR("100.64.0.0/10")
	return err == nil && block.Contains(ip)
}

// resolveHost resolves host to its IPv4/IPv6 addresses. An IP literal
// resolves to itself; a hostname is looked up via the standard resolver.
func resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	var resolver net.Resolver
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// defaultGateway shells out to `route -n get default` (macOS) and parses the
// gateway line. Best-effort: an error or unparseable output yields "", which
// classifyIP treats as "not the homelab" (fail closed on the RFC1918 branch).
func defaultGateway(ctx context.Context, runner exec.Runner) string {
	out, err := runner.Run(ctx, "route", "-n", "get", "default")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if m := gatewayRe.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// checkHostPinning resolves host, refuses to proceed unless every resolved
// address is a sanctioned destination for the bearer token, and returns the
// addresses it vetted plus the gateway it corroborated against.
//
// Returning the vetted set is what makes this a pin rather than an omen. An
// earlier revision checked here and let http.Transport resolve the name again
// at dial time — two independent lookups, so a resolver that answers the
// check with a public address and the dial with 192.168.1.50 sends the token
// to the attacker's device while this function reports success. That resolver
// is the exact adversary the control names, so the gap voided the control on
// its own stated threat. The caller MUST dial only the returned addresses;
// pinnedDialer is the mechanism, and the gateway rides along so a dial-time
// re-check needs no second `route` call.
func checkHostPinning(ctx context.Context, runner exec.Runner, host string) (vetted []net.IP, gateway string, err error) {
	ips, err := resolveHost(ctx, host)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s does not resolve: %v", ErrUnreachable, host, err)
	}
	if len(ips) == 0 {
		return nil, "", fmt.Errorf("%w: %s did not resolve to any address", ErrUnreachable, host)
	}
	gateway = defaultGateway(ctx, runner)
	for _, ip := range ips {
		if allowed, reason := classifyIP(ip, gateway); !allowed {
			return nil, "", fmt.Errorf("%w: %s %s", ErrHostRefused, host, reason)
		}
	}
	return ips, gateway, nil
}

// pinnedDialer returns a DialContext that dials ONLY the vetted addresses,
// in order, and re-runs classifyIP on each before connecting. The address
// substitution closes the two-lookup gap described on checkHostPinning; the
// re-check is belt-and-braces, so that a future edit which widens the vetted
// set cannot silently widen what gets dialed.
//
// The port from the requested address is preserved — only the host part is
// replaced, so an instance on a non-443 port still works.
func pinnedDialer(vetted []net.IP, gateway string) func(context.Context, string, string) (net.Conn, error) {
	return pinnedDialerWithClassifier(vetted, gateway, classifyIP)
}

// pinnedDialerWithClassifier is pinnedDialer with the policy injected. The
// seam exists because classifyIP refuses loopback by design, and a test that
// wants to prove the ADDRESS SUBSTITUTION works has to dial a real listener,
// which is on loopback. Separating the two lets each be tested for what it
// actually does instead of one masking the other.
func pinnedDialerWithClassifier(
	vetted []net.IP,
	gateway string,
	classify func(net.IP, string) (bool, string),
) func(context.Context, string, string) (net.Conn, error) {
	pinned := make([]net.IP, len(vetted))
	copy(pinned, vetted)
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot parse dial address %q: %v", ErrHostRefused, addr, err)
		}
		var lastErr error
		for _, ip := range pinned {
			if allowed, reason := classify(ip, gateway); !allowed {
				return nil, fmt.Errorf("%w: pinned address %s", ErrHostRefused, reason)
			}
			conn, dialErr := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			// An empty pinned set must FAIL, never fall through to the
			// transport's own resolution — that fall-through is the whole
			// gap this dialer exists to close.
			lastErr = fmt.Errorf("%w: no vetted address to dial", ErrHostRefused)
		}
		return nil, lastErr
	}
}
