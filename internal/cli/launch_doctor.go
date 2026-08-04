package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
)

var (
	launchOKMark   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	launchWarnMark = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("!")
	launchFailMark = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("✗")
)

func newLaunchDoctorCmd(cfg config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check harness availability and launch config validity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			healthy := true

			lc, src := resolveLaunchConfig(cfg)

			profile := launch.DefaultsProfile(lc)
			if cwd, err := os.Getwd(); err == nil {
				profile = launch.Resolve(lc, cwd)
			}
			if err := profile.Validate(); err != nil {
				healthy = false
				fmt.Fprintf(out, "%s launch profile invalid: %v\n", launchFailMark, err)
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
					fmt.Fprintf(out, "%s [pr] config invalid: %v\n", launchFailMark, err)
				}
			}
			var binaryPath string
			var binaryErr error
			if profile.Harness == "codex" {
				binaryPath, binaryErr = launch.CodexPath(lc.Defaults)
			} else {
				binaryPath, binaryErr = launch.ClaudePath(lc.Defaults)
			}
			if binaryErr == nil {
				fmt.Fprintf(out, "%s %s found: %s\n", launchOKMark, profile.Harness, binaryPath)
			} else {
				healthy = false
				fmt.Fprintf(out, "%s %v\n", launchFailMark, binaryErr)
			}

			switch parseErr := config.Validate(); {
			case parseErr != nil:
				healthy = false
				fmt.Fprintf(out, "%s config failed to parse: %v\n", launchFailMark, parseErr)
			case lc.IsZero():
				if legacyErr := config.ValidateLegacyLaunch(); legacyErr != nil {
					healthy = false
					fmt.Fprintf(out, "%s legacy claunch config failed to parse: %v\n", launchFailMark, legacyErr)
				} else {
					fmt.Fprintf(out, "%s no launch profiles configured — using built-in defaults (run `forgectl launch init`)\n", launchWarnMark)
				}
			default:
				fmt.Fprintf(out, "%s launch config: %s (%d project profile(s))\n", launchOKMark, src, len(lc.Projects))
				if w := legacyShadowWarning(cfg); w != "" {
					fmt.Fprintf(out, "%s %s\n", launchWarnMark, w)
				}
			}

			// Bench telemetry injection is informational, not a health signal —
			// off is a valid choice (a machine with no local collector).
			if cfg.Bench.Telemetry {
				fmt.Fprintf(out, "%s telemetry: on → %s (%s)\n", launchOKMark,
					cfg.Bench.ResolvedOTLPEndpoint(), cfg.Bench.ResolvedOTLPProtocol())
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
