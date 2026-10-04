package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

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
	// No config section: v1 adds no default backend, no auto-detection, and no
	// persisted preference. Every launch names its manager explicitly.
	ConfigKey: "",
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

The backend is always explicit. There is no default and no detection.`,
	}
	cmd.AddCommand(newSurfaceLaunchCmd(deps))
	return cmd
}

func newSurfaceLaunchCmd(deps module.Deps) *cobra.Command {
	var (
		backendName string
		displayName string
		allowPATH   bool
		worktree    string
		harness     string
	)

	cmd := &cobra.Command{
		Use:   "launch [target]",
		Short: "Start a harness in a new managed surface",
		Long: `launch resolves a target directory, creates a surface in the named
terminal manager, and starts the harness inside it.

The target is a project name or a path. A bare name is looked up beneath the
projects root and must match exactly — an ambiguous name is refused rather than
guessed at, because guessing means opening a session in the wrong repository.
A path may be anywhere, because naming it is the choice being made explicitly.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSurfaceLaunch(cmd, deps, surfaceLaunchOptions{
				Target:      firstArg(args),
				Backend:     backendName,
				DisplayName: displayName,
				AllowPATH:   allowPATH,
				Worktree:    worktree,
				Harness:     harness,
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
		"run this harness instead of the one the directory's launch profile names (claude or codex)")

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
	if opts.Backend == "" {
		return WithExitCode(fmt.Errorf(
			"--surface is required and has no default; pass --surface tmux"), 2)
	}

	if opts.Worktree != "" {
		return runWorkerLaunch(cmd, deps, opts)
	}

	adapter, err := surfaceAdapterForWithWarnings(opts.Backend, cmd.ErrOrStderr())
	if err != nil {
		return WithExitCode(err, 2)
	}

	// New resolves the root from PROJECTS_DIR or ~/Projects; the surface has no
	// root of its own, so there is one place a project can be looked up.
	client := projects.New(deps.Runner)
	target, err := client.ResolveTarget(opts.Target)
	if err != nil {
		return WithExitCode(err, 2)
	}

	self, err := surface.SelfPath()
	if err != nil {
		return err
	}

	injected, unset, err := injectedLaunchEnv(deps.Cfg)
	if err != nil {
		return WithExitCode(termsafe.Error(err), 2)
	}

	built, err := launch.BuildInvocation(surfaceInvocationRequest(deps, target, injected, unset, opts.Harness))
	if err != nil {
		return err
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
	_, err = fmt.Fprintln(cmd.OutOrStdout(), termsafe.SafeLine(result.Ref().String()))
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
// The trampoline puts back the new pane's own values (paneIdentityEnv).
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

// surfaceInvocationRequest is the one place a surface launch, ordinary or
// worker, builds its invocation request. Keeping both paths on it means an
// environment or resolver change cannot reach one kind of surface and not the
// other.
func surfaceInvocationRequest(deps module.Deps, cwd string, injected map[string]string, unset []string, harness string) launch.InvocationRequest {
	return launch.InvocationRequest{
		Config:      deps.Cfg.Launch,
		CWD:         cwd,
		BaseEnv:     surfaceLaunchEnvironment(os.Environ()),
		InjectedEnv: injected,
		UnsetEnv:    unset,
		Resolve:     launch.ResolveBinary,
		Harness:     harness,
	}
}
