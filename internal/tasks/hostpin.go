package tasks

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

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
//   - loopback or link-local: always refused. Nothing legitimate answers a
//     Vikunja request from 127.0.0.0/8 or 169.254.0.0/16.
//   - RFC1918 (10/8, 172.16/12, 192.168/16): accepted ONLY when gateway
//     equals HomelabGateway — otherwise this could be someone else's device
//     answering the same private address on a different network.
//   - 100.64.0.0/10 (Tailscale's CGNAT range): always accepted — the
//     tailnet name is the sanctioned off-LAN path.
//   - anything else (a public address): accepted. This client's own
//     transport (TLS) is the control past that point.
func classifyIP(ip net.IP, gateway string) (allowed bool, reason string) {
	if ip == nil {
		return false, "did not resolve to a usable address"
	}
	if ip.IsLoopback() {
		return false, fmt.Sprintf("resolves to %s (loopback)", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false, fmt.Sprintf("resolves to %s (link-local)", ip)
	}
	if isTailscaleCGNAT(ip) {
		return true, fmt.Sprintf("resolves to %s (tailnet)", ip)
	}
	if isRFC1918(ip) {
		if gateway == HomelabGateway {
			return true, fmt.Sprintf("resolves to %s (homelab LAN, gateway %s)", ip, gateway)
		}
		return false, fmt.Sprintf(
			"resolves to RFC1918 %s but the default gateway is %q, not the homelab's %s — use the tailnet name off-LAN",
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

func isRFC1918(ip net.IP) bool {
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil && block.Contains(ip) {
			return true
		}
	}
	return false
}

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

// checkHostPinning resolves host and refuses to proceed unless every
// resolved address is a sanctioned destination for the bearer token. It is
// called once per Client construction (not per-request) since the resolved
// answer for a stable hostname does not change within one process's
// lifetime, and DNS is itself a network call subject to the same bounded
// timeout every other request gets.
func checkHostPinning(ctx context.Context, runner exec.Runner, host string) error {
	ips, err := resolveHost(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %s does not resolve: %v", ErrUnreachable, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: %s did not resolve to any address", ErrUnreachable, host)
	}
	gateway := defaultGateway(ctx, runner)
	for _, ip := range ips {
		if allowed, reason := classifyIP(ip, gateway); !allowed {
			return fmt.Errorf("%w: %s %s", ErrHostRefused, host, reason)
		}
	}
	return nil
}
