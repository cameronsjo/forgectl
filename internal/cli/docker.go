package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	dockerpkg "github.com/cameronsjo/forgectl/internal/docker"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
)

// newDockerCmd builds `forgectl docker` — mirrors newNetCmd/newPipCmd in
// building its own exec.Runner rather than sharing another domain's client
// lifecycle.
func newDockerCmd(cfg config.Config) *cobra.Command {
	client := dockerpkg.New(exec.OSRunner{}, dockerpkg.WithDockerConfig(cfg.Docker))
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

  forgectl docker build [context]   build, tagging {repo}:{branch}-{sha} and :dev
  forgectl docker run [-- args...]  run the built (or --tag) image
  forgectl docker shell             open a shell in the built (or --tag) image

run and shell reuse the tag from the most recent build when --tag is
omitted. Configure defaults in the [docker] section of config.toml (macOS:
~/Library/Application Support/forgectl/config.toml).`,
	}
	cmd.AddCommand(
		newDockerBuildCmd(client),
		newDockerRunCmd(client),
		newDockerShellCmd(client),
	)
	applyAliases(cmd, forgive.DockerAliases)
	return cmd
}

// newDockerBuildCmd builds `docker build`.
func newDockerBuildCmd(client *dockerpkg.Client) *cobra.Command {
	var platform string
	cmd := &cobra.Command{
		Use:   "build [context]",
		Short: "Build an image tagged from git repo/branch/sha",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			contextDir := "."
			if len(args) > 0 {
				contextDir = args[0]
			}
			tag, err := client.Build(cmd.Context(), dockerpkg.BuildOptions{
				ContextDir: contextDir,
				Platform:   platform,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "built %s\n", tag)
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
