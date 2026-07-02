package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
)

var (
	launchLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Width(14)
	launchValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	launchTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("110"))
	launchDimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true)
)

func newLaunchWhichCmd(cfg config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "which",
		Short: "Print the resolved launch profile for the current directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("determine working directory: %w", err)
			}
			lc, src := resolveLaunchConfig(cfg)
			printLaunchProfile(cmd.OutOrStdout(), launch.Resolve(lc, cwd), cwd, src)
			return nil
		},
	}
}

func printLaunchProfile(w io.Writer, p launch.Profile, cwd, confPath string) {
	row := func(label, value string) {
		_, _ = fmt.Fprintln(w, launchLabelStyle.Render(label)+launchValueStyle.Render(value))
	}

	_, _ = fmt.Fprintln(w, launchTitleStyle.Render("launch profile")+launchDimStyle.Render("  "+cwd))
	row("config", confPath)

	matched := p.Match
	if matched == "" {
		matched = launchDimStyle.Render("(defaults only)")
	}
	row("matched", matched)
	row("model", p.Model)
	row("permission", p.PermissionMode)
	row("allow danger", fmt.Sprintf("%t", p.AllowDanger))

	if len(p.Env) > 0 {
		keys := launch.SortedEnvKeys(p.Env)
		pairs := make([]string, len(keys))
		for i, k := range keys {
			pairs[i] = k + "=" + p.Env[k]
		}
		row("env", strings.Join(pairs, "  "))
	}
	for i, d := range p.AddDir {
		label := ""
		if i == 0 {
			label = "add-dir"
		}
		row(label, d)
	}
}
