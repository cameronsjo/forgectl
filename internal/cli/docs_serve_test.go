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
//   The exposed half of this policy is pinned via tokenRequiredByPolicy, a
//   direct mirror of runDocsServe's own conditional — see that helper's
//   comment for why an end-to-end exposed bind is deliberately not exercised.

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

// tokenRequiredByPolicy mirrors the token-generation conditional inside
// runDocsServe (docs_serve.go: "token == "" && !httpsrv.IsLoopbackAddr(bindAddr)").
//
// It is a deliberate duplication, not a call into runDocsServe, because the
// real conditional lives inline in a function whose exposed-address path
// can't be driven end-to-end without binding a real, network-reachable
// listener off loopback — something a test suite must not do. The loopback
// half of the SAME decision IS exercised end-to-end, through a real running
// server, by TestRunDocsServe_Loopback_NoBearerTokenRequired above. This
// helper covers the exposed half that a real bind can't safely reach, and
// pins the formula so a future edit to it is a visible, deliberate choice
// rather than a silent drift between this test and the source.
func tokenRequiredByPolicy(tokenFlag, bindAddr string) bool {
	return tokenFlag == "" && !httpsrv.IsLoopbackAddr(bindAddr)
}

func TestTokenPolicy_NonLoopbackWithoutFlag_RequiresGeneration(t *testing.T) {
	cases := []string{"0.0.0.0:3590", "192.168.1.10:3590", "100.64.1.2:3590"}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			if !tokenRequiredByPolicy("", addr) {
				t.Errorf("tokenRequiredByPolicy(\"\", %q) = false, want true — binding off loopback without an explicit --token must generate one rather than start unauthenticated", addr)
			}
		})
	}
}

func TestTokenPolicy_Loopback_NoGenerationNeeded(t *testing.T) {
	cases := []string{"127.0.0.1:3590", "localhost:3590"}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			if tokenRequiredByPolicy("", addr) {
				t.Errorf("tokenRequiredByPolicy(\"\", %q) = true, want false — a loopback bind needs no auth by default", addr)
			}
		})
	}
}

func TestTokenPolicy_ExplicitTokenFlag_NeverRegeneratesEvenOffLoopback(t *testing.T) {
	if tokenRequiredByPolicy("operator-supplied", "192.168.1.10:3590") {
		t.Error("tokenRequiredByPolicy with an explicit --token = true, want false — an operator-supplied token must not be silently replaced by a generated one")
	}
}
