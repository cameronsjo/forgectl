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
wrapper, not a status or display command; forgectl never logs those values.
The read verbs, list and status, print profile names and per-variable
set/unset only — never a value, from either the configuration or the live
environment.`,
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

// proxyListRowJSON is the --json wire shape for one `proxy list` row — the
// profile name the human list prints, and nothing else: no profile value is
// read or emitted.
type proxyListRowJSON struct {
	Name string `json:"name"`
}

// writeProxyListJSON encodes names as a JSON array through the sanctioned
// termsafe seam. An empty configuration encodes [], never null.
func writeProxyListJSON(w io.Writer, names []string) error {
	rows := make([]proxyListRowJSON, 0, len(names))
	for _, name := range names {
		rows = append(rows, proxyListRowJSON{Name: name})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// proxyStatusJSON is the --json wire shape for `proxy status`. It mirrors the
// human report's redaction exactly: the matched profile's name and per-variable
// set/unset, never a value. When no profile matches, Variables is [] — the
// human path prints only the verdict there, because which variable diverged is
// itself a fact about a configured value.
type proxyStatusJSON struct {
	Matched   bool                `json:"matched"`
	Profile   string              `json:"profile"`
	Variables []proxyVariableJSON `json:"variables"`
}

// proxyVariableJSON is one proxy variable's presence: its lowercase name and
// whether any spelling carries a non-empty value.
type proxyVariableJSON struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

// buildProxyStatusJSON converts a match verdict into the --json wire shape.
// Variables is never nil so the encoder emits [] rather than null.
func buildProxyStatusJSON(name string, matched bool, lookup proxypkg.Lookup) proxyStatusJSON {
	status := proxyStatusJSON{Matched: matched, Variables: []proxyVariableJSON{}}
	if !matched {
		return status
	}
	status.Profile = name
	env := proxypkg.Environment(lookup)
	status.Variables = make([]proxyVariableJSON, 0, len(env))
	for _, v := range env {
		status.Variables = append(status.Variables, proxyVariableJSON{Name: v.Name, Set: v.Set})
	}
	return status
}

// writeProxyStatusJSON encodes the status through the sanctioned termsafe seam.
func writeProxyStatusJSON(w io.Writer, status proxyStatusJSON) error {
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func newProxyListCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured profile names",
		Long: `list prints the name of every configured profile, one per line, sorted.
Names come from config.toml keys; no profile value is read or printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := proxypkg.Names(deps.Cfg.Proxy.Profiles)
			if asJSON {
				// [] is the whole answer for an empty configuration; the
				// stderr notice is the human path's, not the machine's.
				return writeProxyListJSON(cmd.OutOrStdout(), names)
			}
			if len(names) == 0 {
				// Stdout stays a clean name list for a caller piping it, so
				// the informational line goes to stderr.
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), noProfilesMessage)
				return err
			}
			out := cmd.OutOrStdout()
			for _, name := range names {
				if _, err := fmt.Fprintln(out, safeText(name)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"name":...}] to stdout`)
	return cmd
}

// newProxyStatusCmd takes its environment reader as a parameter rather than
// calling os.LookupEnv inline. A test that seeded the real environment with
// proxy variables would also seed net/http's process-wide proxy cache, which
// resolves once and would then route unrelated tests' requests through a host
// that does not exist.
func newProxyStatusCmd(deps module.Deps, lookup proxypkg.Lookup) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report which configured profile the current environment carries",
		Long: `status names the configured profile whose values the current environment
carries, and then reports each proxy variable as set or unset. When no
profile matches, that verdict is the whole output: which variable diverged is
itself a fact about a configured value.

It prints no proxy value, from either the configuration or the environment:
every comparison happens in memory. A half-applied environment matches no
profile, which is the state this verb exists to make visible.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, matched := proxypkg.Match(deps.Cfg.Proxy.Profiles, lookup)
			if asJSON {
				return writeProxyStatusJSON(cmd.OutOrStdout(), buildProxyStatusJSON(name, matched, lookup))
			}
			if !matched {
				// Stdout, unlike list's empty case: "nothing matches" IS
				// this verb's answer, not the absence of one.
				_, err := fmt.Fprintln(cmd.OutOrStdout(), noMatchMessage)
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "profile: %s\n", safeText(name)); err != nil {
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
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"matched":...,"profile":...,"variables":[{"name":...,"set":...}]} to stdout`)
	return cmd
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
		Use:   "use <NAME>",
		Short: "Emit exports/unsets for one configured profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, ok := deps.Cfg.Proxy.Profiles[args[0]]
			if !ok {
				return fmt.Errorf("proxy profile %s is not configured", termsafe.QuoteArgMax(args[0], termsafe.ArgEchoMaxRunes))
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
