//go:build !unix

package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

func newProjectGoalsCmd(_ module.Deps, _ *string) *cobra.Command {
	return unsupportedProjectGoalsCommand("goals")
}

func newProjectGoalCmd(_ module.Deps, _ *string) *cobra.Command {
	return unsupportedProjectGoalsCommand("goal")
}

func unsupportedProjectGoalsCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: "Project goals are unsupported on this platform",
		Args:  cobra.ArbitraryArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("project goals are unsupported on this platform")
		},
	}
}
