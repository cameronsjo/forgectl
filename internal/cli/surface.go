package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// surfaceModule declares the surface extension (ADR-0005).
//
// It enters as TierExtension and stays opt-in: `--surface` is required, there
// is no config default and no auto-detection. Starting a session somewhere the
// operator did not ask for is the failure mode worth designing against, and a
// default backend is how that happens.
//
// The hidden `surface _exec` trampoline is deliberately NOT a subcommand here.
// It is claimed by the classifier in surface_exec.go before any of this
// package's startup runs, because reaching it through Cobra would mean the
// socket path and the nonce had already passed through argv normalization and
// dispatch logging.
var surfaceModule = module.Manifest{
	Name: "surface",
	Tier: module.TierExtension,
	// [surface] holds [surface.drain] only. There is still no default
	// backend, no auto-detection, and no persisted preference: every launch
	// names its manager explicitly.
	ConfigKey: "surface",
	New:       newSurfaceCmd,
}

func newSurfaceCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "surface",
		Short: "Start a harness inside a terminal manager without exposing its invocation",
		Long: `surface starts a harness (claude, codex, pi) inside a terminal manager —
tmux, cmux, or herdr — without the manager ever seeing the harness invocation.

A manager necessarily learns the target directory and the command it is asked
to type, because it creates the workspace. What it does not learn is the
resolved harness path, its arguments, its environment, or a prompt: those are
delivered to a private trampoline over a local socket after the workspace
exists.

  forgectl surface launch forgectl --surface tmux
  forgectl surface launch ~/Projects/thing --surface tmux --name review

The backend is always explicit. There is no default and no detection.

enqueue, dequeue and queue manage the machine's queue of briefs, which
surface drain launches as claude, codex, or pi workers in herdr:

  forgectl surface enqueue --repo forgectl --name fix-login --brief brief.md
  forgectl surface queue

intake gh queues a brief for each open GitHub issue the operator labeled:

  forgectl surface intake gh --repo forgectl --dry-run

drain start runs the detached process that launches them; drain status,
events and stop inspect and end it:

  forgectl surface drain start
  forgectl surface drain status

status shows a worker's PR and what the merge policy ([surface.merge])
decides about it; merge-machine prints this machine's policy value:

  forgectl surface status fix-login --json
  forgectl surface merge-machine`,
	}
	cmd.AddCommand(newSurfaceLaunchCmd(deps))
	cmd.AddCommand(newSurfaceReadyCmd(deps))
	cmd.AddCommand(newSurfaceBriefCmd(deps))
	cmd.AddCommand(newSurfaceWaitCmd(deps))
	cmd.AddCommand(newSurfaceReadCmd(deps))
	cmd.AddCommand(newSurfaceListCmd(deps))
	cmd.AddCommand(newSurfaceCloseCmd(deps))
	cmd.AddCommand(newSurfaceEnqueueCmd(deps))
	cmd.AddCommand(newSurfaceDequeueCmd(deps))
	cmd.AddCommand(newSurfaceQueueCmd(deps))
	cmd.AddCommand(newSurfaceIntakeCmd(deps))
	cmd.AddCommand(newSurfaceDrainCmd(deps))
	cmd.AddCommand(newSurfaceDrainProcessCmd(deps))
	cmd.AddCommand(newSurfaceStatusCmd(deps))
	cmd.AddCommand(newSurfaceMergeMachineCmd(deps))
	return cmd
}

