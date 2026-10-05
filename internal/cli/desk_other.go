// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/module"
)

// deskModule declares the desk. The desk needs Unix primitives, so on this
// platform every desk verb reports desk.ErrUnsupported; the verbs and flags
// are still declared, so a caller gets that error rather than an unknown
// command, and a --json caller gets it as JSON.
var deskModule = module.Manifest{
	Name: "desk",
	Tier: module.TierExtension,
	New:  newDeskCmd,
}

func newDeskCmd(module.Deps) *cobra.Command {
	unsupported := func(*cobra.Command, []string) error { return desk.ErrUnsupported }
	cmd := &cobra.Command{
		Use:   "desk",
		Short: "Approval queue and run viewer (Unix only)",
		Args:  cobra.NoArgs,
		RunE:  unsupported,
	}
	cmd.PersistentFlags().StringArray("spec", nil, "a run spec file whose runs to add (repeatable; experimental format)")
	runs := &cobra.Command{Use: "runs", Short: "List every run from every source (Unix only)", Args: cobra.NoArgs, RunE: unsupported}
	runs.Flags().Bool("json", false, "emit the run list as JSON")
	show := &cobra.Command{Use: "show <run>", Short: "Print one run (Unix only)", Args: cobra.ExactArgs(1), RunE: unsupported}
	show.Flags().String("from", "", "the source that holds the run")
	show.Flags().Int("at", 0, "replay: the state after the run's first N events")
	show.Flags().Bool("json", false, "emit the run's state as JSON")
	cmd.AddCommand(runs, show, newDeskSuperviseCmd())
	return cmd
}
