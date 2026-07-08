package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// newBenchOpenCmd builds `forgectl bench open [target]` — opens a bench UI in
// the browser. With no target it defaults to the hearth homepage.
func newBenchOpenCmd(cfg config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "open [target]",
		Short: "Open a bench UI in the browser (hearth | grafana; default hearth)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return bench.Open(cmd.Context(), cfg, exec.OSRunner{}, target)
		},
	}
}
