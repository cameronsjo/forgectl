package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

const (
	herdrPaneIDEnv       = "HERDR_PANE_ID"
	herdrActivePaneIDEnv = "HERDR_ACTIVE_PANE_ID"
)

var recipeModule = module.Manifest{
	Name:         "recipe",
	Tier:         module.TierExtension,
	ConfigKey:    "",
	GroupAliases: []string{"r"},
	New:          newRecipeCmd,
}

var lookupRecipeEnv = os.LookupEnv

type recipeAfkOptions struct {
	Target string
}

func newRecipeCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "recipe",
		Aliases: []string{"r"},
		Short:   "Run small built-in workbench recipes",
	}
	cmd.AddCommand(newRecipeAfkCmd(deps))
	return cmd
}

func newRecipeAfkCmd(deps module.Deps) *cobra.Command {
	var opts recipeAfkOptions
	cmd := &cobra.Command{
		Use:   "afk",
		Short: "Journal and compact the current Herdr agent pane",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecipeAfk(cmd.Context(), deps.Runner, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Target, "target", "", "Herdr agent name or pane id (defaults to HERDR_PANE_ID, then HERDR_ACTIVE_PANE_ID)")
	return cmd
}

func runRecipeAfk(ctx context.Context, runner exec.Runner, opts recipeAfkOptions) error {
	target, ok := resolveRecipeHerdrTarget(opts.Target)
	if !ok {
		return WithExitCode(fmt.Errorf("no Herdr target found; pass --target or run inside a Herdr pane with %s or %s set", herdrPaneIDEnv, herdrActivePaneIDEnv), 2)
	}

	if _, err := runner.Run(ctx, "herdr", "agent", "prompt", target, "/journal", "--wait"); err != nil {
		return fmt.Errorf("run /journal through herdr agent prompt: %w", err)
	}
	if _, err := runner.Run(ctx, "herdr", "agent", "type-submit", target, "/compact"); err != nil {
		return fmt.Errorf("run /compact through herdr agent type-submit: %w", err)
	}
	return nil
}

func resolveRecipeHerdrTarget(explicit string) (string, bool) {
	if explicit != "" {
		return explicit, true
	}
	if value, ok := lookupRecipeEnv(herdrPaneIDEnv); ok && value != "" {
		return value, true
	}
	if value, ok := lookupRecipeEnv(herdrActivePaneIDEnv); ok && value != "" {
		return value, true
	}
	return "", false
}
