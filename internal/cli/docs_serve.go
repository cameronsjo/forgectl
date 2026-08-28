package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
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
//
// It MUST stay clear of net/http's own five-second rule: Server.Shutdown will
// not close a StateNew connection — one dialed but never used, which any
// client transport may leave behind speculatively — until it has sat there
// five seconds. A grace equal to that raced it exactly, and Ctrl-C exited
// non-zero with "context deadline exceeded" whenever such a connection
// existed. The forced close below is the real guarantee; this is the drain
// window, sized to clear net/http's rule rather than tie with it.
const shutdownGrace = 8 * time.Second

// newDocsServeCmd builds `forgectl docs serve [dir|file ...]`.
func newDocsServeCmd(deps module.Deps) *cobra.Command {
	var addr string
	var openFlag bool
	var tokenFile string

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
			return runDocsServe(cmd, deps, idx, addr, openFlag, tokenFile)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		if err != nil && err.Error() == "unknown flag: --token" {
			return errors.New("--token was removed because command-line values are visible to other processes; use --token-file instead")
		}
		return err
	})
	cmd.Flags().StringVar(&addr, "addr", "", "bind address (default: [docs].addr, else 127.0.0.1 with a random port)")
	cmd.Flags().BoolVar(&openFlag, "open", false, "open the system browser once the server is listening")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the bearer token from an absolute owner-only regular file")
	return cmd
}

