// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// mcpShutdownTimeout bounds the graceful HTTP shutdown after a signal. A
// container stop sends SIGTERM and then SIGKILLs after its own grace period;
// finishing in-flight requests inside that window is the whole ask.
const mcpShutdownTimeout = 5 * time.Second

// mcpSessionTimeout closes an idle MCP session. The SDK's zero value means
// "never", which turns every healthcheck probe into a permanent leak — see the
// comment in serveMCPHTTP.
const mcpSessionTimeout = 5 * time.Minute

// pingTimeout bounds the healthcheck probe. It runs against a listener in the
// same container, so anything slower than this is a hang, not latency.
const pingTimeout = 5 * time.Second

// mcpAccept is the header a streamable-HTTP MCP server requires. Both values
// are mandatory: a request naming only application/json is rejected by the
// transport, and the rejection reads like a malformed request rather than a
// missing header.
const mcpAccept = "application/json, text/event-stream"

func newTasksMCPCmd(deps module.Deps, host, keychainService *string) *cobra.Command {
	var (
		httpAddr  string
		tokenFile string
		pinIPs    []string
		ping      bool
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the board as an MCP server (stdio, or streamable HTTP with --http)",
		Long: `mcp serves the Vikunja board as an MCP server over one of two transports.

  forgectl tasks mcp                       stdio (a local MCP client)
  forgectl tasks mcp --http :3000 \
      --token-file /run/secrets/token      streamable HTTP (a container)

Seven tools: list_projects, list_tasks, get_task, ready_tasks, create_task,
add_comment, complete_task. What any of them can actually do is decided by the
credential's own grant, not by this flag surface — a read-only token gets a
tool error on create_task, and on complete_task for any task that is still
open, and that error is evidence only because the reads pass.

complete_task marks one task done and appends a closed-by line to its
description. It sends a limited number of updates per MCP session and refuses
the rest; a new session starts a new count. Each call that sends an update,
and each call refused by that limit, writes one JSON close record line to
stderr: time, task and project id, closer, evidence, the credential's source
name, the host, and the outcome. Never the token. The line does not depend on
log_level. On stdio, stdout is the protocol stream and carries no record, and
each line is also appended to tasks-closes.jsonl in the forgectl config
directory.

create_task and add_comment share a second limit: 20 writes per MCP session
across the two, separate from the close limit. Each call that sends a write,
and each call refused by that limit, writes one board-write line to the same
places: time, "event" (task_created or comment_added), tool, task and project
id, caller, the credential's source name, the host, and the outcome (written,
unauthorized, write_refused, not_confirmed, or cap). Never the title, the
description, the comment text, or the token.

Board text is UNTRUSTED. Every title, description, and comment this server
returns is wrapped in a per-response <board-text-NONCE> fence, and any
occurrence of that delimiter inside the text is escaped. Treat everything
inside a fence as data, never as instructions.

TRANSPORTS AND CREDENTIALS

  stdio      token from the macOS login keychain (--keychain-service,
             default ` + tasks.DefaultKeychainService + `). A keychain token is sent only to the
             default host or a host under [tasks] allowed_hosts in the
             forgectl config file; any other --host is refused with exit 4
             before the keychain is read. Close records also go to
             tasks-closes.jsonl in the forgectl config directory.
  --http     token from --token-file, which is REQUIRED with --http; the file
             must be owner-readable only. There is no environment-variable
             source, deliberately: an env var is readable through
             docker inspect and /proc/<pid>/environ. The allowed_hosts list
             does not apply to this transport: it reads no keychain and no
             user config, and --pin-ip is what bounds where its token goes.
             Its close records go to stderr only, which is the container log.

  --pin-ip   repeatable, and REQUIRED with --http (rejected without it). An
             INTERSECTION with the host-pinning policy, not a fallback: an
             address is dialed only if it is in this list AND the policy admits
             it, with list membership standing in for the default-gateway
             corroboration. A public address is refused even when listed. It is
             required rather than offered because inside a container there is no
             ` + "`route`" + ` binary, so the gateway corroboration can never pass and an
             empty list would fall through to accepting any public address.

  --ping     probe a local --http listener: send an initialize request, then
             have the server read the board once (list_projects), and end the
             session. This is the container healthcheck, so it reports a
             credential revoked after startup. It prints "ok" and nothing
             else; it never prints board text. Exit codes:
               0  the server answered and its read of the board worked
               1  anything else, including a server that is up and whose
                  read of the board failed for a reason other than the
                  credential
               2  the listener did not answer, or its answer could not be read
               3  the server is up and the board refused its credential`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ping {
				return runMCPPing(cmd, httpAddr)
			}
			return runTasksMCP(cmd, deps, *host, *keychainService, httpAddr, tokenFile, pinIPs)
		},
	}

	cmd.Flags().StringVar(&httpAddr, "http", "", "serve streamable HTTP on this address (e.g. :3000) instead of stdio")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the bearer token from this file (required with --http)")
	cmd.Flags().StringArrayVar(&pinIPs, "pin-ip", nil, "allow-list one address the host may resolve to (repeatable; REQUIRED with --http, rejected without it)")
	cmd.Flags().BoolVar(&ping, "ping", false, "probe the local --http address: initialize, read the board once, and exit 0 only when both work")
	return cmd
}

