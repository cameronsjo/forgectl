// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"strconv"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/theme"
)

// unknownSubcommand returns the error cobra would report for args when they
// name a command or subcommand that does not exist, in the two cases where
// cobra itself stays silent. It returns nil in every other case.
//
//   - Help was asked for. Cobra checks the help flag before it validates
//     positional arguments, so `forgectl nosuchcmd --help` and
//     `forgectl desk nosuchsub --help` printed help and exited 0
//     (forgectl#1080). A capability probe then read "the command exists".
//   - The command is a group with no Run of its own. Cobra answers it with help
//     before it looks at Args, so `forgectl surface lst` exited 0 (forgectl#1090).
//     rejectUnknownSubcommands gives such a group the validator; this runs it.
func unknownSubcommand(root *cobra.Command, args []string) error {
	// Lazy builtins (help, completion, man) are not in the tree yet. `completion`
	// is a group, so it is registered now and validated below like any other
	// (`completion nonesuch` must not exit 0).
	if first, _ := firstNonFlag(args); builtinVerbs[first] {
		if first != "completion" {
			return nil
		}
		root.InitDefaultCompletionCmd()
	}
	found, rest, err := root.Find(args)
	if found == nil {
		return nil
	}
	group := found.HasSubCommands() && !found.Runnable() && !parentTakesArg(found)
	// Parsing flags below has side effects: a slice flag would append twice once
	// cobra parses them again. A group never reaches a Run, so it is safe there.
	// Anything else is left alone unless help was asked for, when cobra stops at
	// help and no Run sees the flags either.
	if !group && !mentionsHelp(args) {
		return nil
	}
	found.InitDefaultHelpFlag()
	// An unparseable flag stays cobra's error to report.
	if found.ParseFlags(rest) != nil {
		return nil
	}
	if !group {
		if help, herr := found.Flags().GetBool("help"); herr != nil || !help {
			return nil
		}
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
	return asUsageError(found, found.ValidateArgs(positional))
}

// unknownCommandJSON answers an unknown verb or subverb asked for with --json
// (`forgectl zzbogus --json`, `forgectl desk zzbogus --json`) with the --json
// failure object, code unknown_command, instead of fang's frame. Cobra parses
// flags before it validates positionals, and a group declares no --json, so
// without this the error names the wrong problem ("unknown flag: --json") and
// a caller probing for a verb this forgectl lacks cannot tell it apart from a
// bad call. The exit code is the one the same call gets without --json.
//
// It returns nil, leaving cobra to report, unless --json is asked for (before
// any `--`), args resolve to a group (a command with subcommands that declares
// no --json and takes no positional of its own), and the group's validator
// refuses the first positional as an unknown command. Any other flag that does
// not parse stays cobra's to report.
func unknownCommandJSON(root *cobra.Command, args []string) error {
	if !argvAsksJSON(args) {
		return nil
	}
	if first, _ := firstNonFlag(args); builtinVerbs[first] && first != "completion" {
		return nil
	}
	found, rest, err := root.Find(args)
	if err != nil || found == nil || !found.HasSubCommands() || parentTakesArg(found) || found.Flags().Lookup("json") != nil {
		return nil
	}
	found.InitDefaultHelpFlag()
	// Parsing has side effects (a slice flag appends), but a group that
	// declares no --json never reaches a Run with --json in its argv: cobra
	// refuses the flag first.
	if found.ParseFlags(withoutJSONFlag(rest)) != nil {
		return nil
	}
	positional := found.Flags().Args()
	if len(positional) == 0 {
		return nil
	}
	verr := found.ValidateArgs(positional)
	var unknown *structuredTerminalError
	if verr == nil || !chainAs(verr, &unknown) {
		return nil
	}
	return jsonFailure(found, asUsageError(found, verr), true, jsonCodeUnknownCommand)
}

// argvAsksJSON reports whether args ask for --json before any `--`: a bare
// --json, or --json=<bool>, the last one winning.
func argvAsksJSON(args []string) bool {
	want := false
	for _, a := range args {
		switch {
		case a == "--":
			return want
		case a == "--json":
			want = true
		case strings.HasPrefix(a, "--json="):
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json=")); err == nil {
				want = v
			}
		}
	}
	return want
}

// withoutJSONFlag returns args with every --json and --json=<v> before any
// `--` removed, so a command that does not declare --json can parse the rest.
func withoutJSONFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			continue
		}
		out = append(out, a)
	}
	return out
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
