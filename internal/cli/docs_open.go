package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newDocsOpenCmd builds `forgectl docs open [path]`.
//
// This verb STEERS an already-running reader; it never starts one. That is a
// deliberate boundary, not a missing feature. `docs serve` is a foreground
// process the operator owns — it prints its address, holds the terminal, and
// stops on Ctrl-C. If `open` could spawn one, it would either fork a server
// nobody can see (and that nothing reaps) or block the terminal it was called
// from, and either way the operator would no longer know how many readers exist
// or which one their browser is pointed at. When nothing is running, `open` says
// so and names the command to run.
//
// It uses the SYSTEM browser, never a terminal's own browser command. The
// reader's entire premise is being terminal-agnostic — reachable from the
// machine, from an SSH session, from a phone — so coupling this verb to any one
// terminal emulator would undo the reason the reader is served over HTTP at all.
func newDocsOpenCmd(deps module.Deps) *cobra.Command {
	var printOnly bool

	cmd := &cobra.Command{
		Use:   "open [path]",
		Short: "Point the system browser at a doc on the running reader",
		Long: `open steers the reader started by ` + "`forgectl docs serve`" + `.

With a path, it opens that document; with no path, the reader's index. The
reader must already be running — open never starts one, so there is exactly one
server and the operator always knows where it came from.

  forgectl docs open                       open the reader's index
  forgectl docs open docs/plans/thing.md   open one doc
  forgectl docs open --print-url thing.md  print the URL instead of opening it
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var target string
			if len(args) == 1 {
				target = args[0]
			}
			return runDocsOpen(cmd, deps, target, printOnly)
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print-url", false, "print the resolved URL instead of opening a browser")
	return cmd
}

func runDocsOpen(cmd *cobra.Command, deps module.Deps, target string, printOnly bool) error {
	serversDir, err := config.DocsServersDir()
	if err != nil {
		return err
	}
	legacyPath, err := config.DocsServerPath()
	if err != nil {
		return err
	}

	server, err := docspkg.DiscoverServerInfo(cmd.Context(), serversDir, legacyPath)
	if errors.Is(err, docspkg.ErrNoServer) {
		// Name the fix rather than just the failure — this is the error an
		// operator will hit most, and it has exactly one remedy.
		return fmt.Errorf("no docs server is running; start one with `forgectl docs serve`")
	}
	if err != nil {
		return err
	}
	info := server.Info

	url := info.BaseURL()
	if target != "" {
		url, err = resolveOpenTarget(cmd.Context(), server, target)
		if err != nil {
			return err
		}
	}

	if printOnly {
		fmt.Fprintln(cmd.OutOrStdout(), url)
		return nil
	}

	if info.Token != "" && server.Legacy {
		// A legacy server has no freshness endpoint, so there is no way to
		// establish that the listener at this address is the server the record
		// described before handing it a credential. Print the URL and the fix;
		// never the token.
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "%s\n", url)
		fmt.Fprintf(out, "\nThis reader predates generation-owned discovery and requires a bearer token.\n")
		fmt.Fprintf(out, "Restart it with `forgectl docs serve` so `docs open` can verify it before authenticating.\n")
		return nil
	}

	if info.Token != "" {
		// A browser navigation cannot carry an Authorization header, and putting
		// the token in the URL would write it into history, referrers, and any
		// logs in between. So when the reader is token-protected, print the URL
		// and the token instead of pretending a plain navigation will work.
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "%s\n", url)
		fmt.Fprintf(out, "\nThis reader requires a bearer token, which a browser navigation cannot supply.\n")
		fmt.Fprintf(out, "Use a client that can set the header, e.g.:\n")
		fmt.Fprintf(out, "  curl -H 'Authorization: Bearer %s' '%s'\n", info.Token, url)
		return nil
	}

	return docspkg.OpenBrowser(cmd.Context(), deps.Runner, url)
}

// resolveOpenTarget maps a user-supplied path onto a URL on the running server.
//
// The path is resolved through the SERVER's index, not by string manipulation
// here: the server is the only thing that knows its own root labels and which
// files it actually indexed. Asking it means `open` cannot hand back a URL for a
// file the server would refuse to serve, and it inherits the index's exclusions
// for free rather than reimplementing them.
func resolveOpenTarget(ctx context.Context, server docspkg.DiscoveredServer, target string) (string, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", termsafe.QuotePath(target), err)
	}

	root, rel, err := docspkg.LocateDoc(ctx, server, abs)
	if err != nil {
		return "", err
	}
	return server.Info.DocURL(root, rel), nil
}
