package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// docsModule declares the local markdown reader extension (ADR-0005) and owns
// the [docs] config section. See forgectl#93 for the full design.
var docsModule = module.Manifest{
	Name:      "docs",
	Tier:      module.TierExtension,
	ConfigKey: "docs",
	New:       newDocsCmd,
}

// newDocsCmd builds the `docs` parent command over the registry Deps. Verbs
// are attached as subcommands, mirroring newBenchCmd's parent/subcommand
// shape.
func newDocsCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs [dir|file ...]",
		Short: "Read an indexed Markdown doc set in an embedded HTML preview",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if handled, err := docsHelpForNonTTY(cmd, args); handled {
				return err
			}
			return runDocsPreview(cmd, deps, args)
		},
		Long: `docs is forgectl's local markdown reader (forgectl#93): pure-Go
rendering with an Artificer-themed HTML preview as the ordinary path. Inside
cmux, the preview opens as a right-hand browser pane in the caller's workspace
without taking focus. Elsewhere it opens in the system browser. The invoking
terminal owns the foreground loopback server; press Ctrl-C there to stop it.

  forgectl docs [dir|file ...]           serve + open the reading preview
  forgectl docs serve [dir|file ...]     render + serve an indexed doc set
  forgectl docs serve --open             also open the system browser
  forgectl docs open [path]              point the browser at a doc on the
                                         already-running reader
  forgectl docs list [dir|file ...]      list the indexed docs, no server
  forgectl docs list --json              machine-readable output for scripts

Mermaid.js renders fenced mermaid blocks, and Mermaid and inline SVG can pan and
zoom (drag to pan, modifier-scroll or click-then-scroll to zoom, double-click or
0 to reset). Reading settings in the app bar control body, heading, and code
fonts plus text size, line height, and content width.

With no arguments, both verbs index cwd, ./docs (if present), and
$CADENCE_FIELD_REPORTS_DIR (if set), plus any extra roots configured in the
[docs] section of config.toml (macOS: ~/Library/Application
Support/forgectl/config.toml). Naming directories or files on the command
line replaces that default set entirely.

The server binds loopback-only by default and rejects any request whose
Host header isn't 127.0.0.1/localhost/::1 — DNS rebinding defense, not just
a bind-address restriction. Binding --addr to a non-loopback address adds that
address to the allowlist and REQUIRES a bearer token, generating one if you do
not pass --token-file: exposing the reader to the network and authenticating it
are one decision, never two. A token file must be an absolute owner-only regular
file containing one RFC 6750 bearer token plus an optional final LF or CRLF.
Protected servers cannot be opened directly with --open because browser
navigation cannot attach an Authorization header.`,
	}
	cmd.AddCommand(
		newDocsServeCmd(deps),
		newDocsOpenCmd(deps),
		newDocsListCmd(deps),
	)
	return cmd
}
