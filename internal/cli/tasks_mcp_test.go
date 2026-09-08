// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"strings"
	"testing"
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
		"192.0.2.10:8080": "http://192.0.2.10:8080/mcp",
	} {
		if got := pingURL(addr); got != want {
			t.Errorf("pingURL(%q) = %q, want %q", addr, got, want)
		}
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