func newSurfaceLaunchCmd(deps module.Deps) *cobra.Command {
	var (
		backendName string
		displayName string
		allowPATH   bool
		worktree    string
		harness     string
		brief       string
		profile     string
		model       string
		here        bool
		dryRun      bool
		asJSON      bool
	)

	cmd := &cobra.Command{
		Use:   "launch [target]",
		Short: "Start a harness in a new managed surface",
		Long: `launch resolves a target directory, creates a surface in the named
terminal manager, and starts the harness inside it.

The target is a project name or a path. A bare name is looked up beneath the
projects root and must match exactly — an ambiguous name is refused rather than
guessed at, because guessing means opening a session in the wrong repository.
A path may be anywhere, because naming it is the choice being made explicitly.

A claude session aimed at a subfolder of a git repository starts at the
repository root when the root has .claude/settings.json or
.claude/settings.local.json and the subfolder has neither, because Claude Code
reads those settings only from the directory it starts in. The launch profile
is still the target's. After a move, relative paths in a prompt resolve
against the root; --here starts the session in the target itself.

With --worktree <branch> (herdr only, --name required) the launch starts a
coordinator worker instead: a git worktree at <repo>/.claude/worktrees/<name>,
created with repository hooks disabled, and a row in the worker ledger under
$XDG_STATE_HOME/forgectl/surface. A branch that does not exist yet starts at
the head of the repository's GitHub default branch, read from the GitHub API,
rather than at the checkout's HEAD; a repository whose origin is not on
github.com starts it at HEAD. Workers take their posture from [launch.worker] (see
docs/commands/launch.md), at most claude auto, or codex
workspace-write with on-request approvals; pi and anything looser are
refused, and workers never get --allow-dangerously-skip-permissions.

--brief gives a worker its first brief as the harness's prompt argument, so
the harness starts its first turn with it and nothing is typed into the pane.
A random marker is recorded in the ledger, and the brief asks the worker to
end with a REPORT line naming it (surface read --report). @file reads the
brief from a file of at most 64 KiB. The brief stays in the harness's
process arguments, where any local process can list it, so it must not hold
a secret; point the worker at a file in the worktree instead.

--profile <name> runs a claude worker with CLAUDE_CONFIG_DIR set to the
config_dir of [surface.profiles.<name>] (see docs/configuration.md), so it
uses that Claude account and settings home; main, or no --profile, keeps the
launcher's own CLAUDE_CONFIG_DIR. An unknown name is refused. --model <name>
replaces the launch profile's model for this worker and derives its effort
again; it is a plain token (letters, digits, '.', '-', '_', '[', ']', at most
64 characters, not starting with '-').

--dry-run runs every check the launch runs before it writes (the backend is
on PATH, the target resolves, the harness profile builds, and for a worker the
name, branch and worktree path are free) and prints what it would create, then
creates nothing: no worktree, branch, ledger row or surface. It never prints
the harness path, arguments, environment or brief. A refusal prints as the
launch would and exits as the launch does. Nothing is asked of GitHub, so a new
branch reads as branch_from "new". The preview sees a branch another worktree
has checked out, but git can still refuse the real worktree add for a reason
only it sees, so a clean preview is not a promise. --json prints the preview as one object:
{"dry_run","surface","name","target","harness","worker"}, where worker is
{"repo","worktree","branch","branch_from","ledger_row","brief","posture"} for --worktree
and absent otherwise. posture maps each field the harness takes (claude
permission_mode; codex sandbox, approval_policy) to {"value","source"}, where
source is default, launch.worker, or repo-profile; the text preview prints
<field>= and <field>_source= lines. --json needs --dry-run.

  forgectl surface launch . --surface herdr --worktree feat/x --name x --harness codex
  forgectl surface launch . --surface herdr --worktree fix/login --name fix-login --brief @brief.md
  forgectl surface launch . --surface herdr --worktree fix/y --name y --profile work --model sonnet
  forgectl surface launch . --surface herdr --worktree feat/x --name x --dry-run --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSurfaceLaunch(cmd, deps, surfaceLaunchOptions{
				Target:      firstArg(args),
				Backend:     backendName,
				DisplayName: displayName,
				AllowPATH:   allowPATH,
				Worktree:    worktree,
				Harness:     harness,
				Brief:       brief,
				Profile:     profile,
				Model:       model,
				Here:        here,
				DryRun:      dryRun,
				JSON:        asJSON,
			})
		},
	}

	// The parenthesised list is the DRIVEN set, not the recognised set, and the
	// difference is the point: parseBackendKind accepts "herdr" so a typo and an
	// unimplemented backend get different refusals, but naming it here would
	// advertise something no build can do. It went stale once already — cmux
	// shipped while this still said "(tmux)" — so it is written next to the
	// switch it describes in surfaceAdapterFor, and both change together.
	cmd.Flags().StringVar(&backendName, "surface", "",
		"terminal manager to create the surface in (tmux, cmux, herdr) — required, no default")
	cmd.Flags().StringVar(&displayName, "name", "",
		"display name for the surface (defaults to the target's directory name)")
	cmd.Flags().BoolVar(&allowPATH, "allow-path-binary", false,
		"accept a harness found by searching $PATH rather than named in config")
	cmd.Flags().StringVar(&worktree, "worktree", "",
		"start a worker on this branch in its own git worktree under <repo>/.claude/worktrees/<name> (herdr only; --name required)")
	cmd.Flags().StringVar(&harness, "harness", "",
		"run this harness instead of the one the directory's launch profile names (claude, codex, or pi)")
	cmd.Flags().BoolVar(&here, "here", false,
		"start claude in the target itself, not at its repository root when the .claude settings live there (claude only; no effect on workers or codex)")
	cmd.Flags().StringVar(&brief, "brief", "",
		"a worker's first brief, text or @file, passed as the harness's prompt argument (--worktree only)")
	cmd.Flags().StringVar(&profile, "profile", "",
		"run the worker under this [surface.profiles] entry's config_dir as CLAUDE_CONFIG_DIR; main is the launcher's own (--worktree only, claude)")
	cmd.Flags().StringVar(&model, "model", "",
		"run the worker on this model instead of the launch profile's (--worktree only)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"run the launch's checks and print what it would create, creating nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`print the --dry-run preview as {"dry_run","surface","name","target","harness","run_directory","worker"} JSON (needs --dry-run)`)

	return cmd
}

// surfaceLaunchOptions is the flag set, gathered so the run function reads as
// a sequence of steps rather than a parameter list.
type surfaceLaunchOptions struct {
	Target      string
	Backend     string
	DisplayName string
	AllowPATH   bool
	Worktree    string
	Harness     string
	Brief       string
	Profile     string
	Model       string
	Here        bool
	DryRun      bool
	JSON        bool
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "."
	}
	return args[0]
}

// runSurfaceLaunch is the CLI half: resolve, build, hand to the service.
//
// It deliberately banners nothing on success. The exception is an advisory
// backend warning: unlike a success banner, a warning about a socket location
// another local user can perturb is actionable at launch time and is routed to
// this command's stderr explicitly rather than depending on optional logging.
func runSurfaceLaunch(cmd *cobra.Command, deps module.Deps, opts surfaceLaunchOptions) error {
	if err := refuseInDrainWorker(os.Getenv, "surface launch"); err != nil {
		return err
	}
	if opts.Backend == "" {
		return WithExitCode(fmt.Errorf(
			"--surface is required and has no default; pass --surface tmux"), 2)
	}
	if opts.JSON && !opts.DryRun {
		return WithExitCode(errors.New("--json prints the --dry-run preview; add --dry-run (a launch prints one line, not JSON)"), exitUsage)
	}

	if opts.Worktree != "" {
		return runWorkerLaunch(cmd, deps, opts)
	}
	if opts.Brief != "" {
		return WithExitCode(errors.New("--brief needs --worktree; only a worker launch takes a brief"), exitUsage)
	}
	if opts.Profile != "" || opts.Model != "" {
		return WithExitCode(errors.New("--profile and --model need --worktree; only a worker launch takes them"), exitUsage)
	}

	adapter, err := surfaceAdapterForWithWarnings(opts.Backend, cmd.ErrOrStderr())
	if err != nil {
		return WithExitCode(err, exitUsage)
	}

	// New resolves the root from PROJECTS_DIR or ~/Projects; the surface has no
	// root of its own, so there is one place a project can be looked up.
	client := projects.New(deps.Runner)
	target, err := client.ResolveTarget(opts.Target)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}

	self, err := surface.SelfPath()
	if err != nil {
		return err
	}

	injected, unset, err := injectedLaunchEnv(deps.Cfg)
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}

	built, err := buildSurfaceLaunchInvocation(deps.Cfg.Launch, target, injected, unset, opts.Harness, opts.Here, cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	if opts.DryRun {
		// The service refuses an unaccepted binary (a harness found on $PATH
		// without --allow-path-binary, a missing or self-looping path) when it
		// launches; the preview asks the same policy so it refuses too.
		if err := (surface.Policy{AllowPATHBinary: opts.AllowPATH}).AcceptBinary(built.Invocation.Binary, self); err != nil {
			return err
		}
		plan := launchPlan{
			DryRun:  true,
			Surface: opts.Backend,
			Name:    displayNameFor(opts.DisplayName, target),
			Target:  target,
			Harness: built.Invocation.Harness,
		}
		if built.Invocation.Harness == "claude" {
			plan.RunDirectory = built.Invocation.CWD
		}
		return renderLaunchPlan(cmd.OutOrStdout(), plan, opts.JSON)
	}

	service := surface.NewService(adapter, surface.Policy{AllowPATHBinary: opts.AllowPATH}, "")

	if opts.Backend == "herdr" {
		built.Invocation.Env = markHerdrPane(built.Invocation.Env)
	}
	req := surface.NewLaunchRequest(displayNameFor(opts.DisplayName, target), built.Invocation)
	req.Self = self

	result, err := service.Launch(cmd.Context(), req)
	if err != nil {
		return err
	}

	// One line, to stdout, naming only what the manager already knows. The ref
	// renders as its backend and recovery tag; it has no accessor that would
	// print an invocation, an environment, or a server fingerprint.
	_, err = fmt.Fprintln(cmd.OutOrStdout(), safeTitle(result.Ref().String()))
	return err
}

const claudeCodeChildSessionEnv = "CLAUDE_CODE_CHILD_SESSION"

// herdrPaneIdentityEnv are the variables herdr sets to name the pane a process
// runs in. HERDR_SOCKET_PATH is not among them: it names the server, which the
// launcher and the new pane share.
var herdrPaneIdentityEnv = []string{"HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID"}

// maxPaneIdentityLen bounds a herdr id carried into a harness. herdr's ids are
// short ("w83:p5"); anything longer is not one.
const maxPaneIdentityLen = 64

// herdrPaneMarkerEnv marks an invocation the herdr backend delivered. Only
// then is the trampoline inside a pane herdr opened: a tmux or cmux pane can
// carry stale HERDR_* values inherited from whatever started its server.
const herdrPaneMarkerEnv = "FORGECTL_SURFACE_HERDR_PANE"

// markHerdrPane adds the herdr marker to an invocation's environment.
func markHerdrPane(env []string) []string {
	return append(slices.Clone(env), herdrPaneMarkerEnv+"=1")
}

// paneIdentityEnv removes the herdr marker from env and, only when it was
// there, appends the trampoline's own herdr pane identity.
//
// The trampoline runs inside the new pane, so getenv here answers with that
// pane's ids, which herdr set when it opened it. Only these three keys cross,
// only when env does not already name them, and only when the value has the
// shape of a herdr id, so nothing else of the trampoline's environment reaches
// the harness.
func paneIdentityEnv(env []string, getenv func(string) string) []string {
	marked := false
	out := make([]string, 0, len(env)+len(herdrPaneIdentityEnv))
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == herdrPaneMarkerEnv {
			marked = true
			continue
		}
		out = append(out, e)
	}
	if !marked {
		return out
	}
	for _, key := range herdrPaneIdentityEnv {
		value := getenv(key)
		if !validPaneIdentity(value) || slices.ContainsFunc(env, func(e string) bool {
			k, _, _ := strings.Cut(e, "=")
			return k == key
		}) {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

func validPaneIdentity(v string) bool {
	if v == "" || len(v) > maxPaneIdentityLen {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ':', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// surfaceLaunchEnvironment removes the Claude marker that describes a child
// process of the current session. A newly created surface is an independently
// resumable workspace, so forwarding the marker would misclassify the harness
// and silently disable its transcript. This policy belongs at the surface
// boundary: ordinary in-place launches continue to inherit the marker.
//
// The herdr pane-identity variables go for the same reason. They name the pane
// the LAUNCHER runs in, and the trampoline replaces the new pane's environment
// with this one, so a worker started from a herdr pane would report its agent
// state, and receive anything addressed to "its" pane, on the launcher's pane.
// For a herdr launch, which marks its invocation (markHerdrPane), the
// trampoline puts back the new pane's own values (paneIdentityEnv).
func surfaceLaunchEnvironment(base []string) []string {
	out := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if key == claudeCodeChildSessionEnv || slices.Contains(herdrPaneIdentityEnv, key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// displayNameFor falls back to the target's own directory name, which is what
// an operator would have typed anyway and what the manager will show.
func displayNameFor(explicit, target string) string {
	if explicit != "" {
		return explicit
	}
	return filepath.Base(target)
}

// buildSurfaceLaunchInvocation builds a non-worker surface launch. A claude
// session aimed at a repository subfolder starts at the root, where its
// .claude settings live, and the surface is created there
// (cadence-ecosystem#608); here keeps it in the target. The move is said on
// stderr, like the backend warnings: the manager shows the root, not the
// directory that was named.
func buildSurfaceLaunchInvocation(cfg config.LaunchConfig, target string, injected map[string]string, unset []string, harness string, here bool, stderr io.Writer) (launch.BuiltInvocation, error) {
	req := surfaceInvocationRequest(cfg, target, injected, unset, harness)
	req.StayInCWD = here
	built, err := launch.BuildInvocation(req)
	if err != nil {
		return launch.BuiltInvocation{}, err
	}
	if dir := built.Invocation.CWD; dir != target {
		_, _ = fmt.Fprintln(stderr, settingsRootNotice(target, dir))
	}
	return built, nil
}

// surfaceInvocationRequest is the one place a surface launch, ordinary or
// worker, builds its invocation request, so an environment or resolver
// change cannot reach one kind of surface and not the other.
//
// The harness runs in a fresh terminal pane, so its stdout IS a terminal,
// and StdoutTerminal says so explicitly (#816): the zero value means "not a
// terminal", which lets `--output-format` alone select the print posture
// (forgectl#795). With no args that choice never arises today, but a later
// args field must not inherit a non-TTY default for a TTY pane.
func surfaceInvocationRequest(cfg config.LaunchConfig, cwd string, injected map[string]string, unset []string, harness string) launch.InvocationRequest {
	return launch.InvocationRequest{
		Config:         cfg,
		CWD:            cwd,
		Args:           nil,
		BaseEnv:        surfaceLaunchEnvironment(os.Environ()),
		InjectedEnv:    injected,
		UnsetEnv:       unset,
		Resolve:        launch.ResolveBinary,
		Harness:        harness,
		StdoutTerminal: true,
	}
}
