package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
	return interactiveTTY(
		term.IsTerminal(int(os.Stdin.Fd())),
		term.IsTerminal(int(os.Stdout.Fd())),
	)
}

func interactiveTTY(stdinTTY, stdoutTTY bool) bool { return stdinTTY && stdoutTTY }

// The startup steps Execute runs, behind package-level seams so the entry
// tests can replace each with a fail-if-called sentinel. That is how the
// ordering guarantee is asserted rather than merely read: `surface _exec` must
// be claimed before any of these run, and reading the source proves that only
// until the next edit.
//
// osArgs is here for the same reason — Execute reads process argv directly,
// which is the behavior under test, so the test has to be able to set it.
var (
	captureEnvSnapshot    = config.CaptureEnvSnapshot
	prepareLegacyBoundary = config.PrepareLegacyMigrationBoundary
	setupLogger           = config.SetupLogger
	buildRoot             = newRoot
	osArgs                = os.Args
)

// processArgs returns the arguments after the program name, tolerating an argv
// with no program name at all. Go's runtime normally guarantees os.Args[0], but
// an execve with argc == 0 does not, and this is read on the binary's first
// executable statement — the one place a panic has nothing to report it.
func processArgs() []string {
	if len(osArgs) == 0 {
		return nil
	}
	return osArgs[1:]
}

// Execute is the binary's entrypoint. It claims a surface bootstrap re-entry
// before doing anything else, then normalizes argv (forgiveness layer) and
// gives an eligible unknown top-level verb to the extension rungs before either
// opening the TUI (bare invoke or an external-command miss — the thumb-mode
// affordance) or handing off to fang for styled help/errors/version.
func Execute(ctx context.Context) error {
	// FIRST, ahead of every line below. A `surface _exec` re-entry carries a
	// private socket path and a one-use rendezvous nonce in argv, and every
	// statement after this one either does work that invocation does not need
	// or writes something derived from argv into the log. See surface_exec.go.
	//
	// argv[0] is not assumed to exist: an execve with argc == 0 would otherwise
	// panic on the binary's first executable statement, before any logger or
	// recovery exists to say why.
	if handled, err := trySurfaceExec(ctx, processArgs(), productionTrampolineRuntime()); handled {
		// This path returns before fang exists, so nothing downstream renders
		// the error — without this the operator gets a bare non-zero exit.
		//
		// Everything reachable here is fixed category text: the classifier's
		// refusals and the trampoline's are bare sentinels precisely because
		// this stderr is the terminal manager's pane, so none of them carry a
		// socket path, a nonce, or anything from the invocation.
		//
		// errHarnessExit is the exception, and it is suppressed rather than
		// printed. It means an ordinary session ended non-zero — the harness
		// has already written its own diagnostics to this same stderr, and a
		// second line beneath them is noise on every failed claude run. Its
		// exit code still propagates; only the message is dropped.
		if err != nil && !errors.Is(err, errHarnessExit) {
			fmt.Fprintln(os.Stderr, termsafe.SafeLine(err.Error()))
		}
		return err
	}

	env, err := captureEnvSnapshot()
	if err != nil {
		return err
	}
	legacyBoundary, err := prepareLegacyBoundary(env, config.NativeMigrationFS())
	if err != nil {
		return err
	}
	defer legacyBoundary.Close() //nolint:errcheck

	var cfg config.Config
	if !errors.Is(legacyBoundary.Refusal, config.ErrLegacyPathControl) {
		cfg = config.LoadPath(legacyBoundary.ConfigPath)
	}
	closer := setupLogger(cfg)
	defer closer.Close()

	slog.Debug("Starting forgectl.", "version", meta.Version)
	deps := productionDeps(cfg, legacyBoundary)
	// The bare-invoke TUI/runAction path keeps its own tmux client — clients
	// are stateless wrappers over the Runner, so a second instance is free and
	// the TUI stays decoupled from the module registry (ADR-0005: the menu is
	// a tmux session jumper, not a command palette).
	tmuxClient := tmux.New(exec.OSRunner{})
	root := buildRoot(deps)
	args := normalizeArgs(processArgs())

	// The launcher intercept runs before TUI/fang routing: `forgectl launch …`
	// (and its `cl` alias) must reach claude byte-clean for builder/agents
	// passthrough and exec the resolved profile when bare, bypassing Cobra flag
	// parsing. Own-verbs (which/edit/init/doctor/help) fall through to fang for
	// styled help. Only an inert global flag (--no-icons) may precede the token —
	// a root --help/--version must reach fang, not be skipped into the launcher.
	if rest, ok := launchIntercept(args); ok {
		if handled, err := runLaunch(deps, rest); handled {
			// This path bypasses fang, which is what prints styled errors for
			// the normal command tree. Print here so an intercept error (e.g. a
			// bad FORGECTL_CLAUDE_BIN from ClaudePath) doesn't exit non-zero with
			// empty stderr — mirrors claunch's original main().
			if err != nil {
				fmt.Fprintln(os.Stderr, meta.AppName+": "+termsafe.SafeLine(err.Error()))
			}
			return err
		}
	}

	if handled, err := tryExtensionRungs(root, args, defaultExternalCommandRuntime()); handled {
		return err
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
		logDispatch("Headless; routing to Cobra/fang instead of the TUI.", root, args)
		root.SetOut(os.Stderr)
		if err := execCommand(ctx, root, args); err != nil {
			return err
		}
		return errHeadlessMenuRoute
	default:
		logDispatch("Dispatching to command verb.", root, args)
		return execCommand(ctx, root, args)
	}
}

