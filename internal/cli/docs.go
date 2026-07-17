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
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Local markdown reader — render + serve an indexed doc set over loopback HTTP",
		Long: `docs is forgectl's local markdown reader (forgectl#93): pure-Go
server-side rendering (goldmark+GFM, class-based chroma highlighting,
bluemonday sanitization), Artificer-themed, served over loopback HTTP so it
behaves the same whether you're at the machine or SSH'd in from the headless
workbench — no terminal-specific rendering, no popping between windows.

  forgectl docs serve [dir|file ...]     render + serve an indexed doc set
  forgectl docs serve --open             also open the system browser
  forgectl docs list [dir|file ...]      list the indexed docs, no server
  forgectl docs list --json              machine-readable output for scripts

With no arguments, both verbs index cwd, ./docs (if present), and
$CADENCE_FIELD_REPORTS_DIR (if set), plus any extra roots configured in the
[docs] section of config.toml (macOS: ~/Library/Application
Support/forgectl/config.toml). Naming directories or files on the command
line replaces that default set entirely.

The server binds loopback-only by default and rejects any request whose
Host header isn't 127.0.0.1/localhost/::1 — DNS rebinding defense, not just
a bind-address restriction. Live reload (SSE), mermaid, and pan/zoom SVG are
staged as follow-on PRs (#93 PR2/PR3); this is render + index only.`,
	}
	cmd.AddCommand(
		newDocsServeCmd(deps),
		newDocsListCmd(deps),
	)
	return cmd
}