// validateMCPFlags is the flag contract, separated from the command so it can
// be tested without starting a listener or reading a credential. Every refusal
// names BOTH flags involved, because the fix is always a pair.
func validateMCPFlags(httpAddr, tokenFile string, pinIPs []string) error {
	if httpAddr == "" {
		if tokenFile != "" {
			return fmt.Errorf("--token-file is only used with --http; the stdio transport reads the token from the login keychain (--keychain-service)")
		}
		if len(pinIPs) > 0 {
			return fmt.Errorf("--pin-ip is only accepted with --http; on stdio the default-gateway corroboration is the host pin")
		}
		return nil
	}
	if tokenFile == "" {
		return fmt.Errorf("--http requires --token-file: the HTTP transport has no keychain to read, and there is no environment-variable token source by design")
	}
	// --pin-ip is REQUIRED with --http, not merely accepted there.
	//
	// Inside a container the base host-pin policy has no live acceptance arm
	// left except "public: allowed". There is no `route` binary, so the
	// default-gateway lookup returns "" and the private-range arm can never
	// pass — which means that with an empty pin list the only way the client
	// proceeds is by accepting whatever public address the resolver named,
	// with TLS as the sole remaining control. That is precisely the posture
	// the pin exists to replace, and it would be reached by OMITTING a flag
	// rather than by setting one.
	if len(pinIPs) == 0 {
		return fmt.Errorf("--http requires at least one --pin-ip: inside a container the default-gateway corroboration cannot succeed, " +
			"so without a pin list the host policy would fall through to accepting any public address the resolver returns")
	}
	return nil
}