// allowedHosts returns the Host-header allowlist for a bind address: the
// loopback defaults, plus the bound host itself when it is not loopback, plus
// the advertised host when discovery derived a different one.
//
// Without the bound-host addition, binding to a LAN or Tailscale address would
// serve a 403 to every request — the request's Host header would carry that
// address, which DefaultAllowedHosts does not list. The allowlist's job is
// blocking DNS-rebinding (a hostile page resolving its own domain to 127.0.0.1
// to reach this server through the victim's browser), and adding an address the
// operator explicitly chose to bind does not weaken that: an attacker's page
// still cannot present a Host header outside this list.
//
// The advertised host is added for the same reason one step later: discovery
// publishes the address a reader will connect to, so that address must be one
// the server accepts a Host header for, or every discovered request would 403.
func allowedHosts(bindAddr, advertisedAddr string) []string {
	allowed := append([]string(nil), httpsrv.DefaultAllowedHosts...)
	add := func(addr string) {
		if addr == "" {
			return
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		host = strings.Trim(host, "[]")
		if host == "" {
			return
		}
		for _, existing := range allowed {
			if existing == host {
				return
			}
		}
		allowed = append(allowed, host)
	}
	if !httpsrv.IsLoopbackAddr(bindAddr) {
		add(bindAddr)
	}
	add(advertisedAddr)
	return allowed
}

type docsTokenSource uint8

const (
	docsTokenNone docsTokenSource = iota
	docsTokenGenerated
	docsTokenFromFile
)

type resolvedDocsToken struct {
	value  string
	source docsTokenSource
}

func docsAuthStartupLine(token resolvedDocsToken) string {
	if token.value == "" {
		return ""
	}
	if token.source == docsTokenFromFile {
		return "  auth: bearer token required (from --token-file)"
	}
	return fmt.Sprintf("  auth: bearer token required (%s)", token.value)
}

// resolveDocsToken decides the bearer token a docs server should require,
// given the operator's --token-file path and the address being bound.
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
func resolveDocsToken(tokenFile, bindAddr string) (resolvedDocsToken, error) {
	if tokenFile != "" {
		token, err := acquireDocsTokenFile(tokenFile)
		if err != nil {
			return resolvedDocsToken{}, err
		}
		return resolvedDocsToken{value: token, source: docsTokenFromFile}, nil
	}
	if httpsrv.IsLoopbackAddr(bindAddr) {
		return resolvedDocsToken{}, nil
	}
	token, err := httpsrv.GenerateToken()
	if err != nil {
		return resolvedDocsToken{}, err
	}
	return resolvedDocsToken{value: token, source: docsTokenGenerated}, nil
}

// docsServeRuntime is every effect runDocsServe performs, passed BY VALUE.
//
// Not package-level test hooks: this command's tests run concurrently with each
// other and with real servers, and a mutable global would let one test observe
// another's fakes. Not nil-means-default fields either — a test that forgot one
// would silently exercise production behavior and report a pass.
type docsServeRuntime struct {
	listen      func(string) (net.Listener, error)
	serversDir  func() (string, error)
	newInfo     func(addr, token string) (docspkg.ServerInfo, error)
	probe       func(ctx context.Context, addr, generation string) error
	publish     func(dir string, info docspkg.ServerInfo) (docspkg.Publication, error)
	serve       func(*http.Server, net.Listener) error
	closeServer func(*http.Server) error
	shutdown    func(*http.Server, context.Context) error
	closeLease  func(*docspkg.ServerLease) error
}

func productionDocsServeRuntime() docsServeRuntime {
	return docsServeRuntime{
		listen:      httpsrv.Listen,
		serversDir:  config.DocsServersDir,
		newInfo:     docspkg.NewServerInfo,
		probe:       docspkg.ProbeServerGeneration,
		publish:     docspkg.PublishServerInfo,
		serve:       func(srv *http.Server, ln net.Listener) error { return srv.Serve(ln) },
		closeServer: func(srv *http.Server) error { return srv.Close() },
		shutdown:    func(srv *http.Server, ctx context.Context) error { return srv.Shutdown(ctx) },
		closeLease:  func(lease *docspkg.ServerLease) error { return lease.Close() },
	}
}

// runDocsServe is the production entry point. It does nothing but hand its
// arguments plus a fully populated runtime to the implementation.
func runDocsServe(cmd *cobra.Command, deps module.Deps, idx *docspkg.Index, addrFlag string, openFlag bool, tokenFile string) error {
	return runDocsServeWithRuntime(cmd, deps, idx, addrFlag, openFlag, tokenFile, productionDocsServeRuntime())
}

// runDocsServeWithRuntime binds the listener, wires the security middleware
// chain (forgectl#93 security-chain item 1, plus the cross-site rejecter
// forgectl#178 adds) around the docs handler, publishes a generation-owned
// discovery record, and serves until the command's context is canceled
// (Ctrl-C/SIGTERM) or the server itself fails.
//
// Chain order is deliberate: security headers, Host allowlist, cross-site
// rejection, discovery identity, then the bearer token when one is required.
// The two header checks are pure string comparisons and deny the requests that
// should never have arrived at all, so they run before the token check hashes
// anything. Putting authentication last also means a cross-site probe gets 403
// (this origin may not talk to me) rather than 401 (send me credentials), which
// is both the truer answer and the one that tells a hostile page less.
//
// The discovery identity endpoint sits behind the Host and cross-site gates but
// AHEAD of authentication, because a reader asking "are you the server I found?"
// has not yet decided whether to present a credential.
//
// SecurityHeaders sits OUTERMOST, ahead of every gate, because a rejected
// request never reaches the docs handler that would otherwise set them — so
// without this the 401s and 403s produced right here would be the only
// responses in the whole server carrying no CSP and no nosniff. The docs
// handler applies it again on the way past; setting the same fixed headers
// twice is idempotent, and having it in both places means neither the handler
// nor the chain depends on the other remembering.
func runDocsServeWithRuntime(
	cmd *cobra.Command,
	deps module.Deps,
	idx *docspkg.Index,
	addrFlag string,
	openFlag bool,
	tokenFile string,
	rt docsServeRuntime,
) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	bindAddr := addrFlag
	if bindAddr == "" {
		bindAddr = deps.Cfg.Docs.Addr
	}
	if bindAddr == "" {
		bindAddr = httpsrv.LoopbackAddr
	}
	resolvedToken, err := resolveDocsToken(tokenFile, bindAddr)
	if err != nil {
		return err
	}
	token := resolvedToken.value

	ln, err := rt.listen(bindAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", bindAddr, err)
	}
	defer ln.Close() //nolint:errcheck // best-effort; Shutdown below already closes it on the success path

	store := docspkg.NewStore(idx)
	events := docspkg.NewBroker()
	// Backstop for every exit path that is NOT the signal path below —
	// principally Serve returning an unexpected error, which would otherwise
	// leave SSE handlers parked on their subscriber channels until process
	// teardown.
	//
	// This does NOT replace the explicit events.Close() before Shutdown, and
	// must not be "simplified" into it. A deferred call runs after the return
	// expression is evaluated, so on the shutdown path the defer alone would fire
	// only once Shutdown had already returned — reinstating the full-grace hang
	// it exists to prevent. Both are correct together because Close is
	// idempotent: whichever runs first does the work.
	defer events.Close()

	// Discovery eligibility is decided BEFORE the middleware chain exists,
	// because an ineligible server must not carry an identity endpoint at all.
	// A route that answered for a generation no record will ever name would
	// imply a discoverability this server does not have.
	var (
		serversDir  string
		initialInfo docspkg.ServerInfo
		generation  atomic.Value
		eligible    bool
	)
	advertisedAddr, advertiseErr := docspkg.AdvertisedDocsAddr(ln.Addr())
	switch {
	case advertiseErr != nil:
		advertisedAddr = ""
		warnDocsServe(errOut, "warning: `forgectl docs open` will not find this server — its address cannot be published for discovery: %v", advertiseErr)
	default:
		dir, dirErr := rt.serversDir()
		if dirErr != nil {
			warnDocsServe(errOut, "warning: `forgectl docs open` will not find this server — its discovery directory cannot be located: %v", dirErr)
			break
		}
		info, infoErr := rt.newInfo(advertisedAddr, token)
		if infoErr != nil {
			// Before Serve, so this is a clean startup failure rather than a
			// half-started server: nothing is listening and nothing was published.
			events.Close()
			ln.Close() //nolint:errcheck // returning a failure; best-effort release
			return errDocsServeGeneration
		}
		serversDir, initialInfo, eligible = dir, info, true
		generation.Store(info.Generation)
	}

	middleware := []func(http.Handler) http.Handler{
		docspkg.SecurityHeaders,
		httpsrv.HostAllowlist(allowedHosts(bindAddr, advertisedAddr)),
		httpsrv.RejectCrossSite(),
	}
	if eligible {
		middleware = append(middleware, docspkg.DiscoveryIdentity(func() string {
			current, _ := generation.Load().(string)
			return current
		}))
	}
	if token != "" {
		middleware = append(middleware, httpsrv.BearerToken(token))
	}
	srv := &http.Server{
		Handler: httpsrv.Chain(docspkg.NewHandler(store, events), middleware...),

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

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// background tracks every goroutine this function starts. Waiting on it
	// before returning is what makes "drain the started operations" structural
	// rather than a sequence of receives a later edit could get out of step with.
	var background sync.WaitGroup
	serveCh := make(chan error, 1)
	background.Add(1)
	go func() {
		defer background.Done()
		serveCh <- rt.serve(srv, ln)
	}()

	var lease *docspkg.ServerLease
	if eligible {
		session := &docsDiscoverySession{
			rt:         rt,
			dir:        serversDir,
			token:      token,
			generation: &generation,
			serve:      serveCh,
			background: &background,
			errOut:     errOut,
		}
		outcome, publishErr := session.publish(ctx, initialInfo)
		lease = outcome.lease
		if publishErr != nil {
			return abortDocsServeStartup(rt, srv, events, &background, lease, errOut, publishErr)
		}
		if outcome.primary != nil {
			return finishDocsServeStartup(rt, srv, events, &background, lease, errOut, *outcome.primary)
		}
	}

	// The banner prints AFTER publication, not before.
	//
	// Startup can now fail (a self-probe that never answers) or be superseded
	// (a Serve error, a signal) after the listener exists but before the server
	// is usable. Printing the URL first would announce a reader that is about
	// to exit, and the address line is what every caller — an operator, and the
	// test harness — treats as "it is up".
	url := "http://" + ln.Addr().String() + "/"
	fmt.Fprintf(out, "forgectl docs: serving %d doc(s) across %d root(s)\n", len(idx.List()), len(idx.Roots()))
	fmt.Fprintf(out, "  %s\n", url)
	if authLine := docsAuthStartupLine(resolvedToken); authLine != "" {
		fmt.Fprintln(out, authLine)
	}
	fmt.Fprintln(out, "  Ctrl-C to stop")

	// Live reload is best-effort: if the OS refuses a watcher (descriptor
	// limits, an exotic filesystem), the reader still serves — it just stops
	// refreshing on its own. Failing the whole command over it would be a worse
	// trade than a warning.
	if watcher, watchErr := docspkg.NewWatcher(store, events); watchErr != nil {
		warnDocsServe(errOut, "warning: live reload unavailable: %v", watchErr)
	} else {
		defer watcher.Close() //nolint:errcheck // best-effort resource release on exit
		go watcher.Run(ctx)
		fmt.Fprintln(out, "  live reload: on")
	}

	if openFlag {
		// Don't open a tab that is guaranteed to 401. A browser navigation cannot
		// carry an Authorization header, so on a token-protected server --open
		// would reliably produce an unauthorized page and leave the operator
		// debugging the reader instead of reading. `docs open` already declines
		// for exactly this reason; applying the same rule here keeps the two
		// verbs consistent rather than correct in one place only.
		if token != "" {
			fmt.Fprintln(errOut, "note: not opening a browser — this server requires a bearer token, which a browser navigation cannot supply")
		} else if openErr := docspkg.OpenBrowser(ctx, deps.Runner, url); openErr != nil {
			warnDocsServe(errOut, "warning: failed to open browser: %v", openErr)
		}
	}

	// Steady state. A buffered Serve result outranks cancellation here for the
	// same reason it does during startup: a server that has already stopped
	// serving has nothing left for Shutdown to drain gracefully.
	event := awaitDocsServeEvent(ctx, serveCh, nil)
	var result error
	switch event.Kind {
	case docsServeEventServe:
		result = normalizeDocsServeResult(event.Err)
	default:
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
		result = rt.shutdown(srv, shutdownCtx)
		if errors.Is(result, context.DeadlineExceeded) {
			// A drain that runs out of time is not a failed command. The
			// operator asked the server to stop; it stops. Report the forced
			// close so a hung connection is visible, then exit zero — exiting
			// non-zero here would make an idle browser tab look like a crash.
			_ = rt.closeServer(srv) //nolint:errcheck,gosec // already forcing the close; there is nothing left to recover
			warnDocsServe(errOut, "warning: some connections were still open after %s; closed them", shutdownGrace)
			result = nil
		}
	}

	closeDocsServeLease(rt, lease, errOut)
	return result
}

// abortDocsServeStartup unwinds a startup that failed after Serve began.
//
// closeServer rather than shutdown: there is no graceful drain to perform for a
// server that never finished starting, and Shutdown would wait out its grace
// period on a listener no client has reached.
func abortDocsServeStartup(
	rt docsServeRuntime,
	srv *http.Server,
	events *docspkg.Broker,
	background *sync.WaitGroup,
	lease *docspkg.ServerLease,
	errOut io.Writer,
	cause error,
) error {
	events.Close()
	rt.closeServer(srv) //nolint:errcheck // already returning a failure
	background.Wait()
	closeDocsServeLease(rt, lease, errOut)
	return cause
}

// finishDocsServeStartup returns under a Serve result or a cancellation that
// won an arbitration before the server reached steady state.
//
// Cancellation here is a normal startup stop, so it returns nil after cleanup
// rather than calling Shutdown: nothing has been announced and no client has
// been served, so there is nothing to drain.
func finishDocsServeStartup(
	rt docsServeRuntime,
	srv *http.Server,
	events *docspkg.Broker,
	background *sync.WaitGroup,
	lease *docspkg.ServerLease,
	errOut io.Writer,
	event docsServeEvent,
) error {
	var result error
	if event.Kind == docsServeEventServe {
		result = normalizeDocsServeResult(event.Err)
	} else {
		events.Close()
		rt.closeServer(srv) //nolint:errcheck // stopping a server that never finished starting
	}
	background.Wait()
	closeDocsServeLease(rt, lease, errOut)
	return result
}

// closeDocsServeLease removes this server's discovery record exactly once,
// after the primary outcome is known.
//
// A cleanup failure is a warning and never becomes the returned error: the
// operator's question is "did my server run?", and answering it with a
// bookkeeping failure would misreport a session that worked.
func closeDocsServeLease(rt docsServeRuntime, lease *docspkg.ServerLease, errOut io.Writer) {
	if lease == nil {
		return
	}
	if err := rt.closeLease(lease); err != nil {
		warnDocsServe(errOut, "warning: could not remove this server's docs discovery record: %v", err)
	}
}

// normalizeDocsServeResult maps the expected close error to a clean exit.
func normalizeDocsServeResult(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func warnDocsServe(errOut io.Writer, format string, args ...any) {
	fmt.Fprintf(errOut, format+"\n", args...)
}
