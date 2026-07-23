package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

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

WARNING: --apply writes the COMPLETE aligned set, not a delta — any
plugin you currently have enabled that is neither catalog-core-tier NOR
named in this project's own committed .claude/settings.json gets DISABLED
for this project. On a project with no committed enabledPlugins at all,
that means every plugin outside the catalog's core tier.

preflight's only write target is .claude/settings.local.json — the
personal, auto-gitignored override scope. The committed .claude/settings.json
is read-only input, never touched. Marketplace SOURCES (extraKnownMarketplaces)
written on --apply come ONLY from your own user/local settings — never from
a project's committed file, so a cloned repo can enable a plugin name but
can never register a marketplace on your behalf; an enabled plugin whose
marketplace isn't already registered by you is reported, not silently
skipped or silently trusted.

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
// catalog, the current/target enabledPlugins sets, the trust-filtered
// marketplaces --apply will write, any target plugin whose marketplace has
// no trusted registration, and the computed change-set.
type preflightReport struct {
	CatalogPath             string
	Current                 map[string]bool
	Target                  map[string]bool
	Marketplaces            map[string]json.RawMessage
	UnregisteredMarketplace []string
	ChangeSet               preflightpkg.ChangeSet
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

	// Marketplace SOURCES draw ONLY from user/local scope — never project.
	// A cloned repo's committed .claude/settings.json can fold a plugin
	// name into target above (locked decision 2), but it must never be
	// able to smuggle in the marketplace SOURCE that plugin resolves
	// against. See FilterMarketplaces/TrustedMarketplaces.
	trusted := preflightpkg.TrustedMarketplaces(userDoc, localDoc)
	marketplaces, unregistered := preflightpkg.FilterMarketplaces(target, trusted)

	return preflightReport{
		CatalogPath:             catalogPath,
		Current:                 current,
		Target:                  target,
		Marketplaces:            marketplaces,
		UnregisteredMarketplace: unregistered,
		ChangeSet:               preflightpkg.Diff(current, target),
	}, nil
}

// sortedMarketplaceNames returns m's keys sorted — the shared rendering
// helper for both the human and --json report of what --apply will write to
// extraKnownMarketplaces.
func sortedMarketplaceNames(m map[string]json.RawMessage) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// printPreflightReport writes the human-readable report. Marketplace
// changes are always shown when present — even when enabledPlugins is
// already aligned — because nothing about extraKnownMarketplaces may change
// silently on --apply (the security fix this report format exists for).
func printPreflightReport(out io.Writer, r preflightReport) {
	fmt.Fprintf(out, "catalog: %s\n", r.CatalogPath)
	if r.ChangeSet.Aligned() && len(r.UnregisteredMarketplace) == 0 {
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
	if len(r.Marketplaces) > 0 {
		fmt.Fprintln(out, "marketplaces (written on --apply, from your own user/local settings):")
		for _, name := range sortedMarketplaceNames(r.Marketplaces) {
			fmt.Fprintf(out, "  %s\n", name)
		}
	}
	if len(r.UnregisteredMarketplace) > 0 {
		fmt.Fprintln(out, "enabled but marketplace unregistered — will not load until you register it yourself:")
		for _, p := range r.UnregisteredMarketplace {
			fmt.Fprintf(out, "  ! %s\n", p)
		}
	}
}

// preflightJSON is the --json wire shape for `preflight`. Every array is
// never null (checked below), matching the `env check --json` empty-[]
// convention.
type preflightJSON struct {
	CatalogPath             string   `json:"catalogPath"`
	Aligned                 bool     `json:"aligned"`
	Enable                  []string `json:"enable"`
	Disable                 []string `json:"disable"`
	Marketplaces            []string `json:"marketplaces"`            // marketplace names --apply will write, sourced only from user/local settings
	UnregisteredMarketplace []string `json:"unregisteredMarketplace"` // "plugin@marketplace" enabled but skipped — no trusted marketplace registration
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
	unregistered := r.UnregisteredMarketplace
	if unregistered == nil {
		unregistered = []string{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(preflightJSON{
		CatalogPath:             r.CatalogPath,
		Aligned:                 r.ChangeSet.Aligned(),
		Enable:                  enable,
		Disable:                 disable,
		Marketplaces:            sortedMarketplaceNames(r.Marketplaces),
		UnregisteredMarketplace: unregistered,
	})
}
