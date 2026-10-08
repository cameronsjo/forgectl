package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// defaultFindingsOlderThan is `pr findings cleanup`'s default --older-than
// window: 30 days. Findings are the deliverable of a local clean-room
// review, so the default leans conservative — long enough that a review
// still being acted on is never swept by an unattended default run.
const defaultFindingsOlderThan = 720 * time.Hour

// newPrFindingsCmd builds `forgectl pr findings` — the reclaim path for the
// durable findings dir (config.PrFindingsDir): list what's there, and
// cleanup (dry-run by default) to reclaim old ones.
func newPrFindingsCmd(client *pr.Client, th theme.Theme) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "findings",
		Short: "List or reclaim durable findings from local clean-room reviews",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newPrFindingsListCmd(client),
		newPrFindingsCleanupCmd(client, th),
	)
	return cmd
}

func newPrFindingsListCmd(client *pr.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List findings directories from local clean-room reviews",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := client.FindingsList()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeFindingsListJSON(out, entries)
			}
			if len(entries) == 0 {
				fmt.Fprintln(out, "no findings")
				return nil
			}
			for _, e := range entries {
				_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n", termsafe.QuotePathIfUnsafe(e.Path), e.ModTime.Format(time.RFC3339), formatBytes(e.Size))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"path":...,"modified_at":...,"size_bytes":...}] to stdout`)
	return cmd
}

// findingsRowJSON is the --json wire shape for one `pr findings list` row:
// the human columns, with the size as raw bytes rather than a rounded label.
type findingsRowJSON struct {
	Path       string `json:"path"`
	ModifiedAt string `json:"modified_at"`
	SizeBytes  int64  `json:"size_bytes"`
}

// writeFindingsListJSON encodes the findings dirs through the sanctioned
// termsafe seam; a path is a filename chosen on disk, so the encoder's
// escaping is what makes it terminal-safe. An empty result encodes [].
func writeFindingsListJSON(w io.Writer, entries []pr.FindingsEntry) error {
	rows := make([]findingsRowJSON, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, findingsRowJSON{
			Path:       e.Path,
			ModifiedAt: e.ModTime.Format(time.RFC3339),
			SizeBytes:  e.Size,
		})
	}
	enc := termsafe.JSONEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// findingsCleanupJSON is the `pr findings cleanup --json` shape (additive-only,
// ADR-0008). Paths is always an array.
type findingsCleanupJSON struct {
	Paths []string `json:"paths"`
	Count int      `json:"count"`
}

func writeFindingsCleanupJSON(w io.Writer, paths []string) error {
	if paths == nil {
		paths = []string{}
	}
	return writeJSON(w, findingsCleanupJSON{Paths: paths, Count: len(paths)})
}

func newPrFindingsCleanupCmd(client *pr.Client, th theme.Theme) *cobra.Command {
	var (
		olderThan time.Duration
		apply     bool
		asJSON    bool
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
  forgectl pr findings cleanup --json               the dry-run report as JSON;
                                                     refused with --apply

This never touches the disposable review workspace or a live session — only
findings dirs under the durable findings store. --apply records each removal
in the audit log, which forgectl pr history reads back.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON && apply {
				return errors.New("--json reports the dry-run set; it cannot be combined with --apply (the delete arm is interactive and reports separately)")
			}
			if err := validateFindingsOlderThan(olderThan); err != nil {
				return err
			}
			return runPrFindingsCleanup(cmd, client, olderThan, apply, asJSON, th)
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", defaultFindingsOlderThan, "only consider findings dirs older than this (>= 0; 0 reclaims everything)")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete matched findings dirs, after a confirmation prompt")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"paths":[...],"count":N} of the reclaimable findings dirs to stdout; not valid with --apply`)
	return cmd
}

// validateFindingsOlderThan rejects a strictly-negative --older-than before
// any scan runs. A typo like -24h would push FindingsCleanup's cutoff into
// the future, and findingsRemovalCandidate would then treat essentially
// every findings dir — even one created seconds ago — as reclaimable. Zero
// stays valid: it is the explicit "reclaim everything" cutoff, and is still
// gated by the same --apply + confirm() prompt as any other value.
func validateFindingsOlderThan(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("--older-than %s must not be negative (0 means reclaim everything)", d)
	}
	return nil
}

// runPrFindingsCleanup scans exactly ONCE via FindingsCleanup(ctx, olderThan,
// false) — mirroring runClean's scan-once-reuse-twice shape (internal/cli/
// clean.go): the SAME set is printed at the preview, shown in the confirm
// prompt, and (only with --apply, after confirming) handed to
// client.FindingsRemove to delete. A second FindingsCleanup(ctx, olderThan, true)
// call would re-derive its target set from a fresh ReadDir, and could
// silently diverge from what the user just confirmed if the filesystem
// changed in between; FindingsRemove instead removes exactly the confirmed
// paths, re-validating each one at removal time (still TOCTOU-safe: a path
// that stopped qualifying is skipped with a note, not re-scanned into a
// different set). Each removal takes the lifecycle lock and writes the
// intent-then-completion audit pair `pr repair --history` shows, so a busy
// lock or an unwritable audit log stops the run before that dir is touched.
func runPrFindingsCleanup(cmd *cobra.Command, client *pr.Client, olderThan time.Duration, apply, asJSON bool, th theme.Theme) error {
	out := cmd.OutOrStdout()

	preview, err := client.FindingsCleanup(cmd.Context(), olderThan, false)
	if err != nil {
		return err
	}
	if asJSON {
		return writeFindingsCleanupJSON(out, preview)
	}
	if len(preview) == 0 {
		fmt.Fprintln(out, "nothing to reclaim")
		return nil
	}
	// Every path below is a directory name read off disk, so each one goes
	// through termsafe before reaching the terminal (forgectl#551): a name
	// carrying ESC or a newline is shown escaped and quoted, never raw.
	// QuotePathIfUnsafe rather than QuotePath keeps an ordinary path
	// byte-identical, the same choice `pr list` makes for its path field.
	for _, p := range preview {
		_, _ = fmt.Fprintln(out, termsafe.QuotePathIfUnsafe(p))
	}
	fmt.Fprintf(out, "\n%d findings dir(s) reclaimable\n", len(preview))

	if !apply {
		fmt.Fprintln(out, "re-run with --apply to delete them")
		return nil
	}

	ok, err := confirmFn(th, fmt.Sprintf("Delete %d findings dir(s)?", len(preview)))
	if err != nil {
		return err
	}
	if !ok {
		return noteCancelled(out)
	}

	removed, err := client.FindingsRemove(cmd.Context(), preview)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	for _, p := range removed {
		_, _ = fmt.Fprintf(out, "reclaimed %s\n", termsafe.QuotePathIfUnsafe(p))
	}
	fmt.Fprintf(out, "\nreclaimed %d findings dir(s)\n", len(removed))
	return nil
}
