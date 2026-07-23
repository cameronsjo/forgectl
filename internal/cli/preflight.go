package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	preflightpkg "github.com/cameronsjo/forgectl/internal/preflight"
)

// preflightModule declares the configuration-alignment extension (ADR-0005):
// the [preflight] config section's owner, no alias surface.
var preflightModule = module.Manifest{
	Name:      "preflight",
	Tier:      module.TierExtension,
	ConfigKey: "preflight",
	New:       newPreflightCmd,
}

// newPreflightCmd builds `forgectl preflight` over the registry Deps.
func newPreflightCmd(deps module.Deps) *cobra.Command {
	return newPreflightCmdForConfig(deps.Cfg.Preflight)
}

// newPreflightCmdForConfig builds the command over an already-resolved
// PreflightConfig — split out so tests can inject one directly (mirrors
// newNetCmdForClient) without going through the full module.Deps wiring.
// homeDir and projectDir are resolved at RunE time via
// os.UserHomeDir()/os.Getwd(), not injected here — tests control both with
// t.Setenv("HOME", …) and t.Chdir(…), the pattern resolveDocsRoots' tests
// already use.
func newPreflightCmdForConfig(cfg config.PreflightConfig) *cobra.Command {
	var apply, asJSON bool

	cmd := &cobra.Command{
		Use: "preflight",
		// SilenceUsage/SilenceErrors mirror env's own setting (env.go):
		// --json is spec'd to emit ONLY the machine-readable report on
		// stdout, and cobra's default auto-usage-on-error would otherwise
		// append usage text to that same stream when this command tree is
		// exercised directly (as the tests do) rather than through fang's
		// root, which already silences both.
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Align enabled plugins to the skill catalog's core-tier default set",
		Long: `preflight computes the cadence skill catalog's deterministic core-tier
plugin set, folds in this project's committed .claude/settings.json
enabledPlugins entries (a repo's own choices survive by inclusion), and
diffs that target against what's currently effectively enabled across the
three settings scopes (user, project, local — replace-not-merge: a higher
scope's enabledPlugins is the whole answer, never merged with a lower one).

  forgectl preflight             report the change-set, make no changes
  forgectl preflight --apply     write the COMPLETE aligned enabledPlugins
                                  set to .claude/settings.local.json
  forgectl preflight --json      machine-readable report for scripting

preflight's only write target is .claude/settings.local.json — the
personal, auto-gitignored override scope. The committed .claude/settings.json
is read-only input, never touched.

Exit codes: 0 aligned (or --apply just made it so), 1 misaligned (dry-run
only), 2 error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return WithExitCode(fmt.Errorf("resolve home directory: %w", err), 2)
			}
			projectDir, err := os.Getwd()
			if err != nil {
				return WithExitCode(fmt.Errorf("resolve project directory: %w", err), 2)
			}

			report, err := computePreflightReport(homeDir, projectDir, cfg)
			if err != nil {
				return WithExitCode(err, 2)
			}

			if apply && !report.ChangeSet.Aligned() {
				path, err := preflightpkg.WriteLocal(projectDir, report.Target, report.Marketplaces)
				if err != nil {
					return WithExitCode(fmt.Errorf("apply: %w", err), 2)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", path)
			}

			out := cmd.OutOrStdout()
			if asJSON {
				if err := writePreflightJSON(out, report); err != nil {
					return WithExitCode(err, 2)
				}
			} else {
				printPreflightReport(out, report)
			}

			if !apply && !report.ChangeSet.Aligned() {
				return WithExitCode(
					fmt.Errorf("misaligned: %d to enable, %d to disable", len(report.ChangeSet.Enable), len(report.ChangeSet.Disable)),
					1,
				)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write the complete aligned enabledPlugins set to .claude/settings.local.json")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable report to stdout")
	return cmd
}

// preflightReport bundles computePreflightReport's result: the located
// catalog, the current/target enabledPlugins sets, the marketplaces to
// carry forward on --apply, and the computed change-set.
type preflightReport struct {
	CatalogPath  string
	Current      map[string]bool
	Target       map[string]bool
	Marketplaces map[string]json.RawMessage
	ChangeSet    preflightpkg.ChangeSet
}

// computePreflightReport runs the full scan: locate + parse the catalog,
// read the three settings scopes, and diff current against Cut A's
// deterministic target.
func computePreflightReport(homeDir, projectDir string, cfg config.PreflightConfig) (preflightReport, error) {
	catalogPath, err := preflightpkg.LocateCatalog(homeDir, cfg.CatalogPath)
	if err != nil {
		return preflightReport{}, fmt.Errorf("locate skill catalog: %w", err)
	}
	plugins, err := preflightpkg.ReadCatalog(catalogPath)
	if err != nil {
		return preflightReport{}, err
	}
	core := preflightpkg.CoreDefaultSet(plugins)
	for _, extra := range cfg.DefaultSet {
		core[extra] = true
	}

	userDoc, err := preflightpkg.ReadDocument(preflightpkg.UserPath(homeDir))
	if err != nil {
		return preflightReport{}, err
	}
	projectDoc, err := preflightpkg.ReadDocument(preflightpkg.ProjectPath(projectDir))
	if err != nil {
		return preflightReport{}, err
	}
	localDoc, err := preflightpkg.ReadDocument(preflightpkg.LocalPath(projectDir))
	if err != nil {
		return preflightReport{}, err
	}

	current := preflightpkg.EffectiveEnabled(userDoc, projectDoc, localDoc)
	target := preflightpkg.Target(core, projectDoc.EnabledPlugins)
	marketplaces := preflightpkg.EffectiveMarketplaces(userDoc, projectDoc, localDoc)

	return preflightReport{
		CatalogPath:  catalogPath,
		Current:      current,
		Target:       target,
		Marketplaces: marketplaces,
		ChangeSet:    preflightpkg.Diff(current, target),
	}, nil
}

// printPreflightReport writes the human-readable report.
func printPreflightReport(out io.Writer, r preflightReport) {
	fmt.Fprintf(out, "catalog: %s\n", r.CatalogPath)
	if r.ChangeSet.Aligned() {
		fmt.Fprintln(out, "aligned — no changes needed")
		return
	}
	if len(r.ChangeSet.Enable) > 0 {
		fmt.Fprintln(out, "enable:")
		for _, p := range r.ChangeSet.Enable {
			fmt.Fprintf(out, "  + %s\n", p)
		}
	}
	if len(r.ChangeSet.Disable) > 0 {
		fmt.Fprintln(out, "disable:")
		for _, p := range r.ChangeSet.Disable {
			fmt.Fprintf(out, "  - %s\n", p)
		}
	}
}

// preflightJSON is the --json wire shape for `preflight`. Enable/Disable
// arrays are never null (checked below), matching the `env check --json`
// empty-[] convention.
type preflightJSON struct {
	CatalogPath string   `json:"catalogPath"`
	Aligned     bool     `json:"aligned"`
	Enable      []string `json:"enable"`
	Disable     []string `json:"disable"`
}

// writePreflightJSON encodes r as preflightJSON to out.
func writePreflightJSON(out io.Writer, r preflightReport) error {
	enable, disable := r.ChangeSet.Enable, r.ChangeSet.Disable
	if enable == nil {
		enable = []string{}
	}
	if disable == nil {
		disable = []string{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(preflightJSON{
		CatalogPath: r.CatalogPath,
		Aligned:     r.ChangeSet.Aligned(),
		Enable:      enable,
		Disable:     disable,
	})
}
