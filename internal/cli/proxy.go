package cli

import (
	"fmt"
	"io"
	"os"

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
		newProxyListCmd(deps),
		newProxyStatusCmd(deps, os.LookupEnv),
	)
	return cmd
}

// noProfilesMessage and noMatchMessage are category-only: each names the
// state reached and nothing about the configuration or environment that
// reached it.
const (
	noProfilesMessage = "no proxy profiles are configured"
	noMatchMessage    = "no configured profile matches the current environment"
)

func newProxyListCmd(deps module.Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured profile names",
		Long: `list prints the name of every configured profile, one per line, sorted.
Names come from config.toml keys; no profile value is read or printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := proxypkg.Names(deps.Cfg.Proxy.Profiles)
			if len(names) == 0 {
				// Stdout stays a clean name list for a caller piping it, so
				// the informational line goes to stderr.
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), noProfilesMessage)
				return err
			}
			out := cmd.OutOrStdout()
			for _, name := range names {
				if _, err := fmt.Fprintln(out, termsafe.SafeLine(name)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// newProxyStatusCmd takes its environment reader as a parameter rather than
// calling os.LookupEnv inline. A test that seeded the real environment with
// proxy variables would also seed net/http's process-wide proxy cache, which
// resolves once and would then route unrelated tests' requests through a host
// that does not exist.
func newProxyStatusCmd(deps module.Deps, lookup proxypkg.Lookup) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report which configured profile the current environment carries",
		Long: `status names the configured profile whose values the current environment
carries, then reports each proxy variable as set or unset.

It prints no proxy value, from either the configuration or the environment:
every comparison happens in memory. A half-applied environment matches no
profile, which is the state this verb exists to make visible.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, matched := proxypkg.Match(deps.Cfg.Proxy.Profiles, lookup)
			if !matched {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), noMatchMessage)
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "profile: %s\n", termsafe.SafeLine(name)); err != nil {
				return err
			}
			for _, v := range proxypkg.Environment(lookup) {
				if _, err := fmt.Fprintf(out, "%s: %s\n", v.Name, variableState(v.Set)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// variableState renders presence, the only shape a proxy variable reports.
func variableState(set bool) string {
	if set {
		return "set"
	}
	return "unset"
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
