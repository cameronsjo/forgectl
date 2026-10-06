// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// unknownCommandWithHelp returns the error cobra would report for args when
// they name a command or subcommand that does not exist, but only when args
// also ask for help. It returns nil in every other case.
//
// Cobra checks the help flag before it validates positional arguments, so
// `forgectl nosuchcmd --help` and `forgectl desk nosuchsub --help` print help
// and exit 0 (forgectl#1080). A capability probe written the natural way then
// reads "the command exists". This runs the validation cobra skips.
func unknownCommandWithHelp(root *cobra.Command, args []string) error {
	// Parsing flags below has side effects (a slice flag would append twice
	// once cobra parses them again), so leave every run without a help token
	// alone.
	if !mentionsHelp(args) {
		return nil
	}
	// Lazy builtins (help, completion, man) are not in the tree yet.
	if first, _ := firstNonFlag(args); builtinVerbs[first] {
		return nil
	}
	found, rest, err := root.Find(args)
	if found == nil {
		return nil
	}
	found.InitDefaultHelpFlag()
	// An unparseable flag stays cobra's error to report.
	if found.ParseFlags(rest) != nil {
		return nil
	}
	if help, herr := found.Flags().GetBool("help"); herr != nil || !help {
		return nil
	}
	if err != nil {
		return err
	}
	// Only a command that has subcommands can be handed an unknown one. A leaf
	// that wants a positional (`pr <ref>`) still prints help without it.
	positional := found.Flags().Args()
	if !found.HasSubCommands() || len(positional) == 0 {
		return nil
	}
	return found.ValidateArgs(positional)
}

// mentionsHelp reports whether args hold a help flag before any `--`: the long
// form, with or without a value, or -h alone or inside a shorthand cluster.
// It may say yes when a value token merely looks like one; the caller's parse
// is the authority.
func mentionsHelp(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--":
			return false
		case a == "--help" || strings.HasPrefix(a, "--help="):
			return true
		case len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'h'):
			return true
		}
	}
	return false
}

// renderCommandError renders err through fang, so it gets the same styled frame
// and terminal-safety filter as any error a command returns. A throwaway
// command carries it, because fang only renders what a command's RunE returns.
func renderCommandError(ctx context.Context, root *cobra.Command, th theme.Theme, err error) error {
	shim := &cobra.Command{
		Use:           root.Use,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return err },
	}
	shim.SetOut(root.OutOrStdout())
	shim.SetErr(root.ErrOrStderr())
	// Empty, not nil: nil makes cobra read os.Args.
	shim.SetArgs([]string{})
	return fang.Execute(ctx, shim, fangOptions(root.Version, "", th)...)
}
