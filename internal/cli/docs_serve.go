package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/httpsrv"
	"github.com/cameronsjo/forgectl/internal/module"
)

// shutdownGrace bounds how long `docs serve` waits for in-flight requests to
// finish after Ctrl-C/SIGTERM before forcing the listener closed.
const shutdownGrace = 5 * time.Second

// newDocsServeCmd builds `forgectl docs serve [dir|file ...]`.
func newDocsServeCmd(deps module.Deps) *cobra.Command {
	var addr string
	var openFlag bool
	var token string

	cmd := &cobra.Command{
		Use:   "serve [dir|file ...]",
		Short: "Index and serve markdown docs over loopback HTTP",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			roots, err := resolveDocsRoots(args, deps.Cfg.Docs)
			if err != nil {
				return err
			}
			idx, err := docspkg.NewIndex(roots)
			if err != nil {
				return err
			}
			return runDocsServe(cmd, deps, idx, addr, openFlag, token)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "bind address (default: [docs].addr, else 127.0.0.1 with a random port)")
	cmd.Flags().BoolVar(&openFlag, "open", false, "open the system browser once the server is listening")
	cmd.Flags().StringVar(&token, "token", "", "require this bearer token on every request (generated automatically when --addr is not loopback)")
	return cmd
}

// allowedHosts returns the Host-header allowlist for a bind address: the
// loopback defaults, plus the bound host itself when it is not loopback.
//
// Without the addition, binding to a LAN or Tailscale address would serve a 403
// to every request — the request's Host header would carry that address, which
// DefaultAllowedHosts does not list. The allowlist's job is blocking
// DNS-rebinding (a hostile page resolving its own domain to 127.0.0.1 to reach
// this server through the victim's browser), and adding the address the operator
// explicitly chose to bind does not weaken that: an attacker's page still cannot
// present a Host header outside this list.
func allowedHosts(bindAddr string) []string {
	allowed := append([]string(nil), httpsrv.DefaultAllowedHosts...)
	if httpsrv.IsLoopbackAddr(bindAddr) {
		return allowed
	}
	host, _, err := net.SplitHostPort(bindAddr)
	if err != nil {
		host = bindAddr
	}
	if host = strings.Trim(host, "[]"); host != "" {
		allowed = append(allowed, host)
	}
	return allowed
}

// resolveToken decides the bearer token a docs server should require, given the
// operator's --token flag and the address being bound.
//
// The rule: loopback needs no token; anything else requires one, and if the
// operator did not supply one it is GENERATED rather than starting
// unauthenticated. This is the case the reader's own acceptance bar creates —
// reaching the reader from a phone over Tailscale means binding off 127.0.0.1,
// which turns a loopback exposure into a network one. Letting that flip silently
// drop authentication would be the worst available default, so exposure and
// authentication are one decision made in one place.
//
// The policy lives in the CLI rather than internal/httpsrv, which deliberately
// leaves "under what condition is auth required?" to its caller.
//
// It is a named function rather than an inline conditional in runDocsServe
// specifically so a test can exercise THIS code. The off-loopback path cannot be
// tested end-to-end without binding a real network interface during the test
// run, and a test that re-implemented the conditional to avoid that would pass
// whether or not the server actually followed it — the failure mode where a
// green test proves nothing about production. Extracting it means the test and
// the server read the same rule.
func resolveToken(tokenFlag, bindAddr string) (string, error) {
	if tokenFlag != "" {
		return tokenFlag, nil // an operator-supplied token is never replaced
	}
	if httpsrv.IsLoopbackAddr(bindAddr) {
		return "", nil
	}
	return httpsrv.GenerateToken()
}