func runTasksMCP(
	cmd *cobra.Command,
	deps module.Deps,
	host, keychainService, httpAddr, tokenFile string,
	pinIPs []string,
) error {
	if err := validateMCPFlags(httpAddr, tokenFile, pinIPs); err != nil {
		return WithExitCode(fmt.Errorf("tasks mcp: %w", err), 1)
	}
	ctx := cmd.Context()

	pins, err := tasks.ParsePinList(pinIPs)
	if err != nil {
		return WithExitCode(err, 1)
	}

	// The credential is read BEFORE the listener opens. A server that binds a
	// port and only then discovers its token file is unreadable is a container
	// that reports healthy and answers every tool call with an auth failure.
	var token tasks.Token
	if httpAddr == "" {
		// The stdio transport reads the keychain, so the allowed-host rule
		// applies, and is checked before the read.
		token, err = readTasksKeychainToken(cmd, deps.Runner, tasksKeychainFlag, keychainService, host, deps.Cfg.Tasks.AllowedHosts)
	} else {
		// The HTTP transport is NOT subject to the allowed-host rule. Its
		// token comes from a mounted file, in a container that has no user
		// config file to hold a host list; where that token may go is bounded
		// by the --pin-ip list validateMCPFlags requires, and a token from
		// ReadTokenFile carries no host restriction of its own.
		token, err = tasks.ReadTokenFile(tokenFile)
	}
	if err != nil {
		return tasksExitError(err)
	}

	client, err := tasks.NewClientWithPins(ctx, deps.Runner, host, token, pins)
	if err != nil {
		return tasksExitError(err)
	}
	// Prove the host is a Vikunja API before serving a single tool call. The
	// estate's reverse proxy answers an unmatched host with a 200 and a
	// landing page, so without this the failure surfaces hours later as a
	// confusing tool error rather than a refusal to start.
	if err := client.AssertVikunja(ctx); err != nil {
		return tasksExitError(err)
	}

	server := tasks.NewMCPServer(client, mcpServerConfig(cmd, httpAddr, keychainService, host))
	if httpAddr == "" {
		// stdout is the transport on stdio. Anything written there that is
		// not a JSON-RPC frame corrupts the session, which is why nothing in
		// this branch prints.
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	return serveMCPHTTP(cmd, server, httpAddr)
}

// mcpServerConfig is what the server is told about its own launch: the
// fallback client name for a trailer, where close records go, and the two
// facts a record carries about the credential — its source's name and the
// host it is sent to.
//
// Records go to stderr on both transports, never stdout: on stdio stdout is
// the JSON-RPC transport, and a record written there would corrupt the session
// it describes. On stdio they are also appended to the close log, because the
// server's stderr belongs to the MCP client that started it and is gone when
// that client is. Over HTTP stderr is the container log, which is that
// transport's durable record, and no file is written.
//
// The credential source is a NAME, never the credential. On stdio it is the
// keychain service the token was read from. On HTTP it is the fixed word for
// a mounted file: --keychain-service still holds its default there, and it
// names an entry this transport never read.
func mcpServerConfig(cmd *cobra.Command, httpAddr, keychainService, host string) tasks.MCPConfig {
	cfg := tasks.MCPConfig{
		DefaultClientName: "forgectl (stdio)",
		Records:           tasksCloseRecordWriter(cmd.ErrOrStderr()),
		CredentialSource:  keychainService,
		Host:              host,
	}
	if httpAddr != "" {
		cfg.DefaultClientName = "forgectl (http)"
		cfg.Records = cmd.ErrOrStderr()
		cfg.CredentialSource = tasks.CredentialSourceTokenFile
	}
	return cfg
}

func serveMCPHTTP(cmd *cobra.Command, server *mcp.Server, addr string) error {
	// SessionTimeout is NOT optional here, and the zero value is the trap: the
	// SDK never closes an idle session when it is unset. Every `--ping` opens
	// a session — an `initialize`, then one tool call — and a ping that dies
	// before its DELETE leaves that session and a goroutine behind. At a 30s
	// Docker healthcheck interval that is ~2,880 sessions a day in a
	// container meant to run for months. Measured during review: 50
	// sequential initialize posts against a default-options handler left 52
	// goroutines behind.
	//
	// It also bounds the honest case — a real client that disconnects without
	// a DELETE — which is why the timeout is the fix and the DELETE in
	// runMCPPing is only the tidy-up.
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout},
	)

	// Cross-origin protection is applied EXPLICITLY, as middleware. The SDK's
	// own guard is conditional in two ways that both fail open here: it
	// applies a zero-value protection only when an environment variable is
	// set, and its automatic DNS-rebinding defence engages only when the LOCAL
	// address is loopback — which a container binding :3000 is not. So the
	// default is a handler with no origin check at all, on a process holding a
	// write credential. The equivalent StreamableHTTPOptions field is
	// deprecated in favour of exactly this wrapping.
	//
	// State the limit honestly: this stops a browser on the operator's machine
	// from being walked into calling create_task, and it stops nothing else.
	// It is NOT authentication. Any client that can open a TCP connection to
	// this port still reaches every tool, so the network position (a
	// two-member network with the gateway as its only other peer) and the
	// gateway's own tool authorization remain the actual access control.
	guarded := http.NewCrossOriginProtection().Handler(handler)

	mux := http.NewServeMux()
	mux.Handle("/mcp", guarded)
	mux.Handle("/mcp/", guarded)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout falls back to ReadTimeout when unset, and ReadTimeout is
		// unset too — so an idle keep-alive connection would never be closed,
		// and a listener meant to run for months accumulates connections
		// nothing reclaims.
		//
		// ReadTimeout and WriteTimeout stay unset ON PURPOSE: the streamable
		// transport holds a long-lived SSE response open, and a WriteTimeout
		// would cut it mid-stream at a wall-clock deadline that has nothing to
		// do with whether the connection is healthy.
		IdleTimeout: 2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind BEFORE the serving goroutine starts, so a port already in use is
	// returned to the caller as a startup failure. Binding inside the
	// goroutine makes an unusable address indistinguishable from a healthy
	// start until the first request arrives.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		// Neither addr nor net's own text (which repeats it) is echoed: the
		// value is the same --http string --ping refuses to print (#658).
		return fmt.Errorf("tasks mcp: cannot listen on the --http address: %w", listenCause(err))
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "forgectl tasks mcp: serving streamable HTTP on %s/mcp\n", ln.Addr()) //nolint:errcheck // best-effort startup notice

	errCh := make(chan error, 1)
	go func() {
		serveErr := httpServer.Serve(ln)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		errCh <- serveErr
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), mcpShutdownTimeout)
		defer cancel()
		// A connected streamable-HTTP client holds an open SSE stream that
		// never returns to idle, so Shutdown ALWAYS burns the full budget and
		// returns context.DeadlineExceeded whenever anyone is attached.
		// Returning that error made every `docker stop` with a live client
		// exit non-zero and print an error, for a shutdown that worked. Past
		// the grace period the answer is to force-close: SIGKILL is what comes
		// next anyway.
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
		}
		return nil
	}
}

