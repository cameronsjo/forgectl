package tasks

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestClassifyIP_RefusesLoopback(t *testing.T) {
	allowed, reason := classifyIP(net.ParseIP("127.0.0.1"), "")
	if allowed {
		t.Fatalf("classifyIP(loopback) = allowed, want refused (reason: %s)", reason)
	}
}

func TestClassifyIP_RefusesLinkLocal(t *testing.T) {
	allowed, _ := classifyIP(net.ParseIP("169.254.1.1"), "")
	if allowed {
		t.Fatal("classifyIP(link-local) = allowed, want refused")
	}
}

func TestClassifyIP_RFC1918_AllowedOnlyOnHomelabGateway(t *testing.T) {
	ip := net.ParseIP("192.168.1.102")

	if allowed, _ := classifyIP(ip, HomelabGateway); !allowed {
		t.Fatal("classifyIP(RFC1918, homelab gateway) = refused, want allowed")
	}
	if allowed, reason := classifyIP(ip, "10.0.0.1"); allowed {
		t.Fatalf("classifyIP(RFC1918, foreign gateway) = allowed (reason: %s), want refused — this could be a stranger's device", reason)
	}
	if allowed, _ := classifyIP(ip, ""); allowed {
		t.Fatal("classifyIP(RFC1918, no gateway) = allowed, want refused")
	}
}

func TestClassifyIP_TailnetAlwaysAllowed(t *testing.T) {
	if allowed, _ := classifyIP(net.ParseIP("100.106.89.63"), "some-other-gateway"); !allowed {
		t.Fatal("classifyIP(tailnet CGNAT) = refused, want allowed regardless of gateway")
	}
}

func TestClassifyIP_PublicAllowed(t *testing.T) {
	if allowed, _ := classifyIP(net.ParseIP("1.1.1.1"), ""); !allowed {
		t.Fatal("classifyIP(public) = refused, want allowed")
	}
}

// TestCheckHostPinning_RefusesLoopbackResolution is the required
// "host pinning refuses a loopback resolution" test, exercised through the
// real entry point (checkHostPinning), not just the pure classifier.
func TestCheckHostPinning_RefusesLoopbackResolution(t *testing.T) {
	runner := &exec.FakeRunner{}
	_, _, err := checkHostPinning(context.Background(), runner, "127.0.0.1")
	if err == nil {
		t.Fatal("checkHostPinning(127.0.0.1) = nil, want ErrHostRefused")
	}
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("checkHostPinning(127.0.0.1) = %v, want errors.Is(ErrHostRefused)", err)
	}
}

func TestCheckHostPinning_AllowsHomelabLANWithMatchingGateway(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "route" {
				return "   route to: default\ngateway: 192.168.1.1\n   interface: en0\n", nil
			}
			return "", nil
		},
	}
	vetted, gateway, err := checkHostPinning(context.Background(), runner, "192.168.1.102")
	if err != nil {
		t.Fatalf("checkHostPinning(homelab LAN IP, matching gateway) = %v, want nil", err)
	}
	// The vetted set is the pin's whole product — an empty one would let
	// pinnedDialer fall through to "no pinned address" and turn a passing
	// check into a dead client.
	if len(vetted) != 1 || vetted[0].String() != "192.168.1.102" {
		t.Fatalf("vetted = %v, want exactly [192.168.1.102]", vetted)
	}
	if gateway != HomelabGateway {
		t.Fatalf("gateway = %q, want %q", gateway, HomelabGateway)
	}
}

func TestCheckHostPinning_RefusesRFC1918OffHomelab(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "route" {
				return "gateway: 10.0.0.1\n", nil
			}
			return "", nil
		},
	}
	_, _, err := checkHostPinning(context.Background(), runner, "192.168.1.102")
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("checkHostPinning(RFC1918, foreign gateway) = %v, want errors.Is(ErrHostRefused)", err)
	}
}

// TestClassifyIP_RefusesIPv6UniqueLocal pins the fix for the gap where the
// private-range test was a hand-rolled list of the three IPv4 CIDRs. An IPv6
// ULA fell past every refusal arm and was classified "public — accepted", so
// the gateway corroboration this control is built around was never reached on
// the IPv6 path. Negative control: reverting classifyIP to the IPv4-only
// isRFC1918 makes this test fail.
func TestClassifyIP_RefusesIPv6UniqueLocalOffHomelab(t *testing.T) {
	allowed, reason := classifyIP(net.ParseIP("fd00::1"), "10.0.0.1")
	if allowed {
		t.Fatalf("classifyIP(fd00::1, foreign gateway) = allowed, want refused (reason: %s)", reason)
	}
}

func TestClassifyIP_IPv6UniqueLocalAllowedOnHomelabGateway(t *testing.T) {
	// The corroboration must still ADMIT the private address when the
	// gateway matches — otherwise the fix above would be a blanket IPv6 ban
	// wearing a refinement's clothes.
	allowed, reason := classifyIP(net.ParseIP("fd00::1"), HomelabGateway)
	if !allowed {
		t.Fatalf("classifyIP(fd00::1, homelab gateway) = refused (%s), want allowed", reason)
	}
}

func TestClassifyIP_RefusesUnspecifiedAndMulticast(t *testing.T) {
	for _, addr := range []string{"0.0.0.0", "::", "224.0.0.1", "ff02::1"} {
		if allowed, reason := classifyIP(net.ParseIP(addr), HomelabGateway); allowed {
			t.Errorf("classifyIP(%s) = allowed, want refused (reason: %s)", addr, reason)
		}
	}
}

// TestPinnedDialer_DialsOnlyTheVettedAddress is the fix for the check-once
// gap: checkHostPinning classified the resolved addresses and then let
// http.Transport resolve the name a SECOND time at dial, so a resolver
// answering the two lookups differently sent the bearer token to an address
// the pin never saw. The dialer must ignore the hostname in the dial address
// entirely and connect to the vetted IP.
func TestPinnedDialer_DialsOnlyTheVettedAddress(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.Close()
		}
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort: %v", err)
	}
	// classifyIP refuses loopback, so pin the listener's address while
	// asserting against a classifier that would allow it — the point under
	// test is the ADDRESS SUBSTITUTION, not the policy (covered above).
	dial := pinnedDialerWithClassifier(
		[]net.IP{net.ParseIP(host)}, HomelabGateway,
		func(net.IP, string) (bool, string) { return true, "test" },
	)

	// The dial address names a hostname that does NOT resolve to the
	// listener. If the dialer honoured it, this connect could not succeed.
	conn, err := dial(context.Background(), "tcp", net.JoinHostPort("host.invalid", port))
	if err != nil {
		t.Fatalf("pinnedDialer did not dial the vetted address: %v", err)
	}
	_ = conn.Close()
}

func TestPinnedDialer_RefusesWhenAPinnedAddressFailsTheClassifier(t *testing.T) {
	dial := pinnedDialer([]net.IP{net.ParseIP("127.0.0.1")}, HomelabGateway)
	_, err := dial(context.Background(), "tcp", "tasks.example:443")
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("dial to a loopback pin = %v, want errors.Is(ErrHostRefused)", err)
	}
}
