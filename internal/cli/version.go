package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
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
	var asJSON bool
	cmd := &cobra.Command{
		Use:                   "version",
		Short:                 "Print the version",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return termsafe.JSONEncoder(cmd.OutOrStdout()).Encode(versionJSON{
					Name:    cmd.Root().Name(),
					Version: cmd.Root().Version,
				})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), cmd.Root().Name()+" version "+cmd.Root().Version)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"name":...,"version":...} to stdout; version is the same string --version prints`)
	return cmd
}

// versionJSON is the `version --json` shape. Fields may be added; existing
// ones do not change (ADR-0008).
type versionJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
