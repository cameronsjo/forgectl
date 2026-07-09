package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	pippkg "github.com/cameronsjo/forgectl/internal/pip"
)

// newPipCmd builds `forgectl pip` — mirrors newNetCmd in building its own
// exec.Runner rather than sharing another domain's client lifecycle.
func newPipCmd() *cobra.Command {
	client := pippkg.New(exec.OSRunner{})
	return newPipCmdForClient(client)
}

// newPipCmdForClient builds the command over an already-constructed client —
// split out so tests can inject a *pip.Client pointed at a temp pip.conf
// (mirrors newNetCmdForClient) without needing to go through newPipCmd.
func newPipCmdForClient(client *pippkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pip",
		Short: "Comment- and whitespace-preserving pip.conf editor",
		Long: `pip edits pip.conf's index-url entries reversibly: remove comments an
entry out (byte-clean round-trip, no backup sidecar), restore uncomments
whatever remove last tagged. Every subcommand takes --path to target a
pip.conf other than the OS default (` + client.Path() + `).

  forgectl pip remove                          comment out [global] index-url
  forgectl pip remove --key extra-index-url    comment out a different key
  forgectl pip restore                         un-comment everything remove tagged
  forgectl pip show                            print the effective pip.conf
  forgectl pip path                            print the resolved pip.conf path`,
	}
	cmd.AddCommand(
		newPipRemoveCmd(client),
		newPipRestoreCmd(client),
		newPipShowCmd(client),
		newPipPathCmd(client),
	)
	return cmd
}

// resolvePipClient returns client, or a copy pointed at path when path is
// non-empty (the --path flag override).
func resolvePipClient(client *pippkg.Client, path string) *pippkg.Client {
	return client.WithPath(path)
}

// newPipRemoveCmd builds `pip remove`.
func newPipRemoveCmd(client *pippkg.Client) *cobra.Command {
	var section, key, path string
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Comment out a pip.conf entry (reversible)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			n, err := c.Remove(cmd.Context(), section, key)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if n == 0 {
				fmt.Fprintf(out, "no [%s] %s entries found in %s\n", section, key, c.Path())
				return nil
			}
			fmt.Fprintf(out, "removed %d [%s] %s %s from %s\n", n, section, key, entryWord(n), c.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&section, "section", "global", "pip.conf section to edit")
	cmd.Flags().StringVar(&key, "key", "index-url", "entry key to remove")
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// newPipRestoreCmd builds `pip restore`.
func newPipRestoreCmd(client *pippkg.Client) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Un-comment whatever remove last tagged",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			n, err := c.Restore(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if n == 0 {
				fmt.Fprintf(out, "no removed entries to restore in %s\n", c.Path())
				return nil
			}
			fmt.Fprintf(out, "restored %d %s in %s\n", n, entryWord(n), c.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// newPipShowCmd builds `pip show`.
func newPipShowCmd(client *pippkg.Client) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective pip.conf",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			data, err := c.Read(cmd.Context())
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// newPipPathCmd builds `pip path`.
func newPipPathCmd(client *pippkg.Client) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "path",
		Short: "Print the resolved pip.conf path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := resolvePipClient(client, path)
			fmt.Fprintln(cmd.OutOrStdout(), c.Path())
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "pip.conf path (default: OS-resolved location)")
	return cmd
}

// entryWord pluralizes "entry" for a count-driven message.
func entryWord(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}
