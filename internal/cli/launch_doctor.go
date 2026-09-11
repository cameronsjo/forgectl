package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func newLaunchDoctorCmd(boundary *config.LegacyMigrationBoundary, cfg config.Config, th theme.Theme) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check harness availability and launch config validity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := th.Writer(cmd.OutOrStdout(), os.Environ())
			marks := th.Marks()
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
				_, _ = fmt.Fprintf(out, "%s launch profile invalid: %s\n", marks.Fail, termsafe.SafeLine(err.Error()))
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
					_, _ = fmt.Fprintf(out, "%s [pr] config invalid: %s\n", marks.Fail, termsafe.SafeLine(err.Error()))
				}
			}
			resolvedBinary, binaryErr := launch.ResolveBinary(profile.Harness, lc.Defaults)
			binaryPath := resolvedBinary.Path
			if binaryErr == nil {
				_, _ = fmt.Fprintf(out, "%s %s found: %s\n", marks.OK, termsafe.SafeLine(profile.Harness), termsafe.QuotePath(binaryPath))
			} else {
				healthy = false
				_, _ = fmt.Fprintf(out, "%s %s\n", marks.Fail, termsafe.SafeLine(binaryErr.Error()))
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
				_, _ = fmt.Fprintf(out, "%s %s\n", marks.Warn, termsafe.SafeLine(notice))
			}
			switch {
			case parseErr != nil:
				healthy = false
				_, _ = fmt.Fprintf(out, "%s config failed to parse: %s\n", marks.Fail, termsafe.SafeLine(parseErr.Error()))
			case !cfg.HasLaunchSection() && lc.IsZero():
				var legacyErr error
				if boundary != nil && boundary.Status != config.BoundaryNoSource {
					_, legacyErr = boundary.LoadReadOnlyLegacy()
				}
				if legacyErr != nil {
					healthy = false
					_, _ = fmt.Fprintf(out, "%s legacy claunch config failed to parse: %s\n", marks.Fail, termsafe.SafeLine(legacyErr.Error()))
				} else {
					_, _ = fmt.Fprintf(out, "%s no launch profiles configured — using built-in defaults (run `forgectl launch init`)\n", marks.Warn)
				}
				// #417: name a config file in the legacy directory that
				// forgectl cannot migrate, so "no profiles configured" does
				// not read as "nothing is there".
				if sibling := boundary.UnmigratableSiblingPath(); sibling != "" {
					_, _ = fmt.Fprintf(out, "%s %s is present but forgectl cannot migrate it — it migrates the historical claunch.conf format only\n",
						marks.Warn, termsafe.QuotePath(sibling))
				}
			default:
				_, _ = fmt.Fprintf(out, "%s launch config: %s (%d project profile(s))\n", marks.OK, termsafe.QuotePath(src), len(lc.Projects))
			}

			if !reportUsageStats(out, usageEnabled, marks) {
				healthy = false
			}

			// Bench telemetry injection is informational, not a health signal —
			// off is a valid choice (a machine with no local collector).
			if cfg.Bench.Telemetry {
				_, _ = fmt.Fprintf(out, "%s telemetry: on → %s (%s)\n", marks.OK,
					termsafe.SafeLine(cfg.Bench.ResolvedOTLPEndpoint()), termsafe.SafeLine(cfg.Bench.ResolvedOTLPProtocol()))
			} else {
				_, _ = fmt.Fprintf(out, "%s telemetry: off (enable with [bench].telemetry = true)\n", marks.Warn)
			}

			if !healthy {
				return fmt.Errorf("doctor found problems")
			}
			return nil
		},
	}
}
