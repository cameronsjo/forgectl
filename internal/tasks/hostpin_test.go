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
	err := checkHostPinning(context.Background(), runner, "127.0.0.1")
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
	if err := checkHostPinning(context.Background(), runner, "192.168.1.102"); err != nil {
		t.Fatalf("checkHostPinning(homelab LAN IP, matching gateway) = %v, want nil", err)
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
	err := checkHostPinning(context.Background(), runner, "192.168.1.102")
	if !errors.Is(err, ErrHostRefused) {
		t.Fatalf("checkHostPinning(RFC1918, foreign gateway) = %v, want errors.Is(ErrHostRefused)", err)
	}
}
