package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	dockerpkg "github.com/cameronsjo/forgectl/internal/docker"
	"github.com/cameronsjo/forgectl/internal/module"
)

// dockerAliases is the single source of truth for docker's subverb
// shorthands — migrated here from forgive.DockerAliases at conversion.
// Separate var for the same initialization-cycle reason as yAliases.
var dockerAliases = map[string][]string{
	"build": {"b"},
	"run":   {"r"},
	"shell": {"sh", "attach"},
}

// dockerModule declares the docker build/run/shell extension (ADR-0005):
// owns the [docker] config section.
var dockerModule = module.Manifest{
	Name:       "docker",
	Tier:       module.TierExtension,
	ConfigKey:  "docker",
	SubAliases: dockerAliases,
	New:        newDockerCmd,
}

// newDockerCmd builds `forgectl docker` over the registry Deps.
func newDockerCmd(deps module.Deps) *cobra.Command {
	client := dockerpkg.New(deps.Runner, dockerpkg.WithDockerConfig(deps.Cfg.Docker))
	return newDockerCmdForClient(client)
}

// newDockerCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *docker.Client
// (mirrors newNetCmdForClient) without going through newDockerCmd.
func newDockerCmdForClient(client *dockerpkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docker",
		Short: "Build/run/shell docker images tagged from git repo/branch/sha",
		Long: `docker wraps build/run/shell around a tag derived from git metadata —
{repo}:{branch-slug}-{shortsha}, plus a :dev alias — so labels attach at the
CLI without touching the Dockerfile.

  forgectl docker build [context] [-- args...]  build, tagging {repo}:{branch}-{sha}
                                                 and :dev; args after -- pass through
                                                 to docker build (can override
                                                 derived flags like --platform)
  forgectl docker run [-- args...]              run the built (or --tag) image
  forgectl docker shell                         open a shell in the built (or --tag) image

run and shell reuse the tag from the most recent build when --tag is
omitted. Configure defaults in the [docker] section of config.toml (macOS:
~/Library/Application Support/forgectl/config.toml).`,
	}
	cmd.AddCommand(
		newDockerBuildCmd(client),
		newDockerRunCmd(client),
		newDockerShellCmd(client),
	)
	applyAliases(cmd, dockerAliases)
	return cmd
}

// dockerBuildArgs bounds `docker build`'s positionals to at most one
// context dir — but only counts args before a `--` dash, so post-dash
// pass-through args (forgectl#398) don't count against the limit the way
// cobra.MaximumNArgs(1) would (cobra counts args on both sides of `--`).
func dockerBuildArgs(cmd *cobra.Command, args []string) error {
	n := len(args)
	if dash := cmd.ArgsLenAtDash(); dash != -1 {
		n = dash
	}
	if n > 1 {
		return fmt.Errorf("accepts at most 1 context arg before --, received %d", n)
	}
	return nil
}

// newDockerBuildCmd builds `docker build`.
func newDockerBuildCmd(client *dockerpkg.Client) *cobra.Command {
	var platform string
	cmd := &cobra.Command{
		Use:   "build [context] [-- args...]",
		Short: "Build an image tagged from git repo/branch/sha",
		Args:  dockerBuildArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			contextDir := "."
			var extraArgs []string
			dash := cmd.ArgsLenAtDash()
			if dash == -1 {
				dash = len(args)
			} else {
				extraArgs = args[dash:]
			}
			if dash > 0 {
				contextDir = args[0]
			}
			result, err := client.Build(cmd.Context(), dockerpkg.BuildOptions{
				ContextDir: contextDir,
				Platform:   platform,
				ExtraArgs:  extraArgs,
			})
			if err != nil {
				return err
			}
			if !result.GitMetadata {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: incomplete git metadata (%s); tagged %s only\n", result.GitReason, result.DevTag)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "built %s\n", result.Tag)
			return nil
		},
	}
	cmd.Flags().StringVar(&platform, "platform", "", "target platform for docker build --platform (default: [docker] default_platform, else unset)")
	return cmd
}

// newDockerRunCmd builds `docker run`.
func newDockerRunCmd(client *dockerpkg.Client) *cobra.Command {
	var tag string
	cmd := &cobra.Command{
		Use:   "run [-- args...]",
		Short: "Run the built (or --tag) image",
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Run(cmd.Context(), dockerpkg.RunOptions{Tag: tag, Args: args})
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "image tag to run (default: the most recently built tag)")
	return cmd
}

// newDockerShellCmd builds `docker shell`.
func newDockerShellCmd(client *dockerpkg.Client) *cobra.Command {
	var tag, shell string
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Open a shell in the built (or --tag) image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return client.Shell(cmd.Context(), dockerpkg.ShellOptions{Tag: tag, Shell: shell})
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "image tag to shell into (default: the most recently built tag)")
	cmd.Flags().StringVar(&shell, "shell", "", `shell command to exec inside the container (default: "sh")`)
	return cmd
}
