package bench

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPProber_HTTPReturnsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code, err := NewHTTPProber().Probe(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if code != http.StatusNoContent {
		t.Errorf("code = %d, want %d", code, http.StatusNoContent)
	}
}

func TestHTTPProber_TCPConnectSucceeds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	code, err := NewHTTPProber().Probe(context.Background(), "tcp://"+ln.Addr().String())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 (no HTTP status for a raw TCP connect)", code)
	}
}

func TestHTTPProber_TCPBoundedByContext(t *testing.T) {
	// A listening addr the dial would otherwise complete against — a cancelled
	// context must still make it fail fast, proving the connect is bounded (the
	// guard against a firewalled port stalling the status card).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	if _, err := NewHTTPProber().Probe(ctx, "tcp://"+ln.Addr().String()); err == nil {
		t.Error("Probe with a cancelled context = nil error, want a failure")
	}
	if elapsed := time.Since(start); elapsed >= probeTimeout {
		t.Errorf("dial took %v, want a fast return well under probeTimeout", elapsed)
	}
}

func TestHTTPProber_TCPConnectRefused(t *testing.T) {
	// Open then close a listener so the port is known-closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if _, err := NewHTTPProber().Probe(context.Background(), "tcp://"+addr); err == nil {
		t.Errorf("Probe on a closed port = nil error, want a connection failure")
	}
}
