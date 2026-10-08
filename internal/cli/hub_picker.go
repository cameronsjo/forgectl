package cli

import (
	"errors"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tui"
)

// errPickerSubcommand is the picker's refusal for a value that would run a
// different command than the row names. Categorical: it never echoes the value.
var errPickerSubcommand = errors.New("that value names a subcommand; pick it from the list instead")

// errPickerOptedOut is the picker's refusal for a command carrying
// hubNoPickerAnnotation: its argument is another CLI's subcommand, so no
// typed value is offered there at all.
var errPickerOptedOut = errors.New("this command takes no value from the picker; finish it by hand")

// hubPickerArgv builds the hub picker's argv against the live command tree
// (forgectl#730 review). tui.PickerArgv validates the value as one element;
// this adds the three things only the tree knows:
//
//   - Refusal by resolution. runHubVerb re-dispatches the argv exactly as a
//     typed command, so a value equal to a child's name or alias would run
//     that child — "drain" in the `pr <ref>` picker is `forgectl pr drain`,
//     which launches every queued review. The argv is resolved the way
//     dispatch resolves it (hubDispatchTarget: the launch intercept, then
//     cobra's own Find) and refused unless it lands on the row's command.
//     No list of names is kept here, so a future subcommand is covered.
//   - Refusal by opt-out. A command carrying hubNoPickerAnnotation passes
//     its argument into another CLI's subcommand slot; its rows never open
//     the picker (NoPicker), and the builder refuses it in case one does.
//   - A "--" before the value where the command's flag parsing honors it, so
//     cobra stops looking for subcommands and flags at the value even if the
//     resolution check were ever wrong.
func hubPickerArgv(root *cobra.Command) tui.ArgvBuilder {
	return func(prefix []string, arg string, optional bool) ([]string, error) {
		argv, err := tui.PickerArgv(prefix, arg, optional)
		if err != nil {
			return nil, err
		}
		want := hubDispatchRoute(root, prefix)
		if want.cmd == nil || !slices.Equal(commandArgv(want.cmd), prefix) {
			return nil, errors.New("this command is not in the command tree")
		}
		if hasNoPickerAnnotation(want.cmd) {
			return nil, errPickerOptedOut
		}
		if arg == "" {
			return argv, nil
		}
		if hubDispatchRoute(root, argv) != want {
			return nil, errPickerSubcommand
		}
		if !dashTerminatorSafe(root, want.cmd) {
			return argv, nil
		}
		dashed := append(append(append([]string(nil), prefix...), "--"), arg)
		if hubDispatchRoute(root, dashed) != want {
			return nil, errPickerSubcommand
		}
		return dashed, nil
	}
}

// hubRoute is where runHubVerb sends an argv: the command it lands on, and
// whether the launch intercept execs the harness with it (launcher) rather
// than handing it to the command tree. `launch x` and `launch help` both
// land on the launch command, by different routes.
type hubRoute struct {
	cmd      *cobra.Command
	launcher bool
}

// hubDispatchRoute mirrors runHubVerb's order: launchIntercept first — whose
// rest goes to the harness launcher unless it opens with one of launch's own
// verbs, which fang then dispatches through the tree — then cobra's own Find.
// A zero cmd means argv resolves to no command.
func hubDispatchRoute(root *cobra.Command, argv []string) hubRoute {
	if rest, ok := launchIntercept(argv); ok && (len(rest) == 0 || !isOwnLaunchVerb(rest[0])) {
		return hubRoute{cmd: findChild(root, "launch"), launcher: true}
	}
	cmd, _, err := root.Find(argv)
	if err != nil || cmd == root {
		return hubRoute{}
	}
	return hubRoute{cmd: cmd}
}

// hubDispatchTarget is hubDispatchRoute's command alone.
func hubDispatchTarget(root *cobra.Command, argv []string) *cobra.Command {
	return hubDispatchRoute(root, argv).cmd
}

// dashTerminatorSafe reports whether inserting "--" before a picked value
// leaves target's parse unchanged: cobra parses its flags (pflag drops the
// terminator, so the positional arrives as it did), the launch intercept does
// not take the argv (it would hand "--" to the harness), and its Use does not
// give "--" a meaning of its own (docker's `[-- args...]`, read through
// ArgsLenAtDash).
func dashTerminatorSafe(root *cobra.Command, target *cobra.Command) bool {
	if target.DisableFlagParsing || target == findChild(root, "launch") {
		return false
	}
	return !strings.Contains(target.Use, "[-- ")
}
