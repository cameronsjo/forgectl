package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// Execute is the binary's entrypoint. It normalizes argv (forgiveness layer),
// then either opens the TUI (bare invoke or an unrecognized verb — the thumb-
// mode affordance) or hands off to fang for styled help/errors/version.
func Execute(ctx context.Context) error {
	cfg := config.Load()
	closer := config.SetupLogger(cfg)
	defer closer.Close()

	slog.Debug("Starting forgectl.", "version", meta.Version)
	deps := module.Deps{Cfg: cfg, Runner: exec.OSRunner{}}
	// The bare-invoke TUI/runAction path keeps its own tmux client — clients
	// are stateless wrappers over the Runner, so a second instance is free and
	// the TUI stays decoupled from the module registry (ADR-0005: the menu is
	// a tmux session jumper, not a command palette).
	tmuxClient := tmux.New(exec.OSRunner{})
	root := newRoot(deps)
	args := normalizeArgs(os.Args[1:])

	// The launcher intercept runs before TUI/fang routing: `forgectl launch …`
	// (and its `cl` alias) must reach claude byte-clean for builder/agents
	// passthrough and open the interview when bare, bypassing Cobra flag
	// parsing. Own-verbs (which/edit/init/doctor/help) fall through to fang for
	// styled help. Only an inert global flag (--no-icons) may precede the token —
	// a root --help/--version must reach fang, not be skipped into the launcher.
	if rest, ok := launchIntercept(args); ok {
		if handled, err := runLaunch(cfg, rest); handled {
			// This path bypasses fang, which is what prints styled errors for
			// the normal command tree. Print here so an intercept error (e.g. a
			// bad FORGECTL_CLAUDE_BIN from ClaudePath) doesn't exit non-zero with
			// empty stderr — mirrors claunch's original main().
			if err != nil {
				fmt.Fprintln(os.Stderr, meta.AppName+": "+err.Error())
			}
			return err
		}
	}

	noIcons := cfg.NoIcons || hasNoIcons(args)

	if shouldLaunchTUI(root, args) {
		slog.Debug("Launching TUI.", "no_icons", noIcons)
		return runAction(ctx, tmuxClient, noIcons)
	}

	slog.Debug("Dispatching to command verb.", "verb", args)
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
		slog.Error("Failed to run TUI.", "error", err)
		return err
	}
	if act.Kind == tui.ActionNone {
		slog.Debug("TUI exited with no action.")
		return nil
	}
	return dispatchAction(ctx, client, act)
}

// dispatchAction routes a TUI action to the appropriate client call. Separated
// from runAction so it can be unit-tested without a real terminal.
func dispatchAction(ctx context.Context, client *tmux.Client, act tui.Action) error {
	switch act.Kind {
	case tui.ActionAttach:
		slog.Debug("Dispatching attach action.", "target", act.Target)
		return client.AttachOrSwitch(ctx, act.Target)
	case tui.ActionPick:
		slog.Debug("Dispatching pick action.", "target", act.Target)
		return client.Pick(ctx, act.Target)
	case tui.ActionLast:
		slog.Debug("Dispatching last session action.")
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
	// Known command group with an unrecognized leftover subverb → menu — but
	// only when the parent does NOT itself take a positional. A parent like
	// `pr <ref>` legitimately accepts an argument that is not a subcommand, so
	// its args must reach Cobra/fang rather than being mistaken for menu garbage;
	// a pure group like `tmux` treats an unknown token as a bad subverb → menu.
	if len(child.Commands()) > 0 && !parentTakesArg(child) {
		if sub, _ := firstNonFlag(args[idx+1:]); sub != "" && findChild(child, sub) == nil {
			return true
		}
	}
	return false
}

// launchIntercept returns the args following a leading `launch`/`cl` command
// token — allowing only inert global flags (--no-icons) before it — or ok=false
// when this invocation isn't a launcher passthrough. A root flag such as
// --help/--version is NOT inert: encountering one disables the shortcut so fang
// can handle it, rather than skipping past it into the launcher.
func launchIntercept(args []string) (rest []string, ok bool) {
	for i, a := range args {
		switch {
		case a == "launch" || a == "cl":
			return args[i+1:], true
		case a == "--no-icons" || strings.HasPrefix(a, "--no-icons="):
			continue
		default:
			return nil, false
		}
	}
	return nil, false
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

// parentTakesArg reports whether cmd's own invocation accepts a positional
// argument, as declared by a placeholder (`<…>` or `[…]`) after the verb in its
// Use line — e.g. `pr <ref>` takes one, `tmux` does not. This is what lets a
// runnable parent's positional reach Cobra instead of being menu-routed as an
// unknown subverb.
func parentTakesArg(cmd *cobra.Command) bool {
	_, rest, _ := strings.Cut(cmd.Use, " ")
	return strings.ContainsAny(rest, "<[")
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
