package cli

import (
	"fmt"

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
		Short: "Check claude availability and launch config validity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			healthy := true

			lc, src := resolveLaunchConfig(cfg)

			if p, err := launch.ClaudePath(lc.Defaults); err == nil {
				fmt.Fprintf(out, "%s claude found: %s\n", launchOKMark, p)
			} else {
				healthy = false
				fmt.Fprintf(out, "%s %v\n", launchFailMark, err)
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
			}

			if !healthy {
				return fmt.Errorf("doctor found problems")
			}
			return nil
		},
	}
}
