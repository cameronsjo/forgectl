package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
)

// defaultFindingsOlderThan is `pr findings cleanup`'s default --older-than
// window: 30 days. Findings are the deliverable of a local clean-room
// review, so the default leans conservative — long enough that a review
// still being acted on is never swept by an unattended default run.
const defaultFindingsOlderThan = 720 * time.Hour

// newPrFindingsCmd builds `forgectl pr findings` — the reclaim path for the
// durable findings dir (config.PrFindingsDir): list what's there, and
// cleanup (dry-run by default) to reclaim old ones.
func newPrFindingsCmd(client *pr.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "findings",
		Short: "List or reclaim durable findings from local clean-room reviews",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newPrFindingsListCmd(client),
		newPrFindingsCleanupCmd(client),
	)
	return cmd
}

func newPrFindingsListCmd(client *pr.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List findings directories from local clean-room reviews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := client.FindingsList()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "no findings")
				return nil
			}
			for _, e := range entries {
				fmt.Fprintf(out, "%s\t%s\t%s\n", e.Path, e.ModTime.Format(time.RFC3339), formatBytes(e.Size))
			}
			return nil
		},
	}
}

func newPrFindingsCleanupCmd(client *pr.Client) *cobra.Command {
	var (
		olderThan time.Duration
		apply     bool
	)
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Reclaim findings directories older than --older-than (dry-run by default)",
		Long: `cleanup reports findings directories older than --older-than (default:
720h — 30 days). Nothing is deleted without --apply, which is gated by a
confirmation prompt.

  forgectl pr findings cleanup                     dry-run over the 30-day default
  forgectl pr findings cleanup --older-than 168h    dry-run over 7 days
  forgectl pr findings cleanup --apply              reclaim, after confirming

This never touches the disposable review workspace or a live session — only
findings dirs under the durable findings store.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrFindingsCleanup(cmd, client, olderThan, apply)
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", defaultFindingsOlderThan, "only consider findings dirs older than this")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete matched findings dirs, after a confirmation prompt")
	return cmd
}

// runPrFindingsCleanup scans exactly ONCE via FindingsCleanup(olderThan,
// false) — mirroring runClean's scan-once-reuse-twice shape (internal/cli/
// clean.go): the SAME set is printed at the preview, shown in the confirm
// prompt, and (only with --apply, after confirming) handed to
// client.FindingsRemove to delete. A second FindingsCleanup(olderThan, true)
// call would re-derive its target set from a fresh ReadDir, and could
// silently diverge from what the user just confirmed if the filesystem
// changed in between; FindingsRemove instead removes exactly the confirmed
// paths, re-validating each one at removal time (still TOCTOU-safe: a path
// that stopped qualifying is skipped with a note, not re-scanned into a
// different set).
func runPrFindingsCleanup(cmd *cobra.Command, client *pr.Client, olderThan time.Duration, apply bool) error {
	out := cmd.OutOrStdout()

	preview, err := client.FindingsCleanup(olderThan, false)
	if err != nil {
		return err
	}
	if len(preview) == 0 {
		fmt.Fprintln(out, "nothing to reclaim")
		return nil
	}
	for _, p := range preview {
		fmt.Fprintln(out, p)
	}
	fmt.Fprintf(out, "\n%d findings dir(s) reclaimable\n", len(preview))

	if !apply {
		fmt.Fprintln(out, "re-run with --apply to delete them")
		return nil
	}

	ok, err := confirm(fmt.Sprintf("Delete %d findings dir(s)?", len(preview)))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	removed, err := client.FindingsRemove(preview)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	for _, p := range removed {
		fmt.Fprintf(out, "reclaimed %s\n", p)
	}
	fmt.Fprintf(out, "\nreclaimed %d findings dir(s)\n", len(removed))
	return nil
}
