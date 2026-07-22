package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// errHeadlessMenuRoute is returned when a non-interactive invocation would
// have opened the TUI menu but Cobra/fang handled it as a silent success
// instead — a bare invoke, or a known parent swallowing an unrecognized
// subverb into its own help, both print via flag.ErrHelp and return nil.
// Left alone that would exit 0 having dispatched nothing; this turns it into
// a failure so scripts/CI/an agent driving forgectl headlessly can tell.
var errHeadlessMenuRoute = errors.New("forgectl: no command to run outside a terminal; see the usage above")

// isInteractiveTTY is the TUI's TTY gate — package-level so decideRoute's
// callers (and tests, via the plain-bool seam below) don't need a real
// terminal. Bubble Tea needs both stdin (input) and stdout (the drawn
// screen); env.go's isTerminal only gates a single stdin prompt, so this is
// a separate check rather than a reuse of that seam.
var isInteractiveTTY = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

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

	switch decideRoute(root, args, isInteractiveTTY()) {
	case routeTUI:
		slog.Debug("Launching TUI.", "no_icons", noIcons)
		return runAction(ctx, tmuxClient, noIcons)
	case routeHeadlessMenu:
		// Route through Cobra/fang instead of the TUI: an unrecognized
		// top-level verb hits cobra's own "unknown command" + "did you mean"
		// suggestion path — previously unreachable, since the TUI intercepted
		// unknown verbs before Cobra ever saw them — which already returns a
		// non-nil error. A bare invoke or a bad subverb of a known parent
		// prints help and returns nil instead; errHeadlessMenuRoute turns
		// that into a failure so headless callers never read silence as
		// success.
		slog.Debug("Headless; routing to Cobra/fang instead of the TUI.", "verb", args)
		root.SetOut(os.Stderr)
		if err := execCommand(ctx, root, args); err != nil {
			return err
		}
		return errHeadlessMenuRoute
	default:
		slog.Debug("Dispatching to command verb.", "verb", args)
		return execCommand(ctx, root, args)
	}
}

// execCommand hands args to Cobra via fang, which renders styled help,
// errors, and version output. Shared by the normal-dispatch and
// headless-menu-route paths in Execute; the only difference between them is
// where fang writes output, which the caller sets via root.SetOut first.
func execCommand(ctx context.Context, root *cobra.Command, args []string) error {
	root.SetArgs(args)
	return fang.Execute(ctx, root, fangVersionOptions(meta.Version, meta.Commit)...)
}

// fangVersionOptions builds the fang.Option set that seeds root.Version
// before Execute runs. Extracted so TestVersion_VerbMatchesFlagThroughFang
// can call the exact same wiring instead of a parallel hand-rolled copy —
// an option added here is automatically exercised by that regression guard
// too.
func fangVersionOptions(version, commit string) []fang.Option {
	return []fang.Option{
		fang.WithVersion(version),
		fang.WithCommit(commit),
	}
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

// menuRoute is Execute's routing decision for a parsed argv.
type menuRoute int

const (
	routeDispatch     menuRoute = iota // known verb — normal Cobra/fang dispatch
	routeTUI                           // menu-eligible and interactive — draw the menu
	routeHeadlessMenu                  // menu-eligible but non-interactive — Cobra/fang, no TUI
)

// decideRoute combines shouldLaunchTUI with the TTY gate, factored out as a
// plain function over a bool so the headless decision is a unit-testable
// seam rather than living inline with the real isatty call in Execute.
func decideRoute(root *cobra.Command, args []string, tty bool) menuRoute {
	if !shouldLaunchTUI(root, args) {
		return routeDispatch
	}
	if tty {
		return routeTUI
	}
	return routeHeadlessMenu
}

// launchIntercept returns the args following a leading `launch`/`cl` command
// token — allowing only inert global flags (--no-icons) before it — or ok=false
// when this invocation isn't a launcher passthrough. A root flag such as
// --help/--version is NOT inert: encountering one disables the shortcut so fang
// can handle it, rather than skipping past it into the launcher.
//
// The "launch"/"cl" literals deliberately do NOT read launchModule — this
// intercept is host-owned dispatch-pipeline plumbing, not module surface
// (ADR-0005 §Future work). TestLaunchIntercept_MatchesLaunchModuleTokens pins
// the literals against the manifest so a GroupAliases change can't drift.
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
