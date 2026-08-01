package cli

// Test plan for docs_serve.go
//
// runDocsServe (Classification: API handler — server lifecycle)
//   [x] Happy: context cancellation (Ctrl-C/SIGTERM) triggers a graceful
//       shutdown that returns nil, not an error
//   [x] Happy: --open (openFlag=true) invokes the browser opener with the
//       served URL via the injected Runner (no real browser launched)
//   [x] Unhappy: an invalid bind address returns a wrapped error naming "bind"
//   [x] Happy (security): a loopback bind serves requests with no
//       Authorization header at all — no token is required
//   [x] Unhappy (security): a request carrying Sec-Fetch-Site: cross-site is
//       rejected 403 by the running server, while the same request without the
//       header 200s. Exercised against a REAL server rather than the
//       middleware alone, because what can regress here is the CHAIN — a
//       reordering or a dropped entry in runDocsServe leaves the middleware's
//       own unit tests green
//
// allowedHosts (Classification: security gate — Host allowlist construction)
//   [x] Happy: a non-loopback bind address adds the bound host to the
//       defaults
//   [x] Happy: a loopback bind address adds nothing beyond the defaults
//
// Token policy in runDocsServe (Classification: security-critical — decides
// whether a docs server starts unauthenticated)
//   [x] Happy: a non-loopback bind with no --token requires a generated token
//   [x] Happy: a loopback bind needs no generated token
//   [x] Happy: an explicit --token is never silently replaced, even off loopback
//   [x] Happy: two generated tokens differ
//   These call resolveToken — the same function runDocsServe calls — so they
//   fail if the server's rule regresses. An end-to-end EXPOSED bind is
//   deliberately not exercised: it would open a real network port during the
//   test run. The loopback half is covered end-to-end as well, above.

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/httpsrv"
	"github.com/cameronsjo/forgectl/internal/module"
)

func testCmdWithContext(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(ctx)
	return cmd
}

func testDocsIndex(t *testing.T) *docspkg.Index {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# Hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := docspkg.NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("docspkg.NewIndex: %v", err)
	}
	return idx
}

func TestRunDocsServe_ContextCancel_GracefulShutdown(t *testing.T) {
	idx := testDocsIndex(t)
	deps := module.Deps{Runner: &exec.FakeRunner{}}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, deps, idx, "127.0.0.1:0", false, "") }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDocsServe after cancel: %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDocsServe did not return within 5s of context cancellation")
	}
}

func TestRunDocsServe_OpenFlag_InvokesBrowserOpener(t *testing.T) {
	idx := testDocsIndex(t)
	fake := &exec.FakeRunner{}
	deps := module.Deps{Runner: fake}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, deps, idx, "127.0.0.1:0", true, "") }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDocsServe did not return within 5s")
	}

	call := fake.Last()
	if call.Name != "open" && call.Name != "xdg-open" {
		t.Fatalf("browser opener call.Name = %q, want open or xdg-open", call.Name)
	}
	if len(call.Args) != 1 || !strings.HasPrefix(call.Args[0], "http://127.0.0.1:") {
		t.Errorf("browser opener args = %v, want a single http://127.0.0.1:<port>/ URL", call.Args)
	}
}

func TestRunDocsServe_InvalidAddr_ErrorsWrapped(t *testing.T) {
	idx := testDocsIndex(t)
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	cmd := testCmdWithContext(context.Background())

	err := runDocsServe(cmd, deps, idx, "not-a-valid-address", false, "")
	if err == nil {
		t.Fatal("runDocsServe with an invalid address: got nil error, want one")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("error = %v, want it to name the bind failure", err)
	}
}

// waitForOpenedURL polls fake until runDocsServe's --open path has invoked
// the browser opener, then returns the served URL it was called with. Used
// instead of a fixed sleep so the test isn't racing an arbitrary delay
// against however long bind + store + watcher setup happens to take.
func waitForOpenedURL(t *testing.T, fake *exec.FakeRunner) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if call := fake.Last(); call.Name != "" {
			if len(call.Args) != 1 {
				t.Fatalf("browser opener args = %v, want exactly one URL arg", call.Args)
			}
			return call.Args[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("runDocsServe never invoked the browser opener within 2s")
	return ""
}

// TestRunDocsServe_Loopback_NoBearerTokenRequired is the loopback half of
// runDocsServe's token policy, exercised end-to-end against a REAL running
// server (unlike the exposed half below, a loopback bind is always safe to
// stand up in a test). A GET with no Authorization header at all must
// succeed — proving no BearerToken middleware was wired in, not just that
// some particular header value happens to be accepted.
func TestRunDocsServe_Loopback_NoBearerTokenRequired(t *testing.T) {
	idx := testDocsIndex(t)
	fake := &exec.FakeRunner{}
	deps := module.Deps{Runner: fake}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, deps, idx, "127.0.0.1:0", true, "") }()

	url := waitForOpenedURL(t, fake)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close() //nolint:errcheck // response already read to completion by the caller's decision below
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s carrying no Authorization header: status = %d, want %d — a loopback bind must not require a bearer token", url, resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDocsServe after cancel: %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDocsServe did not return within 5s of context cancellation")
	}
}

