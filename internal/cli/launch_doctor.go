package cli

import (
	"errors"
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

var (
	launchOKMark   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	launchWarnMark = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("!")
	launchFailMark = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("✗")
)

func newLaunchDoctorCmd(boundary *config.LegacyMigrationBoundary, cfg config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check harness availability and launch config validity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := colorOut(cmd)
			healthy := true

			// Same rule as launchExec: the opt-in is whatever config.toml
			// declared at process start, never what a migration produced.
			usageEnabled := cfg.Launch.UsageStats

			effLaunch, notice, effFrom := autoMigrateOrWarnLegacyLaunch(boundary, cfg)
			cfg.Launch = effLaunch

			lc, src := resolveLaunchConfig(boundary, cfg, effFrom)

			profile := launch.DefaultsProfile(lc)
			if cwd, err := os.Getwd(); err == nil {
				profile = launch.Resolve(lc, cwd)
			}
			if err := profile.Validate(); err != nil {
				healthy = false
				fmt.Fprintf(out, "%s launch profile invalid: %s\n", launchFailMark, termsafe.SafeLine(err.Error()))
			}
			// [pr] effort is validated separately because it never enters the
			// resolved [launch] profile above — it is applied inside the review
			// dispatch. Without this, the one setting whose failure is INVISIBLE
			// at runtime (a review runs in a detached tmux window, so a value
			// claude rejects is an empty pane and no error) is also the one
			// doctor cannot pre-flight. Harness is pinned to claude because the
			// review dispatch forces it there regardless of the ambient profile.
			if cfg.Pr.Effort != "" {
				if err := (launch.Profile{Harness: "claude", Effort: cfg.Pr.Effort}).Validate(); err != nil {
					healthy = false
					fmt.Fprintf(out, "%s [pr] config invalid: %s\n", launchFailMark, termsafe.SafeLine(err.Error()))
				}
			}
			resolvedBinary, binaryErr := launch.ResolveBinary(profile.Harness, lc.Defaults)
			binaryPath := resolvedBinary.Path
			if binaryErr == nil {
				fmt.Fprintf(out, "%s %s found: %s\n", launchOKMark, termsafe.SafeLine(profile.Harness), termsafe.QuotePath(binaryPath))
			} else {
				healthy = false
				fmt.Fprintf(out, "%s %s\n", launchFailMark, termsafe.SafeLine(binaryErr.Error()))
			}

			configPath := ""
			if boundary != nil {
				configPath = boundary.ConfigPath
			} else {
				configPath, _ = config.ConfigPath()
			}
			var parseErr error
			if boundary == nil || !errors.Is(boundary.Refusal, config.ErrLegacyPathControl) {
				parseErr = config.ValidatePath(configPath)
			}
			// The notice prints ahead of the switch, not inside one arm. A
			// legacy file forgectl models nothing of leaves lc zero, which
			// takes the "no launch profiles configured" arm — so gating the
			// notice on the default arm silently dropped it for the exact
			// input class #417 is about (#418 review).
			if notice != "" {
				_, _ = fmt.Fprintf(out, "%s %s\n", launchWarnMark, termsafe.SafeLine(notice))
			}
			switch {
			case parseErr != nil:
				healthy = false
				fmt.Fprintf(out, "%s config failed to parse: %s\n", launchFailMark, termsafe.SafeLine(parseErr.Error()))
			case !cfg.HasLaunchSection() && lc.IsZero():
				var legacyErr error
				if boundary != nil && boundary.Status != config.BoundaryNoSource {
					_, legacyErr = boundary.LoadReadOnlyLegacy()
				}
				if legacyErr != nil {
					healthy = false
					fmt.Fprintf(out, "%s legacy claunch config failed to parse: %s\n", launchFailMark, termsafe.SafeLine(legacyErr.Error()))
				} else {
					fmt.Fprintf(out, "%s no launch profiles configured — using built-in defaults (run `forgectl launch init`)\n", launchWarnMark)
				}
				// #417: name a config file in the legacy directory that
				// forgectl cannot migrate, so "no profiles configured" does
				// not read as "nothing is there".
				if sibling := boundary.UnmigratableSiblingPath(); sibling != "" {
					_, _ = fmt.Fprintf(out, "%s %s is present but forgectl cannot migrate it — it migrates the historical claunch.conf format only\n",
						launchWarnMark, termsafe.QuotePath(sibling))
				}
			default:
				fmt.Fprintf(out, "%s launch config: %s (%d project profile(s))\n", launchOKMark, termsafe.QuotePath(src), len(lc.Projects))
			}

			if !reportUsageStats(out, usageEnabled) {
				healthy = false
			}

			// Bench telemetry injection is informational, not a health signal —
			// off is a valid choice (a machine with no local collector).
			if cfg.Bench.Telemetry {
				fmt.Fprintf(out, "%s telemetry: on → %s (%s)\n", launchOKMark,
					termsafe.SafeLine(cfg.Bench.ResolvedOTLPEndpoint()), termsafe.SafeLine(cfg.Bench.ResolvedOTLPProtocol()))
			} else {
				fmt.Fprintf(out, "%s telemetry: off (enable with [bench].telemetry = true)\n", launchWarnMark)
			}

			if !healthy {
				return fmt.Errorf("doctor found problems")
			}
			return nil
		},
	}
}