// pingReadTimeout bounds the ping's one tool call. That call makes the
// server read the board, and the server's own request deadline (10s in
// internal/tasks) is longer than pingTimeout; the ping waits past it so that a
// slow board is reported by the server as a failed read, not by the ping as a
// listener that did not answer.
const pingReadTimeout = 15 * time.Second

// pingProtocolVersion is the MCP protocol version the ping asks for, and the
// value of the Mcp-Protocol-Version header on its later requests.
const pingProtocolVersion = "2025-06-18"

// runMCPPing is the container healthcheck: POST an initialize request to the
// local --http address, then have the server read the board once, and exit 0
// only when both succeed.
//
// It asserts the result, not the status code. A streamable-HTTP server that is
// up but broken can still answer 200 with a JSON-RPC error, and a healthcheck
// that could not go red on that is not a healthcheck.
//
// The read is the point of the second step. The server checks its credential
// once, at startup, and then serves; a token revoked after that leaves an
// initialize answering perfectly while every tool call fails. list_projects
// is the cheapest read that uses the credential. Its result is board text and
// is never printed: the ping looks only at whether it was a tool error, and,
// when it was, whether the server's own text names a refused credential.
//
// Exit codes: 0 healthy; 2 the listener did not answer, or its answer could
// not be read; 3 the server is up and the board refused its credential; 1
// anything else, including a server that is up and whose read of the board
// failed for another reason.
func runMCPPing(cmd *cobra.Command, httpAddr string) error {
	if httpAddr == "" {
		return WithExitCode(fmt.Errorf("tasks mcp --ping requires --http <addr> — it probes a local listener, and without the address there is nothing to probe"), 1)
	}
	url, err := pingURL(httpAddr)
	if err != nil {
		return WithExitCode(err, 1)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + pingProtocolVersion + `","capabilities":{},"clientInfo":{"name":"forgectl-ping","version":"0"}}}`

	ctx, cancel := context.WithTimeout(cmd.Context(), pingTimeout)
	defer cancel()
	status, header, raw, readErr, err := pingPost(ctx, url, "", body)
	if err != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s did not answer: %w", pingURLLabel, pingCause(err)), exitTasksUnreachable)
	}
	if status < 200 || status >= 300 {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered %d", pingURLLabel, status), 1)
	}
	// Release the session this probe just created, on every path from here.
	// The server also expires idle sessions (mcpSessionTimeout), which is the
	// real defence — but a healthcheck running every 30s for months should not
	// lean on a timeout to clean up after itself, and the session is now held
	// across a tool call, not only an initialize.
	//
	// It gets its OWN deadline rather than sharing ctx: if the probe consumed
	// most of its budget, ctx can already be expired here, and net/http would
	// then refuse to send the DELETE before it left the process — leaving the
	// session behind in exactly the runs where cleanup matters most.
	sessionID := header.Get("Mcp-Session-Id")
	defer releasePingSession(cmd.Context(), url, sessionID)

	// The read error is kept rather than discarded. Dropping it makes a body
	// that FAILED TO ARRIVE byte-identical to one that arrived malformed, and
	// the verdict below would then blame the server's payload for a transport
	// fault — a confident, wrong statement about the service being probed.
	if readErr != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered %d but the body could not be read: %w", pingURLLabel, status, pingCause(readErr)), exitTasksUnreachable)
	}
	if !hasJSONRPCResult(raw) {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered %d but the body carries no JSON-RPC result", pingURLLabel, status), 1)
	}

	// The protocol requires this notification before any other request; a
	// server may refuse a tool call that arrives without it.
	status, _, _, _, err = pingPost(ctx, url, sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s did not answer the initialized notification: %w", pingURLLabel, pingCause(err)), exitTasksUnreachable)
	}
	if status < 200 || status >= 300 {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered %d to the initialized notification", pingURLLabel, status), 1)
	}

	readCtx, cancelRead := context.WithTimeout(cmd.Context(), pingReadTimeout)
	defer cancelRead()
	status, _, raw, readErr, err = pingPost(readCtx, url, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`)
	if err != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered initialize but not the read of the board: %w", pingURLLabel, pingCause(err)), exitTasksUnreachable)
	}
	if readErr != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered the read of the board with %d but the body could not be read: %w", pingURLLabel, status, pingCause(readErr)), exitTasksUnreachable)
	}
	switch pingReadVerdict(status, raw) {
	case pingReadRefused:
		return WithExitCode(errors.New("tasks mcp --ping: the server is up, and the board refused its credential: the token is revoked, expired, or no longer allowed to read"), exitTasksUnauthorized)
	case pingReadFailed:
		return WithExitCode(errors.New("tasks mcp --ping: the server is up, and its read of the board failed"), 1)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok") //nolint:errcheck // best-effort healthcheck output
	return nil
}

// pingPost sends one JSON-RPC message to url and returns the status, the
// response headers, and up to 1 MiB of the body. err is a request that got no
// answer; readErr is an answer whose body did not fully arrive. sessionID,
// when set, goes in Mcp-Session-Id, with the protocol version the session was
// opened with.
func pingPost(ctx context.Context, url, sessionID, body string) (status int, header http.Header, raw []byte, readErr, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, nil, nil, nil, err
	}
	// Both Accept values are mandatory for a streamable-HTTP server. Naming
	// only application/json is rejected by the transport, and the rejection
	// reads like a malformed request rather than a missing header.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("Mcp-Protocol-Version", pingProtocolVersion)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, raw, readErr, nil
}

// pingRead is the verdict on the ping's read of the board.
type pingRead int

const (
	pingReadOK pingRead = iota
	pingReadRefused
	pingReadFailed
)

// pingReadVerdict reads the server's answer to the ping's list_projects call.
//
// Only a JSON-RPC result that is not a tool error is healthy. A tool error is
// a refused credential when its text carries the tasks client's own
// unauthorized sentence — server text this binary wrote, not board text — and
// a failed read otherwise. Anything else (a non-2xx status, a JSON-RPC error,
// a body with no result) is a failed read: the server is up, and it could not
// do the one thing the healthcheck asked of it.
func pingReadVerdict(status int, raw []byte) pingRead {
	if status < 200 || status >= 300 {
		return pingReadFailed
	}
	result, ok := jsonRPCResult(raw)
	if !ok {
		return pingReadFailed
	}
	var call struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &call); err != nil {
		return pingReadFailed
	}
	if !call.IsError {
		return pingReadOK
	}
	// Anchored on the exact text list_projects writes for an auth failure. A
	// substring match would also fire on transport error text quoted later in
	// the same message, which a TLS peer can shape through its certificate.
	refused := listProjectsRefusedPrefix + tasks.ErrUnauthorized.Error()
	for _, c := range call.Content {
		if strings.HasPrefix(c.Text, refused) {
			return pingReadRefused
		}
	}
	return pingReadFailed
}

// listProjectsRefusedPrefix is how the list_projects tool error begins. The ping
// reads the refusal sentence only straight after it.
const listProjectsRefusedPrefix = "could not list projects: "

// releasePingSession best-effort DELETEs the session the probe opened. Every
// failure here is ignored on purpose: this is tidy-up, and a healthcheck that
// went red because it could not clean up would report the service unhealthy
// for a reason that has nothing to do with the service.
func releasePingSession(parent context.Context, url, sessionID string) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := (&http.Client{Timeout: pingTimeout}).Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// pingURL turns a listen address into the URL to probe. A bare or wildcard
// bind (":3000", "0.0.0.0:3000") is probed on loopback: the healthcheck runs
// inside the container, and dialing 0.0.0.0 is not portable.
//
// An unparseable address is an ERROR, not a concatenation. Falling back to
// "http://127.0.0.1" + addr turns `--http 3000` (a missing colon) into
// http://127.0.0.13000/mcp, which fails to connect — so the healthcheck
// reports the service unreachable when the real fault is a typo in its own
// argument, and the exit code claims a network verdict about a parse failure.
//
// The address and the URL are never echoed whole (#658). A URL built from the
// address can carry credentials — `tok@127.0.0.1:3000` splits into host
// "tok@127.0.0.1", and net/http then sends "tok" as userinfo and prints it in
// its own errors (it masks only a password). So the host must be an IP or a
// plain hostname (config.PlainServiceHostname: the one hostname grammar, in
// the form that also admits the '_' of a compose service name), which cannot
// carry userinfo; this function's refusals repeat
// none of the value; and later messages say pingURLLabel instead of the URL.
// A later message can still name host:port through pingCause, which keeps a
// dial or DNS error. That is bounded by the checks here: a validated host and
// a numeric port.
func pingURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// SplitHostPort's error repeats addr, so it is not wrapped.
		return "", errors.New("tasks mcp --ping: the --http value is not a host:port address (a bare port needs its colon, as in :3000)")
	}
	// The Itoa round trip refuses what Atoi tolerates: a leading '+' or '-',
	// and leading zeros.
	if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", errors.New("tasks mcp --ping: the --http port is not a number from 1 to 65535")
	}
	if host != "" && net.ParseIP(host) == nil && !config.PlainServiceHostname(host) {
		return "", errors.New("tasks mcp --ping: the --http host is not an IP address or a plain hostname (letters, digits, '.', '-', '_')")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/mcp", nil
}

// pingURLLabel stands in for the probe URL in every --ping message (#658).
const pingURLLabel = "the configured tasks URL"

// pingCause drops net/http's *url.Error layer, whose text is the method and
// the full URL, and keeps what it wraps (a dial or timeout error naming at
// most host:port, which pingURL has already bounded).
func pingCause(err error) error {
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// listenCause reduces a net.Listen failure to the part that does not repeat
// the address: the bare reason of an address or DNS error, or the OS error
// under a *net.OpError ("address already in use").
func listenCause(err error) error {
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) {
		return termsafe.Categorical(addrErr.Err, err)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return termsafe.Categorical(dnsErr.Err, err)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err
	}
	return err
}

// hasJSONRPCResult reports whether raw carries a JSON-RPC response with a
// `result` member. The streamable transport may answer with SSE framing, so
// each line's `data:` payload is considered as well as the whole body.
func hasJSONRPCResult(raw []byte) bool {
	_, ok := jsonRPCResult(raw)
	return ok
}

// jsonRPCResult returns the `result` member of the JSON-RPC response in raw,
// whole or in SSE framing, as hasJSONRPCResult finds it.
func jsonRPCResult(raw []byte) (json.RawMessage, bool) {
	if result, ok := jsonResult(raw); ok {
		return result, true
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if result, ok := jsonResult(bytes.TrimSpace(payload)); ok {
				return result, true
			}
		}
	}
	return nil, false
}

func jsonResult(raw []byte) (json.RawMessage, bool) {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		return nil, false
	}
	if _, isError := frame["error"]; isError {
		return nil, false
	}
	result, ok := frame["result"]
	return result, ok
}
