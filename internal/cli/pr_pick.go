package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// newPrPickCmd builds `forgectl pr pick`. It needs cfg to launch the review
// agent (Launch resolves the claude posture from the launch profile).
func newPrPickCmd(client *pr.Client, cfg config.Config) *cobra.Command {
	// err discarded: "" degrades to an empty store on read (LoadReviewed).
	reviewedPath, _ := config.PrReviewedPath()
	return newPrPickCmdForClient(client, cfg, reviewedPath)
}

// newPrPickCmdForClient is the test seam — an already-wired client, cfg, and an
// explicit reviewed-store path.
func newPrPickCmdForClient(client *pr.Client, cfg config.Config, reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "pick",
		Short: "Multiselect open PRs and spin up clean-room reviews in bulk",
		Long: `pick lists your open PRs in a multiselect. Chosen PRs are prepared
concurrently (same-repo checkouts serialized) and each launches a clean-room
review. A PR you've already marked reviewed is dimmed in the list and skipped
at launch, so a bulk pick never re-opens a review you've finished.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			prs, notes, err := client.PRs(ctx)
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
			}
			if len(prs) == 0 {
				return fmt.Errorf("no open PRs to pick from")
			}

			store := pr.LoadReviewed(reviewedPath)
			selected, err := pickPRs(prs, store)
			if err != nil {
				return err
			}
			if len(selected) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no PRs selected")
				return nil
			}
			return launchPicked(ctx, client, cfg, cmd, selected, store)
		},
	}
}

// pickPRs runs the multiselect and returns the chosen PRs (input PR order
// preserved). Reviewed options are rendered dimmed via prDimStyle. Options are
// keyed by Ref.String() so a selection round-trips unambiguously.
func pickPRs(prs []pr.PR, store *pr.ReviewedStore) ([]pr.PR, error) {
	opts := make([]huh.Option[string], len(prs))
	for i, p := range prs {
		key := p.Ref.String()
		label := fmt.Sprintf("%s  %s", key, sanitizeCell(p.Title))
		if pr.Dimmed(p, store) {
			label = prDimStyle.Render(label + "  (reviewed)")
		}
		opts[i] = huh.NewOption(label, key)
	}

	var chosen []string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Open PRs — space to select, enter to launch").
				Options(opts...).
				Value(&chosen),
		),
	).Run()
	if err != nil {
		return nil, err
	}

	// Preserve input order rather than selection order for deterministic launch.
	selectedKeys := make(map[string]bool, len(chosen))
	for _, k := range chosen {
		selectedKeys[k] = true
	}
	out := make([]pr.PR, 0, len(chosen))
	for _, p := range prs {
		if selectedKeys[p.Ref.String()] {
			out = append(out, p)
		}
	}
	return out, nil
}

// launchPicked prepares the non-dimmed selected PRs concurrently, then launches
// each in input order. A dimmed selection is skipped BEFORE prepare (no wasted
// clone, no orphan workspace) with a one-line note — the picker's skip is the
// single shared Dimmed authority, matching the dashboard's dim.
func launchPicked(ctx context.Context, client *pr.Client, cfg config.Config, cmd *cobra.Command, selected []pr.PR, store *pr.ReviewedStore) error {
	slog.Debug("Preparing to launch picked PRs.", "selected", len(selected))
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	refs := make([]pr.Ref, 0, len(selected))
	skipped := 0
	for _, p := range selected {
		if pr.Dimmed(p, store) {
			fmt.Fprintf(errOut, "skip %s: previously marked reviewed\n", p.Ref.String())
			skipped++
			continue
		}
		refs = append(refs, p.Ref)
	}
	if len(refs) == 0 {
		fmt.Fprintln(errOut, "all selected PRs already reviewed; nothing to launch")
		slog.Debug("Skipping bulk launch: all selected PRs already reviewed.", "skipped", skipped)
		return nil
	}

	results := client.PrepareMany(ctx, refs, pr.PrepareOpts{Agent: resolveAgent("")})
	launched := 0
	prepareFailed := 0
	launchFailed := 0
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(errOut, "prepare %s failed: %v\n", r.Ref.String(), r.Err)
			prepareFailed++
			continue
		}
		if err := client.Launch(ctx, r.Session, cfg); err != nil {
			fmt.Fprintf(errOut, "launch %s failed: %v\n", r.Ref.String(), err)
			launchFailed++
			continue
		}
		fmt.Fprintf(out, "launched clean-room review of %s\n", r.Ref.String())
		launched++
	}
	// A launch failure leaves a prepared clean room (workspace + breadcrumb) on
	// disk — Phase 1 keeps it so the review is retryable/tearable. In bulk these
	// accumulate silently, so point the user at the cleanup path.
	if launchFailed > 0 {
		fmt.Fprintf(errOut, "%d review(s) prepared but failed to launch — their clean rooms remain; discard via 'forgectl pr list' then 'pr teardown <breadcrumb>'\n", launchFailed)
	}
	slog.Info("Successfully completed bulk launch.", "launched", launched, "prepareFailed", prepareFailed, "launchFailed", launchFailed, "skipped", skipped)
	return nil
}
