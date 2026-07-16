package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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
			return runDocsServe(cmd, deps, idx, addr, openFlag)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "bind address (default: [docs].addr, else 127.0.0.1 with a random port)")
	cmd.Flags().BoolVar(&openFlag, "open", false, "open the system browser once the server is listening")
	return cmd
}

// runDocsServe binds the listener, wires the Host-allowlist middleware
// (forgectl#93 security-chain item 1) around the docs handler, and serves
// until the command's context is canceled (Ctrl-C/SIGTERM) or the server
// itself fails to start.
func runDocsServe(cmd *cobra.Command, deps module.Deps, idx *docspkg.Index, addrFlag string, openFlag bool) error {
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

	handler := httpsrv.Chain(docspkg.NewHandler(idx), httpsrv.HostAllowlist(httpsrv.DefaultAllowedHosts))
	srv := &http.Server{Handler: handler}

	url := "http://" + ln.Addr().String() + "/"
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "forgectl docs: serving %d doc(s) across %d root(s)\n", len(idx.List()), len(idx.Roots()))
	fmt.Fprintf(out, "  %s\n", url)
	fmt.Fprintln(out, "  Ctrl-C to stop")

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if openFlag {
		if err := docspkg.OpenBrowser(ctx, deps.Runner, url); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to open browser: %v\n", err)
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
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
