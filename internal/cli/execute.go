package cli

import (
	"context"
	"os"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// Execute is the binary's entrypoint. It normalizes argv (forgiveness layer),
// then either opens the TUI (bare invoke or an unrecognized verb — the thumb-
// mode affordance) or hands off to fang for styled help/errors/version.
func Execute(ctx context.Context) error {
	client := tmux.New(exec.OSRunner{})
	root := newRoot(client)
	args := normalizeArgs(os.Args[1:])

	if shouldLaunchTUI(root, args) {
		return runAction(ctx, client, hasNoIcons(args))
	}

	root.SetArgs(args)
	return fang.Execute(ctx, root,
		fang.WithVersion(meta.Version),
		fang.WithCommit(meta.Commit),
	)
}

// runAction opens the TUI and performs whatever jump it selected. Jumps that
// need the tty (attach / sesh connect) run here, after Bubble Tea has released
// the terminal.
func runAction(ctx context.Context, client *tmux.Client, noIcons bool) error {
	act, err := tui.Run(ctx, client, noIcons)
	if err != nil {
		return err
	}
	switch act.Kind {
	case tui.ActionAttach:
		return client.AttachOrSwitch(ctx, act.Target)
	case tui.ActionPick:
		return client.Pick(ctx, act.Target)
	case tui.ActionLast:
		return client.LastSession(ctx)
	}
	return nil
}

// builtinVerbs are commands Cobra/fang register lazily at Execute time, so they
// aren't in root.Commands() when we route. They must never fall into the menu.
var builtinVerbs = map[string]bool{
	"help": true, "completion": true,
	"__complete": true, "__completeNoDesc": true,
}

// shouldLaunchTUI decides whether to open the menu instead of dispatching a
// verb: bare invocation, an unknown top-level verb, or an unknown subverb of a
// command group (e.g. `tmux frobnicate`). Flag-only invocations (--version,
// --help) stay with fang — only non-flag garbage falls into the menu. The
// check is against the live command/alias set (not root.Find), so it's immune
// to Cobra's lazy registration of help/completion during Execute.
func shouldLaunchTUI(root *cobra.Command, args []string) bool {
	first, idx := firstNonFlag(args)
	if first == "" {
		// Flags only (--version/--help) → fang; truly empty → bare-invoke menu.
		return len(args) == 0
	}
	if builtinVerbs[first] {
		return false
	}
	child := findChild(root, first)
	if child == nil {
		return true // unknown top-level verb → menu
	}
	// Known command group with an unrecognized leftover subverb → menu.
	if len(child.Commands()) > 0 {
		if sub, _ := firstNonFlag(args[idx+1:]); sub != "" && findChild(child, sub) == nil {
			return true
		}
	}
	return false
}

// firstNonFlag returns the first non-flag token and its index (or "", -1).
func firstNonFlag(args []string) (string, int) {
	for i, a := range args {
		if !isFlag(a) {
			return a, i
		}
	}
	return "", -1
}

// findChild resolves name against a command's subcommands by name or alias.
func findChild(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

// hasNoIcons detects the --no-icons flag in raw argv (the pre-Cobra TUI launch
// path can't read parsed flags yet). Matches both bare and --no-icons=<v> forms.
func hasNoIcons(args []string) bool {
	for _, a := range args {
		if a == "--no-icons" || strings.HasPrefix(a, "--no-icons=") {
			return true
		}
	}
	return false
}
