package cli

import (
	"fmt"
	"os"
	"path/filepath"

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
		Long: `surface starts a harness (claude, codex) inside a terminal manager —
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
			})
		},
	}

	cmd.Flags().StringVar(&backendName, "surface", "",
		"terminal manager to create the surface in (tmux) — required, no default")
	cmd.Flags().StringVar(&displayName, "name", "",
		"display name for the surface (defaults to the target's directory name)")
	cmd.Flags().BoolVar(&allowPATH, "allow-path-binary", false,
		"accept a harness found by searching $PATH rather than named in config")

	return cmd
}

// surfaceLaunchOptions is the flag set, gathered so the run function reads as
// a sequence of steps rather than a parameter list.
type surfaceLaunchOptions struct {
	Target      string
	Backend     string
	DisplayName string
	AllowPATH   bool
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "."
	}
	return args[0]
}

// runSurfaceLaunch is the CLI half: resolve, build, hand to the service.
//
// It deliberately banners nothing on success. `forgectl launch` prints its
// posture to stderr because the operator is watching that terminal; this
// command's stderr belongs to whatever manager is about to host the session,
// so anything written here lands in a pane the operator did not ask to read.
func runSurfaceLaunch(cmd *cobra.Command, deps module.Deps, opts surfaceLaunchOptions) error {
	if opts.Backend == "" {
		return WithExitCode(fmt.Errorf(
			"--surface is required and has no default; pass --surface tmux"), 2)
	}

	adapter, err := surfaceAdapterFor(opts.Backend)
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

	built, err := launch.BuildInvocation(launch.InvocationRequest{
		Config:  deps.Cfg.Launch,
		CWD:     target,
		Args:    nil,
		BaseEnv: os.Environ(),
		Resolve: launch.ResolveBinary,
	})
	if err != nil {
		return err
	}

	service := surface.NewService(adapter, surface.Policy{AllowPATHBinary: opts.AllowPATH}, "")

	result, err := service.Launch(cmd.Context(), surface.LaunchRequest{
		Name:       displayNameFor(opts.DisplayName, target),
		Invocation: built.Invocation,
		Self:       self,
	})
	if err != nil {
		return err
	}

	// One line, to stdout, naming only what the manager already knows. The ref
	// renders as its backend and recovery tag; it has no accessor that would
	// print an invocation, an environment, or a server fingerprint.
	_, err = fmt.Fprintln(cmd.OutOrStdout(), termsafe.SafeLine(result.Ref().String()))
	return err
}

// displayNameFor falls back to the target's own directory name, which is what
// an operator would have typed anyway and what the manager will show.
func displayNameFor(explicit, target string) string {
	if explicit != "" {
		return explicit
	}
	return filepath.Base(target)
}
