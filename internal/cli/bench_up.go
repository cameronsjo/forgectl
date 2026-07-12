package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/module"
)

// newBenchUpCmd builds `forgectl bench up` — thin lifecycle delegation that
// brings the configured bench services up via their own entrypoints. Progress
// and skip notes go to stderr; the delegated scripts own stdout.
func newBenchUpCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Bring up the configured bench services (hearth, chronicle)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return bench.Up(cmd.Context(), deps.Cfg, deps.Runner, cmd.ErrOrStderr())
		},
	}
}
