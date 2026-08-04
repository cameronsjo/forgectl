package cli

import (
	"fmt"
	"io"
	"os"

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
			if w := legacyShadowWarning(cfg); w != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "forgectl: "+w)
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
	row("harness", p.Harness)
	row("model", p.Model)
	if p.Harness == "codex" {
		row("approval", p.ApprovalPolicy)
		row("sandbox", p.Sandbox)
	} else {
		// Omitted rather than shown empty when no level resolved: an absent
		// --effort means Claude Code's own default applies, which is a
		// different statement from "effort is blank". Matches the conditional
		// env/add-dir rows below.
		if p.Effort != "" {
			row("effort", p.Effort)
		}
		row("permission", p.PermissionMode)
		row("allow danger", fmt.Sprintf("%t", p.AllowDanger))
	}

	// Env renders its sorted KEY NAMES only, never its values: this is arbitrary
	// environment injected into the launched harness, so it is where an
	// ANTHROPIC_API_KEY or GH_TOKEN sits, and `which` output is the kind of
	// thing pasted into an issue or a terminal share. The key names carry the
	// signal an operator needs here — which variables the profile injects.
	// Same policy, same rendering as the sibling surface: leafValue's
	// reflect.Map arm in internal/cli/config_cmd.go.
	if len(p.Env) > 0 {
		row("env", redactedMapDisplay(launch.SortedEnvKeys(p.Env)))
	}
	for i, d := range p.AddDir {
		label := ""
		if i == 0 {
			label = "add-dir"
		}
		row(label, d)
	}
}
