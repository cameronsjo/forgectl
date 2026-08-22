package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	proxypkg "github.com/cameronsjo/forgectl/internal/proxy"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// proxyModule declares the config-defined current-shell proxy extension.
var proxyModule = module.Manifest{
	Name:      "proxy",
	Tier:      module.TierExtension,
	ConfigKey: "proxy",
	New:       newProxyCmd,
}

func newProxyCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "proxy",
		Short:         "Emit proxy-profile changes for an explicit shell wrapper",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `proxy emits a fixed batch of shell exports and unsets. It cannot change
its parent shell: capture and eval its output through the documented wrapper.

Profile values are sensitive. The use command is a machine protocol for that
wrapper, not a status or display command; forgectl never logs those values.`,
	}
	cmd.AddCommand(
		newProxyUseCmd(deps),
		newProxyOffCmd(),
	)
	return cmd
}

func newProxyUseCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "use NAME",
		Short: "Emit exports/unsets for one configured profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, ok := deps.Cfg.Proxy.Profiles[args[0]]
			if !ok {
				return fmt.Errorf("proxy profile %s is not configured", termsafe.QuoteText(args[0]))
			}
			script, err := proxypkg.Use(profile)
			if err != nil {
				return err
			}
			return writeProxyScript(cmd.OutOrStdout(), script)
		},
	}
}

func newProxyOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Emit unsets for every supported proxy variable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeProxyScript(cmd.OutOrStdout(), proxypkg.Off())
		},
	}
}

// writeProxyScript is the sole sensitive-output sink. Do not route this
// protocol through termsafe: escaping it for display would corrupt the shell
// grammar; do not add logging here, because the script contains proxy values.
func writeProxyScript(out io.Writer, script string) error {
	_, err := fmt.Fprintln(out, script)
	return err
}
