package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/module"
)

// yAliases is the single source of truth for y's c/p shorthands — migrated
// here from forgive.YAliases at conversion. A separate var (not a literal
// inside yModule) because newYCmdForClient also applies it: routing that read
// through yModule would be an initialization cycle (manifest → constructor →
// manifest).
var yAliases = map[string][]string{
	"copy":  {"c"},
	"paste": {"p"},
}

// yModule declares the y (clipboard) extension (ADR-0005) — the conversion
// template for SubAliases modules.
var yModule = module.Manifest{
	Name:       "y",
	Tier:       module.TierExtension,
	SubAliases: yAliases,
	New:        newYCmd,
}

// newYCmd builds `forgectl y` over the registry Deps. Clipboard half of
// issue #26 only; the shell-history-reading half is deferred.
func newYCmd(deps module.Deps) *cobra.Command {
	client := clippkg.New(deps.Runner)
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
	applyAliases(cmd, yAliases)
	return cmd
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
