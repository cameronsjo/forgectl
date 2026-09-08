package tasks

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// dockerGateway is the address a container's default route usually names —
// an RFC1918 address that is NOT the homelab's. Inside the container the
// gateway corroboration therefore always fails, which is exactly why the
// allow list exists: without a second arm the pinned client could never dial
// the LAN address it is deployed to reach.
const dockerGateway = "172.18.0.1"

func mustIPs(t *testing.T, addrs ...string) []net.IP {
	t.Helper()
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			t.Fatalf("not an IP: %q", a)
		}
		out = append(out, ip)
	}
	return out
}

// TestPinList_AdmitsAListedPrivateAddressUnderAForeignGateway is the arm the
// list exists for: list membership STANDS IN FOR the gateway corroboration,
// and nothing else about the policy relaxes.
func TestPinList_AdmitsAListedPrivateAddressUnderAForeignGateway(t *testing.T) {
	pins := mustIPs(t, "192.168.1.102")
	allowed, reason := classifyIPWithPins(net.ParseIP("192.168.1.102"), dockerGateway, pins)
	if !allowed {
		t.Fatalf("a listed private address under a container gateway = refused (%s), want allowed", reason)
	}
}

// TestPinList_RefusesADifferentRFC1918AddressUnderADockerGateway is the
// intersection, stated the way it fails: the list is not a widening of the
// private-range arm, it is a replacement for the gateway corroboration. An
// unlisted private address stays refused even though a container's own
// gateway can never satisfy the original arm either.
func TestPinList_RefusesADifferentRFC1918AddressUnderADockerGateway(t *testing.T) {
	pins := mustIPs(t, "192.168.1.102")
	allowed, reason := classifyIPWithPins(net.ParseIP("192.168.1.50"), dockerGateway, pins)
	if allowed {
		t.Fatalf("an UNLISTED private address = allowed (%s), want refused", reason)
	}
	if !strings.Contains(reason, "192.168.1.50") {
		t.Fatalf("the refusal must name the address it refused, got: %s", reason)
	}
}

// TestPinList_RefusesAPublicAddressEvenWhenListed is the arm that makes this
// an intersection rather than a fallback. Bare classifyIP ACCEPTS a public
// address (TLS is the control there); under a pin list it must not, or a
// hostile resolver answering with a public address it also happens to control
// would be admitted by the very flag added to narrow the client.
func TestPinList_RefusesAPublicAddressEvenWhenListed(t *testing.T) {
	pins := mustIPs(t, "1.1.1.1")
	allowed, reason := classifyIPWithPins(net.ParseIP("1.1.1.1"), dockerGateway, pins)
	if allowed {
		t.Fatalf("a listed PUBLIC address = allowed (%s), want refused", reason)
	}
}

func TestPinList_StillRefusesLoopbackLinkLocalAndMulticastWhenListed(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "169.254.1.1", "224.0.0.1", "0.0.0.0", "::1"} {
		pins := mustIPs(t, addr)
		if allowed, reason := classifyIPWithPins(net.ParseIP(addr), dockerGateway, pins); allowed {
			t.Errorf("classifyIPWithPins(%s, listed) = allowed (%s), want refused", addr, reason)
		}
	}
}

// TestPinList_EmptyListFallsBackToTheUnpinnedPolicy keeps `--pin-ip` opt-in:
// with no list the stdio transport's behaviour is byte-for-byte the old one.
func TestPinList_EmptyListFallsBackToTheUnpinnedPolicy(t *testing.T) {
	if allowed, _ := classifyIPWithPins(net.ParseIP("192.168.1.102"), HomelabGateway, nil); !allowed {
		t.Fatal("no list + homelab gateway = refused, want the unpinned policy's allow")
	}
	if allowed, _ := classifyIPWithPins(net.ParseIP("192.168.1.102"), dockerGateway, nil); allowed {
		t.Fatal("no list + foreign gateway = allowed, want the unpinned policy's refusal")
	}
	if allowed, _ := classifyIPWithPins(net.ParseIP("1.1.1.1"), "", nil); !allowed {
		t.Fatal("no list + public address = refused, want the unpinned policy's allow")
	}
}

// TestCheckHostPinning_NeverConsultsTheListWhenResolutionFails: a name that
// does not resolve is ErrUnreachable, never an admission. Reading the list as
// a fallback set of addresses would turn a DNS outage into "dial the pin
// anyway" — a silent bypass of the resolution the pin is meant to VET.
func TestCheckHostPinning_NeverConsultsTheListWhenResolutionFails(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "gateway: " + dockerGateway + "\n", nil },
	}
	pins := mustIPs(t, "192.168.1.102")
	vetted, _, err := checkHostPinning(context.Background(), runner, "does-not-exist.invalid", pins)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("checkHostPinning(unresolvable, with a list) = %v, want errors.Is(ErrUnreachable)", err)
	}
	if len(vetted) != 0 {
		t.Fatalf("vetted = %v, want empty — the list is not a resolution fallback", vetted)
	}
}

func TestCheckHostPinning_AdmitsTheListedAddressUnderAForeignGateway(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "gateway: " + dockerGateway + "\n", nil },
	}
	pins := mustIPs(t, "192.168.1.102")
	vetted, gateway, err := checkHostPinning(context.Background(), runner, "192.168.1.102", pins)
	if err != nil {
		t.Fatalf("checkHostPinning(listed address, foreign gateway) = %v, want nil", err)
	}
	if len(vetted) != 1 || !vetted[0].Equal(net.ParseIP("192.168.1.102")) {
		t.Fatalf("vetted = %v, want [192.168.1.102]", vetted)
	}
	if gateway != dockerGateway {
		t.Fatalf("gateway = %q, want %q", gateway, dockerGateway)
	}
}

func TestCheckHostPinning_RefusalNamesHostAddressesAndTheList(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "gateway: " + dockerGateway + "\n", nil },
	}
	pins := mustIPs(t, "192.168.1.102")
	_, _, err := checkHostPinning(context.Background(), runner, "192.168.1.50", pins)
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("checkHostPinning(unlisted address) = %v, want errors.Is(ErrHostRefused)", err)
	}
	msg := err.Error()
	for _, want := range []string{"192.168.1.50", "192.168.1.102"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not name %q: %s", want, msg)
		}
	}
}

// TestPinnedDialer_HonoursTheListToo: the check and the dial are two call
// sites of the same policy, and a list honoured at only one of them is the
// same check-once gap the vetted-address substitution already closed once.
func TestPinnedDialer_HonoursTheList(t *testing.T) {
	// Vetted holds an address the LIST does not — the shape a future edit
	// that widens the vetted set without widening the list would produce.
	dial := pinnedDialer(mustIPs(t, "192.168.1.50"), dockerGateway, mustIPs(t, "192.168.1.102"))
	_, err := dial(context.Background(), "tcp", "tasks.example:443")
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("dial to an address outside the list = %v, want errors.Is(ErrHostRefused)", err)
	}
}

func TestPinnedDialer_RefusesAPublicVettedAddressWhenAListIsSet(t *testing.T) {
	dial := pinnedDialer(mustIPs(t, "1.1.1.1"), dockerGateway, mustIPs(t, "1.1.1.1"))
	_, err := dial(context.Background(), "tcp", "tasks.example:443")
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("dial to a public pinned address = %v, want errors.Is(ErrHostRefused)", err)
	}
}
