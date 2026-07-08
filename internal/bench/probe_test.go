package bench

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
