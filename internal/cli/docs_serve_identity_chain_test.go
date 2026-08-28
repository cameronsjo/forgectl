package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// writeDocsTokenFile writes an owner-only token file --token-file will accept.
func writeDocsTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunDocsServe_IdentityEndpointSitsBehindTheSecurityGates proves the
// discovery identity route is WIRED at the right point in the chain, not merely
// written.
//
// Its own unit test covers what the middleware does. This covers what that test
// cannot see: where runDocsServe puts it. The position is a security decision
// with a gate on each side, and getting it wrong in either direction is
// invisible to the compiler and to every unit test.
//
//   - Behind the cross-site gate, or a hostile page could enumerate the
//     generation of a docs server running on the victim's machine through the
//     victim's browser.
//   - Behind the Host allowlist, for the same reason via DNS rebinding.
//   - AHEAD of bearer auth, because a reader asking "are you the server I
//     found?" has not yet decided whether to present a credential — and if the
//     probe 401'd, a protected server could never be discovered at all.
func TestRunDocsServe_IdentityEndpointSitsBehindTheSecurityGates(t *testing.T) {
	isolateDocsDiscoveryState(t)

	idx := testDocsIndex(t)
	fake := &exec.FakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)
	stdout := &lockedBuffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(&lockedBuffer{})

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, module.Deps{Runner: fake}, idx, "127.0.0.1:0", false, "") }()
	t.Cleanup(cancel)

	var addr string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && addr == "" {
		addr = parseServedAddr(stdout.String())
		time.Sleep(2 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("the server never printed its address; stdout=%q", stdout.String())
	}

	// The record the server just published names the generation it must answer
	// for, so the test asks the same question discovery asks.
	server, ok := discoverDocsServer(t)
	if !ok {
		t.Fatal("the server published no discovery record")
	}
	generation := server.Info.Generation
	if generation == "" {
		t.Fatal("the published record carries no generation")
	}

	identityURL := "http://" + addr + "/.well-known/forgectl-docs"
	client := &http.Client{Timeout: 2 * time.Second}

	get := func(t *testing.T, method string, headers map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, identityURL, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for name, value := range headers {
			if name == "Host" {
				req.Host = value
				continue
			}
			req.Header.Set(name, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, identityURL, err)
		}
		return resp
	}

	// Baseline: an ordinary probe succeeds and names this server's generation.
	resp := get(t, http.MethodGet, nil)
	resp.Body.Close() //nolint:errcheck // only status and headers are under test
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("plain GET: status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := resp.Header.Get("X-Forgectl-Docs-Generation"); got != generation {
		t.Errorf("generation header = %q, want the published %q", got, generation)
	}

	// Cross-site: a page on another origin must not be able to read it.
	resp = get(t, http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"})
	resp.Body.Close() //nolint:errcheck // only the status is under test
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site GET: status = %d, want %d — the identity route must sit BEHIND RejectCrossSite, or a hostile page could enumerate this server's generation through the victim's browser", resp.StatusCode, http.StatusForbidden)
	}

	// DNS rebinding: a Host header outside the allowlist must not reach it.
	resp = get(t, http.MethodGet, map[string]string{"Host": "attacker.example"})
	resp.Body.Close() //nolint:errcheck // only the status is under test
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host GET: status = %d, want %d — the identity route must sit BEHIND HostAllowlist", resp.StatusCode, http.StatusForbidden)
	}

	// Method discipline: only GET and HEAD, and 405 names what is allowed.
	resp = get(t, http.MethodPost, nil)
	resp.Body.Close() //nolint:errcheck // only status and headers are under test
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("405 Allow header = %q, want %q", got, "GET, HEAD")
	}

	// The endpoint must disclose nothing beyond the generation.
	resp = get(t, http.MethodGet, nil)
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body) //nolint:errcheck // a short read is the expected outcome
	resp.Body.Close()            //nolint:errcheck // fully consumed above
	if n != 0 {
		t.Errorf("the identity endpoint returned a %d-byte body: %q", n, body[:n])
	}
	for _, header := range []string{"Access-Control-Allow-Origin", "Set-Cookie"} {
		if got := resp.Header.Get(header); got != "" {
			t.Errorf("the identity response carries %s: %q", header, got)
		}
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDocsServe after cancel: %v, want nil", err)
		}
	case <-time.After(shutdownWaitBudget):
		t.Fatal("runDocsServe did not shut down")
	}
}

// TestRunDocsServe_ProtectedServer_IdentityStillAnswersUnauthenticated is the
// other half of the ordering claim, and the one with a live failure mode: if the
// identity route were placed AFTER bearer authentication, a token-protected
// server would 401 every probe — while every loopback test in the suite stayed
// green, because loopback servers have no token to be blocked by.
//
// Verified by mutation: moving DiscoveryIdentity behind BearerToken turns this
// red. It fails at STARTUP rather than at the probe below, because the server's
// own self-probe is the first thing the misordered chain rejects — which is the
// designed behavior (a server that cannot answer for its own generation is not
// serving correctly), so the polling loop below reports that error rather than
// letting it read as a slow start.
func TestRunDocsServe_ProtectedServer_IdentityStillAnswersUnauthenticated(t *testing.T) {
	isolateDocsDiscoveryState(t)

	const token = "Az09-._~+/==="
	tokenPath := writeDocsTokenFile(t, token)

	idx := testDocsIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)
	stdout := &lockedBuffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(&lockedBuffer{})

	done := make(chan error, 1)
	go func() {
		done <- runDocsServe(cmd, module.Deps{Runner: &exec.FakeRunner{}}, idx, "127.0.0.1:0", false, tokenPath)
	}()
	t.Cleanup(cancel)

	var addr string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && addr == "" {
		if addr = parseServedAddr(stdout.String()); addr != "" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the protected server exited during startup: %v — a self-probe failure here means the identity route is not reachable without a credential, which is what placing it behind BearerToken would cause", err)
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("the protected server never printed its address; stdout=%q", stdout.String())
	}

	server, ok := discoverDocsServer(t)
	if !ok {
		t.Fatal("the protected server published no discovery record")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/.well-known/forgectl-docs", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET the identity path: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // only status and headers are under test

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unauthenticated identity probe on a protected server: status = %d, want %d — the identity route must sit AHEAD of BearerToken, or a protected server can never be discovered", resp.StatusCode, http.StatusNoContent)
	}
	if got := resp.Header.Get("X-Forgectl-Docs-Generation"); got != server.Info.Generation {
		t.Errorf("generation header = %q, want %q", got, server.Info.Generation)
	}
	// The pre-auth endpoint must not disclose the credential it sits in front of.
	for name, values := range resp.Header {
		for _, value := range values {
			if strings.Contains(value, token) {
				t.Errorf("the identity response leaks the bearer token in %s: %q", name, value)
			}
		}
	}
	if strings.Contains(stdout.String(), token) {
		t.Errorf("startup output leaked the --token-file value")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDocsServe after cancel: %v, want nil", err)
		}
	case <-time.After(shutdownWaitBudget):
		t.Fatal("runDocsServe did not shut down")
	}
}