// logDispatch is the ONLY way either dispatch path records what it is about to
// run. Both routes call it rather than composing their own slog.Debug, so the
// argv-safety rule has exactly one implementation and exactly one place a test
// can hold it to: no raw argv, ever — a canonical verb and a count.
//
// It exists as a function purely for that reason. Inline, the two call sites
// were one careless edit away from `"verb", args` again, and nothing would have
// gone red.
func logDispatch(msg string, root *cobra.Command, args []string) {
	slog.Debug(msg, "verb", canonicalVerb(root, args), "argc", len(args))
}

// unknownVerb is what the dispatch log records for a token that resolves to no
// registered command. The literal category, never the supplied token — an
// unrecognized first argument is exactly the case where argv is most likely to
// be something forgectl never composed.
const unknownVerb = "unknown"

// canonicalVerb maps argv to the NAME OF A REGISTERED COMMAND, or the literal
// "unknown". It is what the dispatch debug records instead of raw argv.
//
// The value is drawn from the live command tree rather than filtered against a
// list of things not to log. A denylist has to be updated every time a command
// learns a secret-bearing argument, and the failure mode of forgetting is a
// silent leak; resolving against the registered set means a token can only be
// logged if it is already a command name that shipped in the binary.
//
// An alias resolves to the command's canonical name, so the log says what ran
// rather than how it was spelled.
func canonicalVerb(root *cobra.Command, args []string) string {
	first, _ := firstNonFlag(args)
	if first == "" {
		return unknownVerb
	}
	if builtinVerbs[first] {
		return first
	}
	if child := findChild(root, first); child != nil {
		return child.Name()
	}
	return unknownVerb
}

// productionDeps assembles the dependency set every module constructor
// receives. It is a named function rather than an inline literal so the
// wiring is assertable: a seam that production forgets to fill is a nil
// interface a module would have to discover at its own call site.
func productionDeps(cfg config.Config, boundary *config.LegacyMigrationBoundary) module.Deps {
	return module.Deps{
		Cfg:             cfg,
		Runner:          exec.OSRunner{},
		LegacyBoundary:  boundary,
		SensitiveRunner: exec.NewOSSensitiveRunner(),
	}
}

// execCommand hands args to Cobra via fang, which renders styled help,
// errors, and version output. Shared by the normal-dispatch and
// headless-menu-route paths in Execute; the only difference between them is
// where fang writes output, which the caller sets via root.SetOut first.
func execCommand(ctx context.Context, root *cobra.Command, args []string) error {
	root.SetArgs(args)
	return fang.Execute(ctx, root, fangOptions(meta.Version, meta.Commit)...)
}

// fangOptions builds the fang.Option set every dispatch runs under: the version
// seed for root.Version, and the terminal-safety boundary on the error sink.
// Extracted so TestVersion_VerbMatchesFlagThroughFang can call the exact same
// wiring instead of a parallel hand-rolled copy — an option added here is
// automatically exercised by that regression guard too.
func fangOptions(version, commit string) []fang.Option {
	return []fang.Option{
		fang.WithVersion(version),
		fang.WithCommit(commit),
		fang.WithErrorHandler(termsafeErrorHandler),
	}
}

// termsafeErrorHandler is the terminal-safety boundary for every error the
// cobra command tree returns — the one sink the rest of internal/cli cannot
// reach, because fang renders it after Execute hands the error back.
//
// fang.DefaultErrorHandler prints err.Error() with no filtering of its own, and
// plenty of forgectl errors carry text forgectl never composed: an
// *exec.CommandError concatenates raw tmux stderr, which echoes whatever
// session name it was given, and any same-uid process can name a session with
// an ANSI escape or a bidi override. Sanitizing here rather than at each
// fmt.Errorf means a new command cannot reintroduce the hole by forgetting.
//
// termsafe.Error preserves the unwrap chain, so errors.Is/errors.As disposition
// upstream is untouched; and SafeLine leaves ordinary ASCII byte-identical, so
// fang's own prefix match for usage errors ("unknown flag: …") still fires.
func termsafeErrorHandler(w io.Writer, styles fang.Styles, err error) {
	fang.DefaultErrorHandler(w, styles, termsafe.Error(err))
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
	case tui.ActionAttachSession:
		slog.Debug("Dispatching attach action.", "session_id", act.Session.ID, "name", act.Session.Name)
		return client.AttachSession(ctx, act.Session)
	case tui.ActionAttachWindow:
		slog.Debug("Dispatching window jump.", "window_id", act.Window.ID, "session_id", act.Window.SessionID)
		return client.AttachWindow(ctx, act.Window)
	case tui.ActionPick:
		slog.Debug("Dispatching pick action.", "candidate", act.Pick)
		return client.Pick(ctx, act.Pick)
	case tui.ActionLast:
		slog.Debug("Dispatching last session action.")
		return client.LastSession(ctx)
	}
	return nil
}

// builtinVerbs are commands Cobra/fang register lazily at Execute time, so they
// aren't in root.Commands() when we route. They must never fall into the menu
// or be shadowed by an external command.
var builtinVerbs = map[string]bool{
	"help": true, "completion": true, "man": true,
	"__complete": true, "__completeNoDesc": true,
}

// shouldLaunchTUI decides whether to open the menu instead of dispatching a
// verb: bare invocation, an unknown top-level verb, or an unknown subverb of a
// command group (e.g. `tmux frobnicate`). Flag-only invocations (--version,
// --help) stay with fang — only non-flag garbage falls into the menu. The
// check is against the live command/alias set (not root.Find), so it's immune
// to Cobra/fang's lazy registration of help/completion/man during Execute.
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
