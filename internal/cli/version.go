package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd restores a bare `version` verb. fang already wires
// `--version` (see execute.go); this is a host-level affordance layered on
// top — like fang's injected man/completion and the launch/cl intercept —
// deliberately outside the module registry (ADR-0005 covers domain command
// groups, not host plumbing).
//
// fang sets root.Version to the full build string (including the
// " (commit)" suffix when present) before ExecuteContext runs, so
// cmd.Root().Version here is byte-identical to what --version prints —
// no format duplication.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "version",
		Short:                 "Print the version",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), cmd.Root().Name()+" version "+cmd.Root().Version)
			return err
		},
	}
}
