// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
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

// pingBoardTitle is the one project title the ping's board serves. The ping
// reads the board through the server and must print none of it.
const pingBoardTitle = "PLANTED-PROJECT-TITLE-5e19"

// pingBoard is a stub Vikunja for the ping: GET /projects answers status, with
// one project when status is 200, and the number of reads is kept.
type pingBoard struct {
	mu     sync.Mutex
	status int
	reads  int
}

func (b *pingBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	b.mu.Lock()
	b.reads++
	status := b.status
	b.mu.Unlock()
	if r.URL.Path != "/projects" || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"token invalid"}`))
		return
	}
	if page := r.URL.Query().Get("page"); page != "" && page != "1" {
		_, _ = w.Write([]byte(`[]`))
		return
	}
	_, _ = w.Write([]byte(`[{"id":1,"title":"` + pingBoardTitle + `"}]`))
}

func (b *pingBoard) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

// pingTarget serves the real tasks MCP server over a stub board, wrapped as
// production wraps it, and returns its address. Mounting the bare handler
// would let a future tightening of the origin policy pass here and 403 in the
// container, which is the shape of test that reassures without covering.
func pingTarget(t *testing.T, boardStatus int) (addr string, board *pingBoard, server *mcp.Server) {
	t.Helper()
	board = &pingBoard{status: boardStatus}
	boardSrv := httptest.NewServer(board)
	t.Cleanup(boardSrv.Close)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(tasksTestFakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ReadTokenFile(tokenPath)
	if err != nil {
		t.Fatalf("ReadTokenFile: %v", err)
	}
	client := tasks.NewClientForTesting(boardSrv.URL, token)
	server = tasks.NewMCPServer(client, tasks.MCPConfig{DefaultClientName: "forgectl (http)", Records: io.Discard})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout})
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(handler))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), board, server
}

func runPing(t *testing.T, addr string) (stdout string, err error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err = runMCPPing(cmd, addr)
	return out.String(), err
}

// TestRunMCPPing_SucceedsAgainstARealStreamableHandler is the healthcheck end
// to end, and it is the one that makes the Accept header a tested requirement
// rather than a comment. Both media types are mandatory for a streamable-HTTP
// server: drop `text/event-stream` and every probe in production fails, while
// every unit test stays green.
//
// It also pins that the ping reads the board: a ping that stopped at
// initialize reports a container healthy whose credential is dead.
func TestRunMCPPing_SucceedsAgainstARealStreamableHandler(t *testing.T) {
	addr, board, _ := pingTarget(t, http.StatusOK)
	out, err := runPing(t, addr)
	if err != nil {
		t.Fatalf("--ping against a live server with a working credential = %v, want nil", err)
	}
	if out != "ok\n" {
		t.Errorf("--ping printed %q, want exactly ok", out)
	}
	if board.readCount() == 0 {
		t.Fatal("--ping succeeded without the server reading the board, so a dead credential would pass it")
	}
}

// TestRunMCPPing_ARefusedCredentialFails is the case the read exists for: the
// server is up, and the board refuses its token. That is unhealthy, with the
// credential exit code, and the board's text is printed nowhere.
func TestRunMCPPing_ARefusedCredentialFails(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		addr, _, _ := pingTarget(t, status)
		out, err := runPing(t, addr)
		if err == nil {
			t.Fatalf("--ping with the board answering %d = nil, want a failure", status)
		}
		if ExitCode(err) != exitTasksUnauthorized {
			t.Errorf("board %d: exit code = %d, want %d", status, ExitCode(err), exitTasksUnauthorized)
		}
		msg := err.Error()
		if !strings.Contains(msg, "up") || !strings.Contains(msg, "credential") {
			t.Errorf("board %d: the message does not say the server is up and its credential was refused: %q", status, msg)
		}
		if strings.Contains(msg, "\n") {
			t.Errorf("board %d: the message is more than one line: %q", status, msg)
		}
		for _, where := range []string{out, msg} {
			if strings.Contains(where, pingBoardTitle) || strings.Contains(where, "token invalid") || strings.Contains(where, tasksTestFakeToken) {
				t.Errorf("board %d: the ping printed board text or the token: %q", status, where)
			}
		}
		if strings.Contains(out, "ok") {
			t.Errorf("board %d: a failed ping printed ok: %q", status, out)
		}
	}
}

// TestRunMCPPing_AFailedReadFails: a read that fails for any other reason is
// unhealthy too, under the generic exit code, and says the server is up.
func TestRunMCPPing_AFailedReadFails(t *testing.T) {
	addr, _, _ := pingTarget(t, http.StatusInternalServerError)
	out, err := runPing(t, addr)
	if err == nil {
		t.Fatal("--ping with the board answering 500 = nil, want a failure")
	}
	if ExitCode(err) != 1 {
		t.Errorf("exit code = %d, want 1", ExitCode(err))
	}
	if !strings.Contains(err.Error(), "up") || !strings.Contains(err.Error(), "read") {
		t.Errorf("the message does not say the server is up and the read failed: %q", err)
	}
	if strings.Contains(out, pingBoardTitle) || strings.Contains(err.Error(), pingBoardTitle) {
		t.Errorf("the ping printed board text: %q / %q", out, err)
	}
}

// TestRunMCPPing_EndsItsSession: the ping now holds its session across a tool
// call, so it must end it when done, on success and on failure alike.
func TestRunMCPPing_EndsItsSession(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		addr, _, server := pingTarget(t, status)
		_, _ = runPing(t, addr)
		deadline := time.Now().Add(2 * time.Second)
		for {
			n := 0
			for range server.Sessions() {
				n++
			}
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("board %d: %d session(s) still open after the ping returned", status, n)
			}
			time.Sleep(20 * time.Millisecond)
		}
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
	addr, _, _ := pingTarget(t, http.StatusOK)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)

	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		if err := runMCPPing(cmd, addr); err != nil {
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

// TestPingURL_NeverEchoesTheAddress: a --http value can carry credentials
// (tok@127.0.0.1:3000 splits into host "tok@127.0.0.1", which net/http sends
// as userinfo and prints unmasked), so pingURL refuses any host that is not an
// IP or a plain hostname, and no refusal repeats the value (#658).
func TestPingURL_NeverEchoesTheAddress(t *testing.T) {
	for _, addr := range []string{
		"SECRETTOK@127.0.0.1:3000",
		"user:SECRETTOK@127.0.0.1:3000",
		"SECRETTOK",
		"127.0.0.1:SECRETTOK",
		"SECRETTOK\x1b[2J:3000",
		"[SECRETTOK%eth0]:3000",
		"127.0.0.1:0",
		"127.0.0.1:+3000",
		"127.0.0.1:03000",
	} {
		got, err := pingURL(addr)
		if err == nil {
			t.Errorf("pingURL(%q) = %q with no error, want a refusal", addr, got)
			continue
		}
		if strings.Contains(err.Error(), "SECRETTOK") || strings.Contains(err.Error(), "\x1b") {
			t.Errorf("pingURL(%q) error %q echoes the address", addr, err)
		}
	}
	if got, err := pingURL("tasks-mcp_1.internal:3000"); err != nil || got != "http://tasks-mcp_1.internal:3000/mcp" {
		t.Errorf("pingURL(plain hostname) = %q, %v; want it accepted", got, err)
	}
}

// TestRunMCPPing_UnreachableDoesNotEchoTheURL: net/http's *url.Error renders
// the whole URL; the ping message names it only as pingURLLabel.
func TestRunMCPPing_UnreachableDoesNotEchoTheURL(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens here now

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	err = runMCPPing(cmd, addr)
	if err == nil {
		t.Fatal("--ping against a closed port = nil, want a failure")
	}
	if strings.Contains(err.Error(), "http://") || strings.Contains(err.Error(), "/mcp") {
		t.Errorf("error %q echoes the probe URL", err)
	}
	if !strings.Contains(err.Error(), pingURLLabel) {
		t.Errorf("error %q, want it to name %q", err, pingURLLabel)
	}
}

// TestListenCause_DropsTheAddress: net's listen errors repeat the address,
// which is the same --http value --ping refuses to print.
func TestListenCause_DropsTheAddress(t *testing.T) {
	for _, addr := range []string{"SECRETTOK:1:2", "[SECRETTOK:1"} {
		_, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
		if err == nil {
			t.Fatalf("listen %q succeeded, want a failure", addr)
		}
		if got := listenCause(err).Error(); strings.Contains(got, "SECRETTOK") {
			t.Errorf("listenCause(%v) = %q, echoes the address", err, got)
		}
	}
}

// TestMCPServerConfig_RecordsGoToStderr: on stdio, stdout is the JSON-RPC
// transport, so a close record written there would corrupt the session it
// reports on. The record writer is the command's stderr on both transports.
func TestMCPServerConfig_RecordsGoToStderr(t *testing.T) {
	// The stdio config also appends to the close log, which must land under
	// the test's own config directory.
	isolateTasksConfigDir(t)
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	for _, httpAddr := range []string{"", ":3000"} {
		stdout.Reset()
		stderr.Reset()
		cfg := mcpServerConfig(cmd, httpAddr, "some-entry", "board.example")
		rec := tasks.CloseRecord{TaskID: 1, Surface: tasks.SurfaceMCP, Evidence: "x", Outcome: tasks.CloseOutcomeClosed}
		if err := tasks.WriteCloseRecord(cfg.Records, rec); err != nil {
			t.Fatalf("--http %q: WriteCloseRecord: %v", httpAddr, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("--http %q: a close record reached stdout: %s", httpAddr, stdout.String())
		}
		if !strings.Contains(stderr.String(), `"outcome":"closed"`) {
			t.Errorf("--http %q: the close record did not reach stderr: %q", httpAddr, stderr.String())
		}
	}
}

// TestMCPServerConfig_NamesTheCredentialSourceNotTheCredential: a record says
// where the token came from — the keychain entry's name on stdio, the fixed
// word for a mounted file on HTTP — and the host it was sent to.
func TestMCPServerConfig_NamesTheCredentialSourceNotTheCredential(t *testing.T) {
	cmd := &cobra.Command{}

	stdio := mcpServerConfig(cmd, "", "some-entry", "board.example")
	if stdio.CredentialSource != "some-entry" || stdio.Host != "board.example" || stdio.DefaultClientName != "forgectl (stdio)" {
		t.Errorf("stdio config = %+v, want the keychain service name, the host, and the stdio client name", stdio)
	}

	// The keychain service flag has a default, so it is non-empty on HTTP
	// too. It names an entry the HTTP transport never read.
	overHTTP := mcpServerConfig(cmd, ":3000", "some-entry", "board.example")
	if tasks.CredentialSourceTokenFile != "token-file" {
		t.Errorf("CredentialSourceTokenFile = %q, want the literal token-file", tasks.CredentialSourceTokenFile)
	}
	if overHTTP.CredentialSource != tasks.CredentialSourceTokenFile {
		t.Errorf("http credential source = %q, want %q", overHTTP.CredentialSource, tasks.CredentialSourceTokenFile)
	}
	if overHTTP.Host != "board.example" || overHTTP.DefaultClientName != "forgectl (http)" {
		t.Errorf("http config = %+v, want the host and the http client name", overHTTP)
	}
}

// TestTasksMCPHelp_NamesAllSevenTools: the help is where an operator learns
// what the server exposes before granting it a credential.
func TestTasksMCPHelp_NamesAllSevenTools(t *testing.T) {
	host, service := "board.example", "some-entry"
	long := newTasksMCPCmd(module.Deps{}, &host, &service).Long
	if !strings.Contains(long, "Seven tools") || strings.Contains(long, "Six tools") {
		t.Errorf("the help does not count seven tools:\n%s", long)
	}
	for _, tool := range []string{
		"list_projects", "list_tasks", "get_task", "ready_tasks", "create_task", "add_comment", "complete_task",
	} {
		if !strings.Contains(long, tool) {
			t.Errorf("the help does not name %s", tool)
		}
	}
	if !strings.Contains(long, "stderr") || !strings.Contains(long, "close record") {
		t.Errorf("the help does not say where close records go:\n%s", long)
	}
}
