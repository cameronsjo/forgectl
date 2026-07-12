package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/forgive"
)

// newBenchCmd builds the `bench` parent command. Verbs are attached as
// subcommands: `status` reports aggregate health across the local bench.
// Mirrors newWorkflowCmd's parent/subcommand shape.
func newBenchCmd(cfg config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Discover and health-check the local dev bench (hearth, chronicle, flux)",
		Long: `bench is forgectl's interop spine across the local developer bench —
the hearth telemetry stack, the chronicle transcript-retention layer, and the
flux board. It orchestrates each system through its frozen contract; it never
reimplements one.

  forgectl bench status          aggregate health card across all components
  forgectl bench status --json   machine-readable, for scripts
  forgectl bench up              bring up the configured services
  forgectl bench open [target]   open a bench UI (hearth | grafana)

Configure it in the [bench] section of config.toml (macOS: ~/Library/Application
Support/forgectl/config.toml). Unset components degrade to "not-configured"
rather than erroring.`,
	}
	cmd.AddCommand(
		newBenchStatusCmd(cfg),
		newBenchUpCmd(cfg),
		newBenchOpenCmd(cfg),
	)
	applyAliases(cmd, forgive.BenchAliases)
	return cmd
}