// runDocsServe binds the listener, wires the Host-allowlist middleware
// (forgectl#93 security-chain item 1) around the docs handler, and serves
// until the command's context is canceled (Ctrl-C/SIGTERM) or the server
// itself fails to start.
func runDocsServe(cmd *cobra.Command, deps module.Deps, idx *docspkg.Index, addrFlag string, openFlag bool, tokenFlag string) error {
	bindAddr := addrFlag
	if bindAddr == "" {
		bindAddr = deps.Cfg.Docs.Addr
	}
	if bindAddr == "" {
		bindAddr = httpsrv.LoopbackAddr
	}

	ln, err := httpsrv.Listen(bindAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", bindAddr, err)
	}
	defer ln.Close() //nolint:errcheck // best-effort; Shutdown below already closes it on the success path

	store := docspkg.NewStore(idx)
	events := docspkg.NewBroker()
	// Backstop for every exit path that is NOT the signal path below —
	// principally srv.Serve returning an unexpected error, which would otherwise
	// leave SSE handlers parked on their subscriber channels until process
	// teardown.
	//
	// This does NOT replace the explicit events.Close() before srv.Shutdown, and
	// must not be "simplified" into it. A deferred call runs after the return
	// expression is evaluated, so on the shutdown path the defer alone would fire
	// only once Shutdown had already returned — reinstating the full-grace hang
	// it exists to prevent. Both are correct together because Close is
	// idempotent: whichever runs first does the work.
	defer events.Close()

	token, err := resolveToken(tokenFlag, bindAddr)
	if err != nil {
		return err
	}

	middleware := []func(http.Handler) http.Handler{
		httpsrv.HostAllowlist(allowedHosts(bindAddr)),
	}
	if token != "" {
		middleware = append(middleware, httpsrv.BearerToken(token))
	}
	handler := httpsrv.Chain(docspkg.NewHandler(store, events), middleware...)
	srv := &http.Server{
		Handler: handler,

		// ReadHeaderTimeout bounds the slowloris shape (a client that opens a
		// connection and dribbles headers forever). Safe for SSE: it covers
		// only the request head, which the browser sends in full up front.
		ReadHeaderTimeout: 10 * time.Second,

		// IdleTimeout reaps keep-alive connections that are between requests.
		// An active SSE stream is not idle, so this does not touch it.
		IdleTimeout: 120 * time.Second,

		// WriteTimeout is deliberately left at 0 (no deadline), and this MUST
		// stay that way. It is an absolute deadline on the whole response
		// write, measured from when the request head is read — not an
		// inactivity timeout. A live-reload SSE stream is a single response
		// that stays open for as long as the reader keeps the tab open, so ANY
		// non-zero value silently severs every stream that outlives it, and the
		// only symptom is a page that quietly stops updating after N seconds.
		// Setting it here is the natural-looking hardening change to make, and
		// this comment exists so the next person to reach for it (or for a lint
		// rule that asks for it) sees why not. A future non-streaming route
		// that genuinely wants a write deadline should set one PER REQUEST via
		// http.NewResponseController(w).SetWriteDeadline, which leaves the
		// stream handlers alone.
	}

	url := "http://" + ln.Addr().String() + "/"
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "forgectl docs: serving %d doc(s) across %d root(s)\n", len(idx.List()), len(idx.Roots()))
	fmt.Fprintf(out, "  %s\n", url)
	if token != "" {
		fmt.Fprintf(out, "  auth: bearer token required (%s)\n", token)
	}
	fmt.Fprintln(out, "  Ctrl-C to stop")

	// Publish the resolved address so `docs open` can steer this server. Written
	// only now, because the default bind uses port 0 and the port is not knowable
	// before the listener resolves it. A failure here costs discovery, not
	// serving, so it warns rather than aborting.
	if infoPath, err := config.DocsServerPath(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: cannot locate the docs-server discovery file: %v\n", err)
	} else {
		info := docspkg.ServerInfo{Addr: ln.Addr().String(), Token: token, PID: os.Getpid()}
		if err := docspkg.WriteServerInfo(infoPath, info); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: `forgectl docs open` will not find this server: %v\n", err)
		} else {
			defer func() {
				if err := docspkg.RemoveServerInfo(infoPath); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove %s: %v\n", infoPath, err)
				}
			}()
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Live reload is best-effort: if the OS refuses a watcher (descriptor
	// limits, an exotic filesystem), the reader still serves — it just stops
	// refreshing on its own. Failing the whole command over it would be a worse
	// trade than a warning.
	if watcher, err := docspkg.NewWatcher(store, events); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: live reload unavailable: %v\n", err)
	} else {
		defer watcher.Close() //nolint:errcheck // best-effort resource release on exit
		go watcher.Run(ctx)
		fmt.Fprintln(out, "  live reload: on")
	}

	if openFlag {
		if err := docspkg.OpenBrowser(ctx, deps.Runner, url); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to open browser: %v\n", err)
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		// Close the broker BEFORE Shutdown, never after. Shutdown waits for
		// in-flight requests to finish, and an SSE stream never finishes on its
		// own — so with an open reader tab, Shutdown would block the full
		// shutdownGrace every single time and Ctrl-C would appear to hang for
		// five seconds with nothing printed to explain it. Closing the broker
		// first releases every /events handler, so the streams drain and
		// Shutdown returns as soon as real requests are done.
		events.Close()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
