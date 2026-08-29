package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// docsModule declares the local markdown reader extension (ADR-0005): owns
// the [docs] config section. See forgectl#93 for the full design; this is
// PR1's slice (render + index, no live reload).
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
	var graphics string
	cmd := &cobra.Command{
		Use:   "docs [dir|file ...]",
		Short: "Browse an indexed Markdown doc set in the terminal or over loopback HTTP",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if handled, err := docsHelpForNonTTY(cmd, args); handled {
				return err
			}
			return runDocsBrowse(cmd, deps, args, graphics)
		},
		Long: `docs is forgectl's local markdown reader (forgectl#93): pure-Go
rendering with an Artificer-themed terminal explorer as the ordinary path.
Local images, SVG, and supported Mermaid diagrams render through the Kitty
graphics protocol in compatible terminals such as Ghostty and fall back to
readable text elsewhere. The loopback HTTP reader remains available for remote
and phone access and for exact Mermaid.js rendering.

  forgectl docs [dir|file ...]           browse in the terminal
  forgectl docs browse [dir|file ...]    explicit spelling of the same reader
  forgectl docs serve [dir|file ...]     render + serve an indexed doc set
  forgectl docs serve --open             also open the system browser
  forgectl docs open [path]              point the browser at a doc on the
                                         already-running reader
  forgectl docs list [dir|file ...]      list the indexed docs, no server
  forgectl docs list --json              machine-readable output for scripts

Diagrams render in the page: a fenced code block tagged mermaid becomes a live
diagram themed from the same Artificer tokens as the rest of the reader, and
both those and inline SVG pan and zoom (drag to pan, modifier-scroll or
click-then-scroll to zoom, double-click or 0 to reset).

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
		newDocsBrowseCmd(deps),
		newDocsServeCmd(deps),
		newDocsOpenCmd(deps),
		newDocsListCmd(deps),
	)
	cmd.Flags().StringVar(&graphics, "graphics", "auto", "image mode for terminal browsing: auto, kitty, or off")
	return cmd
}
