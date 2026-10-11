package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hostnameFunc reads the host name; tests replace it.
type hostnameFunc func() (string, error)

func newSurfaceMergeMachineCmd(_ module.Deps) *cobra.Command {
	return newSurfaceMergeMachineCmdWith(os.Hostname)
}

func newSurfaceMergeMachineCmdWith(hostname hostnameFunc) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "merge-machine",
		Short: "Print this machine's [surface.merge] machine value",
		Long: `merge-machine prints the value [surface.merge] machine must hold for the
merge policy to apply on this machine: the first 12 hex characters of
sha256(<host name> + "` + config.MergeMachineSalt + `"), where the host name is the one the
operating system reports. A config file synced to another machine then
resolves the policy to off there. --json prints {"machine","salt"}.

  forgectl surface merge-machine`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := hostname()
			if err != nil {
				return termsafe.Error(fmt.Errorf("read the host name: %w", err))
			}
			machine := config.MergeMachineDigest(h)
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					Machine string `json:"machine"`
					Salt    string `json:"salt"`
				}{machine, config.MergeMachineSalt})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "machine = %q\n", machine)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"machine","salt"} as JSON`)
	return cmd
}
