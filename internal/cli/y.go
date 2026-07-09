package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
)

// newYCmd builds `forgectl y` — mirrors newDockerCmd/newBranchCmd in
// building its own exec.Runner rather than sharing another domain's client
// lifecycle. Clipboard half of issue #26 only; the shell-history-reading
// half is deferred.
func newYCmd(cfg config.Config) *cobra.Command {
	client := clippkg.New(exec.OSRunner{})
	return newYCmdForClient(client)
}

// newYCmdForClient builds the command over an already-constructed client —
// split out so tests can inject a fake-wired *clip.Client (mirrors
// newDockerCmdForClient) without going through newYCmd.
func newYCmdForClient(client *clippkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "y",
		Short: "Read/write the system clipboard",
		Long: `y wraps pbcopy/pbpaste so a shell pipeline can move text through the
clipboard without shelling out directly. macOS only.

  echo hi | forgectl y copy   copy stdin to the clipboard
  forgectl y paste            print the clipboard's current contents`,
	}
	cmd.AddCommand(
		newYCopyCmd(client),
		newYPasteCmd(client),
	)
	applyYAliases(cmd)
	return cmd
}

// applyYAliases sets each y subcommand's Cobra aliases from the forgive
// registry — the single source of truth (mirrors applyDockerAliases).
func applyYAliases(parent *cobra.Command) {
	for _, sub := range parent.Commands() {
		var valid []string
		for _, alias := range forgive.YAliases[sub.Name()] {
			if alias == sub.Name() {
				continue
			}
			valid = append(valid, alias)
		}
		if len(valid) > 0 {
			sub.Aliases = valid
		}
	}
}

// newYCopyCmd builds `y copy`.
func newYCopyCmd(client *clippkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "copy",
		Short: "Copy stdin to the clipboard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			return client.Copy(cmd.Context(), string(data))
		},
	}
}

// newYPasteCmd builds `y paste`.
func newYPasteCmd(client *clippkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "paste",
		Short: "Print the clipboard's current contents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := client.Paste(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
}
