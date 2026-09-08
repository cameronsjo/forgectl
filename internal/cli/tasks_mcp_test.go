// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// TestValidateMCPFlags_HTTPRequiresATokenFile is the refusal the container
// transport rests on. Serving HTTP with no --token-file would leave the server
// looking for a credential it has no source for — and the only remaining
// source anyone would reach for is an environment variable, which is exactly
// what ADR 0009 rules out.
func TestValidateMCPFlags_HTTPRequiresATokenFile(t *testing.T) {
	err := validateMCPFlags(":3000", "", nil)
	if err == nil {
		t.Fatal("--http with no --token-file = nil, want a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"--http", "--token-file"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %s so the operator knows the pair: %s", want, msg)
		}
	}
}

func TestValidateMCPFlags_TokenFileWithoutHTTPIsRefused(t *testing.T) {
	err := validateMCPFlags("", "/run/secrets/token", nil)
	if err == nil {
		t.Fatal("--token-file with no --http = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "--token-file") || !strings.Contains(err.Error(), "--http") {
		t.Fatalf("the refusal must name both flags: %v", err)
	}
}

// TestValidateMCPFlags_PinIPRequiresHTTP: the second, weaker trust arm is
// bounded to the container transport on purpose. Accepting it on stdio would
// let a Mac session substitute the gateway corroboration with a flag value.
func TestValidateMCPFlags_PinIPRequiresHTTP(t *testing.T) {
	err := validateMCPFlags("", "", []string{"192.168.1.102"})
	if err == nil {
		t.Fatal("--pin-ip with no --http = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "--pin-ip") || !strings.Contains(err.Error(), "--http") {
		t.Fatalf("the refusal must name both flags: %v", err)
	}
}

// TestValidateMCPFlags_HTTPRequiresAPinList is the arm a security review
// found: with an empty pin list the host policy falls back to the base one,
// and inside a container the base policy has no live acceptance arm left
// except "public: allowed" — there is no `route` binary, so the default
// gateway is "" and the private-range arm can never pass. That posture would
// be reached by OMITTING a flag, so the flag is required rather than offered.
func TestValidateMCPFlags_HTTPRequiresAPinList(t *testing.T) {
	err := validateMCPFlags(":3000", "/run/secrets/token", nil)
	if err == nil {
		t.Fatal("--http with no --pin-ip = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "--pin-ip") || !strings.Contains(err.Error(), "--http") {
		t.Fatalf("the refusal must name both flags: %v", err)
	}
}

func TestValidateMCPFlags_AcceptsTheTwoValidShapes(t *testing.T) {
	if err := validateMCPFlags("", "", nil); err != nil {
		t.Fatalf("bare stdio = %v, want accepted", err)
	}
	if err := validateMCPFlags(":3000", "/run/secrets/token", []string{"192.168.1.102"}); err != nil {
		t.Fatalf("http with a token file and a pin = %v, want accepted", err)
	}
}

func TestPingURL_RewritesAWildcardBindToLoopback(t *testing.T) {
	for addr, want := range map[string]string{
		":3000":           "http://127.0.0.1:3000/mcp",
		"0.0.0.0:3000":    "http://127.0.0.1:3000/mcp",
		"[::]:3000":       "http://127.0.0.1:3000/mcp",
		"127.0.0.1:3000":  "http://127.0.0.1:3000/mcp",
		"[::1]:3000":      "http://[::1]:3000/mcp",
		"192.0.2.10:8080": "http://192.0.2.10:8080/mcp",
	} {
		got, err := pingURL(addr)
		if err != nil {
			t.Errorf("pingURL(%q) = error %v, want %q", addr, err, want)
			continue
		}
		if got != want {
			t.Errorf("pingURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

// TestPingURL_RefusesAnUnparseableAddress: the old fallback concatenated,
// turning `--http 3000` (a missing colon) into http://127.0.0.13000/mcp — a
// URL that fails to connect, so the healthcheck reported the SERVICE
// unreachable when the fault was a typo in its own argument.
func TestPingURL_RefusesAnUnparseableAddress(t *testing.T) {
	for _, addr := range []string{"3000", "::1:3000", ""} {
		if got, err := pingURL(addr); err == nil {
			t.Errorf("pingURL(%q) = %q with no error, want a refusal", addr, got)
		}
	}
}

// TestRunMCPPing_SucceedsAgainstARealStreamableHandler is the only test that
// exercises the healthcheck end to end, and it is the one that makes the
// Accept header a tested requirement rather than a comment. Both media types
// are mandatory for a streamable-HTTP server: drop `text/event-stream` and
// every probe in production fails, while every unit test stays green.
func TestRunMCPPing_SucceedsAgainstARealStreamableHandler(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout})
	// Wrapped exactly as production wraps it. Mounting the bare handler would
	// let a future tightening of the origin policy pass here and 403 in the
	// container, which is the shape of test that reassures without covering.
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(handler))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	if err := runMCPPing(cmd, srv.Listener.Addr().String()); err != nil {
		t.Fatalf("--ping against a live streamable handler = %v, want nil", err)
	}
}

// TestRunMCPPing_FailsAgainstAServerThatIsUpAndWrong is the negative control
// for the test above: without it, a green ping proves only that something
// answered. A 200 carrying a JSON-RPC error is exactly what an up-but-broken
// server returns, and a status-code-only probe calls that healthy.
func TestRunMCPPing_FailsAgainstAServerThatIsUpAndWrong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"denied"}}`))
	}))
	defer srv.Close()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	if err := runMCPPing(cmd, srv.Listener.Addr().String()); err == nil {
		t.Fatal("--ping against a 200 carrying a JSON-RPC error = nil, want a failure")
	}
}

// TestRunMCPPing_DoesNotLeakSessions is the regression test for a measured
// leak: the SDK never closes an idle session when SessionTimeout is unset, and
// --ping initializes without ever sending a DELETE. At a 30s healthcheck
// interval that is thousands of abandoned sessions and goroutines a day in a
// container meant to run for months.
func TestRunMCPPing_DoesNotLeakSessions(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "probe", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout})
	// Wrapped exactly as production wraps it. Mounting the bare handler would
	// let a future tightening of the origin policy pass here and 403 in the
	// container, which is the shape of test that reassures without covering.
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(handler))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)

	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		if err := runMCPPing(cmd, srv.Listener.Addr().String()); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
	}
	// Settle: the DELETE is best-effort and the server tears down
	// asynchronously, so a strict equality here would be flaky. The assertion
	// is that the count does not grow with the number of probes — 25 pings
	// leaking a session each showed +26 goroutines before the fix.
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	if grew := runtime.NumGoroutine() - before; grew > 10 {
		t.Fatalf("25 pings left %d goroutines behind — sessions are not being released", grew)
	}
}

// TestHasJSONRPCResult_RejectsAnErrorFrame is what stops --ping from being a
// healthcheck that could not go red: a server answering 200 with a JSON-RPC
// error is up and broken, and a status-code-only probe reports it healthy.
func TestHasJSONRPCResult_RejectsAnErrorFrame(t *testing.T) {
	cases := map[string]bool{
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`:   true,
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n": true,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"denied"}}`:  false,
		`{"jsonrpc":"2.0","id":1}`:            false,
		`not json at all`:                     false,
		``:                                    false,
		`<html>catch-all landing page</html>`: false,
	}
	for body, want := range cases {
		if got := hasJSONRPCResult([]byte(body)); got != want {
			t.Errorf("hasJSONRPCResult(%q) = %v, want %v", body, got, want)
		}
	}
}
