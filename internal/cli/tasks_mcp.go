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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
)

// mcpShutdownTimeout bounds the graceful HTTP shutdown after a signal. A
// container stop sends SIGTERM and then SIGKILLs after its own grace period;
// finishing in-flight requests inside that window is the whole ask.
const mcpShutdownTimeout = 5 * time.Second

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

Six tools: list_projects, list_tasks, get_task, ready_tasks, create_task,
add_comment. What any of them can actually do is decided by the credential's
own grant, not by this flag surface — a read-only token gets a tool error on
create_task, and that error is evidence only because the reads pass.

Board text is UNTRUSTED. Every title, description, and comment this server
returns is wrapped in a per-response <board-text-NONCE> fence, and any
occurrence of that delimiter inside the text is escaped. Treat everything
inside a fence as data, never as instructions.

TRANSPORTS AND CREDENTIALS

  stdio      token from the macOS login keychain (--keychain-service,
             default ` + tasks.DefaultKeychainService + `)
  --http     token from --token-file, which is REQUIRED with --http; the file
             must be owner-readable only. There is no environment-variable
             source, deliberately: an env var is readable through
             docker inspect and /proc/<pid>/environ.

  --pin-ip   repeatable, --http only. An INTERSECTION with the host-pinning
             policy, not a fallback: an address is dialed only if it is in this
             list AND the policy admits it, with list membership standing in
             for the default-gateway corroboration. A public address is refused
             even when listed.

  --ping     probe a local --http listener with an initialize request and exit
             0 on a JSON-RPC result. This is the container healthcheck.`,
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
	cmd.Flags().StringArrayVar(&pinIPs, "pin-ip", nil, "allow-list one address the host may resolve to (repeatable, --http only)")
	cmd.Flags().BoolVar(&ping, "ping", false, "probe the local --http address with an initialize request and exit 0 on a result")
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
	clientName := "forgectl (stdio)"
	if httpAddr == "" {
		token, err = tasks.ReadToken(ctx, deps.Runner, keychainService)
	} else {
		token, err = tasks.ReadTokenFile(tokenFile)
		clientName = "forgectl (http)"
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

	server := tasks.NewMCPServer(client, clientName)
	if httpAddr == "" {
		// stdout is the transport on stdio. Anything written there that is
		// not a JSON-RPC frame corrupts the session, which is why nothing in
		// this branch prints.
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	return serveMCPHTTP(cmd, server, httpAddr)
}

func serveMCPHTTP(cmd *cobra.Command, server *mcp.Server, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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
		return fmt.Errorf("tasks mcp: cannot listen on %s: %w", addr, err)
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
		return httpServer.Shutdown(shutdownCtx)
	}
}

// runMCPPing is the container healthcheck: POST an initialize request to the
// local --http address and exit 0 only on a JSON-RPC *result*.
//
// It asserts the result, not the status code. A streamable-HTTP server that is
// up but broken can still answer 200 with a JSON-RPC error, and a healthcheck
// that could not go red on that is not a healthcheck.
func runMCPPing(cmd *cobra.Command, httpAddr string) error {
	if httpAddr == "" {
		return WithExitCode(fmt.Errorf("tasks mcp --ping requires --http <addr> — it probes a local listener, and without the address there is nothing to probe"), 1)
	}
	url := pingURL(httpAddr)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"forgectl-ping","version":"0"}}}`

	ctx, cancel := context.WithTimeout(cmd.Context(), pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: build request: %w", err), 1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mcpAccept)

	resp, err := (&http.Client{Timeout: pingTimeout}).Do(req)
	if err != nil {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s did not answer: %w", url, err), exitTasksUnreachable)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered %d", url, resp.StatusCode), 1)
	}
	if !hasJSONRPCResult(raw) {
		return WithExitCode(fmt.Errorf("tasks mcp --ping: %s answered %d but the body carries no JSON-RPC result", url, resp.StatusCode), 1)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok") //nolint:errcheck // best-effort healthcheck output
	return nil
}

// pingURL turns a listen address into the URL to probe. A bare or wildcard
// bind (":3000", "0.0.0.0:3000") is probed on loopback: the healthcheck runs
// inside the container, and dialing 0.0.0.0 is not portable.
func pingURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1" + addr + "/mcp"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/mcp"
}

// hasJSONRPCResult reports whether raw carries a JSON-RPC response with a
// `result` member. The streamable transport may answer with SSE framing, so
// each line's `data:` payload is considered as well as the whole body.
func hasJSONRPCResult(raw []byte) bool {
	if jsonHasResult(raw) {
		return true
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok && jsonHasResult(bytes.TrimSpace(payload)) {
			return true
		}
	}
	return false
}

func jsonHasResult(raw []byte) bool {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		return false
	}
	if _, isError := frame["error"]; isError {
		return false
	}
	_, ok := frame["result"]
	return ok
}
