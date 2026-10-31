//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"io"
	"net"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk/deskweb"
	"github.com/cameronsjo/forgectl/internal/httpsrv"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// deskServeOpts are `desk serve`'s flags.
type deskServeOpts struct {
	addr        string
	allowHosts  []string
	rotateToken bool
}

func newDeskServeCmd(dir *string, _ module.Deps) *cobra.Command {
	var o deskServeOpts
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "A local web page to answer the desk's asks (phone-friendly)",
		Long: `serve runs a small web page over the desk: open asks by bucket, snoozed asks,
the queued scripts (read-only, with their sha256), and the recently answered
asks. Each ask takes the responses it allows: done, a reply, a link, or a
snooze. The page reloads itself when the desk changes.

It binds loopback only (127.0.0.1, a free port unless --addr names one) and
prints the address to open, with a per-install key after the #. The key lives
in <desk>/asks/web.token (mode 0600); --rotate-token replaces it and signs out
every browser.

From a phone, publish the port on your tailnet:

  tailscale serve --bg http://127.0.0.1:PORT

then open https://<machine>.<tailnet>.ts.net/#k=KEY on the phone once, and
pass that host name with --allow-host so the page accepts it.`,
		Example: `  forgectl desk serve
  forgectl desk serve --addr 127.0.0.1:8765 --allow-host mymac.example.ts.net`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeskServe(cmd, *dir, o)
		},
	}
	cmd.Flags().StringVar(&o.addr, "addr", httpsrv.LoopbackAddr, "loopback address to bind (127.0.0.1:PORT or [::1]:PORT)")
	cmd.Flags().StringArrayVar(&o.allowHosts, "allow-host", nil, "extra Host name to accept, such as the tailscale serve name (repeatable)")
	cmd.Flags().BoolVar(&o.rotateToken, "rotate-token", false, "replace the page's key, signing out every browser")
	return cmd
}

func runDeskServe(cmd *cobra.Command, dirFlag string, o deskServeOpts) error {
	if !httpsrv.IsLoopbackAddr(o.addr) {
		return deskUsage("desk serve: --addr %q is not a loopback address; use tailscale serve for remote access", termsafe.SafeLineMax(o.addr, deskQuoteMax))
	}
	for _, h := range o.allowHosts {
		if !validHostName(h) {
			return deskUsage("desk serve: --allow-host %q is not a host name", termsafe.SafeLineMax(h, deskQuoteMax))
		}
	}
	store, deskDir, err := openAskStore(dirFlag)
	if err != nil {
		return err
	}
	token, err := deskweb.LoadToken(deskDir, o.rotateToken)
	if err != nil {
		return err
	}
	ln, err := httpsrv.Listen(o.addr)
	if err != nil {
		return fmt.Errorf("desk serve: listen on %s: %w", termsafe.SafeLineMax(o.addr, deskQuoteMax), err)
	}
	port := 0
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		port = ta.Port
	}
	srv, err := deskweb.New(deskweb.Options{Store: store, DeskDir: deskDir, Token: token, AllowHosts: o.allowHosts, Port: port})
	if err != nil {
		_ = ln.Close()
		return err
	}
	banner := fmt.Sprintf("desk: %s\nopen: http://%s/#k=%s\nThe key after # is a password for this page; do not share the address. Ctrl+C stops.\n", termsafe.QuotePath(deskDir), ln.Addr().String(), token)
	if _, err := io.WriteString(cmd.OutOrStdout(), banner); err != nil {
		_ = ln.Close()
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return deskweb.Serve(ctx, ln, srv.Handler())
}

// validHostName accepts a DNS name: labels of letters, digits and hyphens.
func validHostName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	label := 0
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c == '.':
			if label == 0 {
				return false
			}
			label = 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			label++
			if label > 63 {
				return false
			}
		default:
			return false
		}
	}
	return label > 0
}