// TestRunDocsServe_CrossSiteRequest_Rejected403 proves the cross-site gate is
// WIRED, not merely written. httpsrv's own tests cover RejectCrossSite's
// behavior; this covers the thing they cannot see — that runDocsServe actually
// puts it in the chain around the docs handler. A dropped or misordered entry
// there is invisible to a middleware unit test and to the compiler.
//
// Both halves matter. The cross-site request must 403, and the otherwise
// identical request without the header must 200: a gate that rejected
// everything would satisfy the first assertion alone while breaking the reader.
func TestRunDocsServe_CrossSiteRequest_Rejected403(t *testing.T) {
	idx := testDocsIndex(t)
	fake := &exec.FakeRunner{}
	deps := module.Deps{Runner: fake}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := testCmdWithContext(ctx)

	done := make(chan error, 1)
	go func() { done <- runDocsServe(cmd, deps, idx, "127.0.0.1:0", true, "") }()

	url := waitForOpenedURL(t, fake)
	client := &http.Client{Timeout: 2 * time.Second}

	get := func(t *testing.T, header string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if header != "" {
			req.Header.Set("Sec-Fetch-Site", header)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close() //nolint:errcheck // only the status code is under test
		return resp.StatusCode
	}

	if got := get(t, "cross-site"); got != http.StatusForbidden {
		t.Errorf("GET %s with Sec-Fetch-Site: cross-site: status = %d, want %d — the running server must reject a request another origin's page initiated, which means RejectCrossSite has to be in runDocsServe's middleware chain", url, got, http.StatusForbidden)
	}
	if got := get(t, ""); got != http.StatusOK {
		t.Errorf("GET %s with no Sec-Fetch-Site header: status = %d, want %d — a client that sends no fetch metadata (curl, the Go http.Client behind `docs open`) must still be served", url, got, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDocsServe after cancel: %v, want nil (graceful shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDocsServe did not return within 5s of context cancellation")
	}
}

func containsHost(hosts []string, want string) bool {
	for _, h := range hosts {
		if h == want {
			return true
		}
	}
	return false
}

func TestAllowedHosts_NonLoopback_AddsBoundHost(t *testing.T) {
	got := allowedHosts("192.168.1.10:3590")

	if !containsHost(got, "192.168.1.10") {
		t.Errorf("allowedHosts(%q) = %v, want it to include the bound host — otherwise every request to it would 403 on the Host allowlist", "192.168.1.10:3590", got)
	}
	for _, want := range httpsrv.DefaultAllowedHosts {
		if !containsHost(got, want) {
			t.Errorf("allowedHosts(%q) = %v, missing default host %q", "192.168.1.10:3590", got, want)
		}
	}
}

func TestAllowedHosts_Loopback_NoAddition(t *testing.T) {
	got := allowedHosts("127.0.0.1:3590")

	if len(got) != len(httpsrv.DefaultAllowedHosts) {
		t.Errorf("allowedHosts(%q) = %v, want exactly the defaults with nothing added for a loopback bind", "127.0.0.1:3590", got)
	}
}

// The token-policy tests below call resolveToken — the SAME function
// runDocsServe calls — rather than re-implementing its conditional here.
//
// That distinction is the point. The off-loopback path cannot be driven
// end-to-end without binding a real network-reachable listener during the test
// run, which a suite must not do; and a test that copied the conditional to work
// around that would stay green after the server's own rule changed, which is
// worse than no test because it reads as coverage. Extracting the policy into
// resolveToken means these tests fail when the server's behavior regresses. The
// loopback half is ALSO covered end-to-end through a real running server by
// TestRunDocsServe_Loopback_NoBearerTokenRequired above.

func TestResolveToken_NonLoopbackWithoutFlag_GeneratesAToken(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:3590", "192.168.1.10:3590", "100.64.1.2:3590", ":3590"} {
		t.Run(addr, func(t *testing.T) {
			got, err := resolveToken("", addr)
			if err != nil {
				t.Fatalf("resolveToken(\"\", %q): %v", addr, err)
			}
			if got == "" {
				t.Errorf("resolveToken(\"\", %q) = \"\", want a generated token — binding off loopback must never start unauthenticated", addr)
			}
		})
	}
}

func TestResolveToken_Loopback_ReturnsNoToken(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:3590", "127.0.0.1:0", "localhost:3590", "[::1]:3590"} {
		t.Run(addr, func(t *testing.T) {
			got, err := resolveToken("", addr)
			if err != nil {
				t.Fatalf("resolveToken(\"\", %q): %v", addr, err)
			}
			if got != "" {
				t.Errorf("resolveToken(\"\", %q) = %q, want \"\" — a loopback bind needs no auth by default", addr, got)
			}
		})
	}
}

func TestResolveToken_ExplicitFlag_IsNeverReplaced(t *testing.T) {
	const supplied = "operator-supplied-token"
	for _, addr := range []string{"192.168.1.10:3590", "127.0.0.1:3590"} {
		t.Run(addr, func(t *testing.T) {
			got, err := resolveToken(supplied, addr)
			if err != nil {
				t.Fatalf("resolveToken(%q, %q): %v", supplied, addr, err)
			}
			if got != supplied {
				t.Errorf("resolveToken(%q, %q) = %q, want the supplied token unchanged — an operator's token must not be silently replaced", supplied, addr, got)
			}
		})
	}
}

func TestResolveToken_GeneratedTokensDiffer(t *testing.T) {
	first, err := resolveToken("", "192.168.1.10:3590")
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	second, err := resolveToken("", "192.168.1.10:3590")
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if first == second {
		t.Errorf("two generated tokens are identical (%q) — a predictable token is not a token", first)
	}
}
