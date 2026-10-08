// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import "github.com/spf13/cobra"

// rejectUnknownSubcommands makes every command group refuse a subcommand it does
// not have (forgectl#1090). A group that declares no Args and has no Run of its
// own got cobra's "accept anything" default: `forgectl surface lst` printed the
// group's help and exited 0. A group that declared cobra.NoArgs refused, but
// with no "did you mean" line. A group's suggestion distance also defaulted to
// exact match, so most groups offered no suggestion at all.
//
// It walks the finished tree, so a module added later is covered with no
// per-group wiring. A group is any non-root command with subcommands that does
// not take a positional of its own (`pr <ref>`). A group run bare keeps
// printing its help and exiting 0.
//
// Cobra answers a non-runnable group with help before it validates Args, so
// unknownSubcommand (unknown_help.go) runs the validator for those. A runnable
// group's Args runs as usual.
func rejectUnknownSubcommands(root *cobra.Command) {
	for _, c := range root.Commands() {
		if c.HasSubCommands() && !parentTakesArg(c) {
			c.Args = groupArgs(c.Args)
			// SuggestionsFor reads the group's own distance, and 0 means exact
			// match only: the "did you mean" line was uneven across groups.
			if c.SuggestionsMinimumDistance == 0 {
				c.SuggestionsMinimumDistance = 2
			}
		}
		rejectUnknownSubcommands(c)
	}
}

// groupArgs returns the validator for a group: prev (when the group declared
// one) decides, and a positional prev refuses becomes the unknown-subcommand
// error with its "did you mean" line. A group declares no positional of its own
// (rejectUnknownSubcommands skips one that does), so a rejected one is a verb
// that does not exist.
func groupArgs(prev cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if prev != nil {
			err := prev(cmd, args)
			if err == nil || len(args) == 0 {
				return err
			}
		}
		return safeRootArgs(cmd, args)
	}
}
