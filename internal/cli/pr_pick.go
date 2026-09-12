package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/theme"
)

var pickPRsFn = pickPRs

// newPrPickCmd builds `forgectl pr pick`. It needs cfg to launch the review
// agent (Launch resolves the claude posture from the launch profile).
func newPrPickCmd(client *pr.Client, cfg config.Config, th theme.Theme) *cobra.Command {
	// err discarded: "" degrades to an empty store on read (LoadReviewed).
	reviewedPath, _ := config.PrReviewedPath()
	return newPrPickCmdForClient(client, cfg, reviewedPath, th)
}

// newPrPickCmdForClient is the test seam — an already-wired client, cfg, and an
// explicit reviewed-store path.
func newPrPickCmdForClient(client *pr.Client, cfg config.Config, reviewedPath string, th theme.Theme) *cobra.Command {
	var noVerify bool
	cmd := &cobra.Command{
		Use:   "pick",
		Short: "Multiselect open PRs and spin up clean-room reviews in bulk",
		Long: `pick lists your open PRs in a multiselect. Chosen PRs are prepared
concurrently (same-repo checkouts serialized) and each launches a clean-room
review. A PR you've already marked reviewed is dimmed in the list and skipped
at launch, so a bulk pick never re-opens a review you've finished. Bulk
launches are capped at 4 concurrent reviews by default (override via [pr]
max_concurrent in config.toml). PRs past the cap are queued for 'forgectl pr
drain', not discarded. With both stdin and stdout attached to terminals,
pick uses the existing multiselect. Otherwise it writes sanitized owner/repo#N candidates
to stdout and exits 1; each printed ref works with forgectl pr <ref>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			prs, notes, err := client.PRs(ctx)
			if err != nil {
				return err
			}
			renderDegradationNotes(cmd, notes)
			if len(prs) == 0 {
				return fmt.Errorf("no open PRs to pick from")
			}

			store := pr.LoadReviewed(reviewedPath)
			selected, err := choosePRs(cmd, prs, store, th)
			if err != nil {
				return err
			}
			if len(selected) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no PRs selected")
				return nil
			}
			return launchPicked(ctx, client, cfg, cmd, selected, store, noVerify)
		},
	}
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the delayed post-dispatch window check")
	return cmd
}

func choosePRs(cmd *cobra.Command, prs []pr.PR, store *pr.ReviewedStore, th theme.Theme) ([]pr.PR, error) {
	if isInteractiveTTY() {
		return pickPRsFn(prs, store, th)
	}
	if err := writePRCandidates(cmd.OutOrStdout(), prs, store); err != nil {
		return nil, err
	}
	return nil, WithExitCode(fmt.Errorf("%d open PRs require a selection, and there is no interactive terminal — pass one printed owner/repo#N to `forgectl pr <ref>`, inspect the inventory with `forgectl pr prs --json`, or rerun `forgectl pr pick` interactively; candidates are on stdout", len(prs)), 1)
}

func writePRCandidates(out io.Writer, prs []pr.PR, store *pr.ReviewedStore) error {
	for _, item := range prs {
		if _, err := fmt.Fprintln(out, prCandidateLine(item, store)); err != nil {
			return err
		}
	}
	return nil
}

func prCandidateLine(item pr.PR, store *pr.ReviewedStore) string {
	line := item.Ref.String() + "  " + item.Title
	if pr.Dimmed(item, store) {
		line += "  (reviewed)"
	}
	return safeCandidate(line)
}

// pickPRs runs the multiselect and returns the chosen PRs (input PR order
// preserved). Reviewed options are rendered dimmed via th's muted style.
// Options are keyed by Ref.String() so a selection round-trips unambiguously.
func pickPRs(prs []pr.PR, store *pr.ReviewedStore, th theme.Theme) ([]pr.PR, error) {
	dimStyle := th.Styles().Muted
	opts := make([]huh.Option[string], len(prs))
	for i, p := range prs {
		refKey := p.Ref.String()
		opts[i] = huh.NewOption(prPickerLabel(p, store, dimStyle), refKey)
	}

	var chosen []string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Open PRs — space to select, enter to launch, esc to cancel").
				Options(opts...).
				Value(&chosen),
		),
	).WithKeyMap(keymap.Cancel()).WithTheme(th.Huh()).Run()
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

// prPickerLabel renders the human-only picker label. Both dynamic fields cross
// the shared terminal boundary; SafeLine leaves ordinary text byte-identical
// and visibly escapes controls rather than silently erasing evidence of them.
func prPickerLabel(p pr.PR, store *pr.ReviewedStore, dimStyle lipgloss.Style) string {
	label := fmt.Sprintf("%s  %s", safeTerm(p.Ref.String()), safeTerm(p.Title))
	if pr.Dimmed(p, store) {
		label = dimStyle.Render(label + "  (reviewed)")
	}
	return label
}

// launchPicked prepares the non-dimmed selected PRs concurrently, then launches
// each in input order. A dimmed selection is skipped BEFORE prepare (no wasted
// clone, no orphan workspace) with a one-line note — the picker's skip is the
// single shared Dimmed authority, matching the dashboard's dim.
func launchPicked(ctx context.Context, client *pr.Client, cfg config.Config, cmd *cobra.Command, selected []pr.PR, store *pr.ReviewedStore, noVerify bool) error {
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

	// Every PR the picker surfaces came from a forge query, so the whole batch
	// is third-party by construction — bulk is the route where an unnoticed
	// escalation would be widest, and it is declared here rather than left to
	// the zero value so the intent is legible.
	//
	// One value, built once, and used by BOTH dispositions a ref can get:
	// prepared now, or queued for the drainer. A queue path with its own
	// hardcoded agent and provenance would store a pairing the launch path
	// never agreed to the day this batch gains an --agent flag.
	batchOpts := pr.PrepareOpts{
		Agent:      resolveAgent(""),
		Provenance: pr.ReviewProvenanceThirdParty,
	}
	// Same fast fail `pr <ref>` runs before its reservation. Every picked ref
	// is third-party, so a pairing the gate refuses would otherwise reserve N
	// slots and park N needs-repair records on the launch branch, or persist a
	// queued record the drainer refuses hours later on the queue branch.
	if err := pr.CheckAgentForReview(batchOpts.Agent, batchOpts.Provenance); err != nil {
		return err
	}

	maxN, _, free, ok := client.Admit(ctx, cfg.Pr.MaxConcurrent)
	if !ok {
		return fmt.Errorf("cannot read the tmux review window count — refusing to launch %d review(s); "+
			"never mass-launch on an unreadable count. Check `tmux list-windows -a`, then retry", len(refs))
	}
	if free == 0 {
		queuePickedRefs(ctx, client, refs, batchOpts, maxN, errOut)
		return nil
	}
	var overflow []pr.Ref
	if len(refs) > free {
		overflow = refs[free:]
		refs = refs[:free]
	}
	if err := client.CheckDispatchCapability(ctx); err != nil {
		return err
	}

	// The cap is re-read inside PrepareMany's single lock hold, where the
	// reservations are written. The Admit call above is what decides how many
	// refs to attempt; the reservation is what actually claims the slots, and
	// only it is race-free against a peer launcher.
	results := client.PrepareMany(ctx, refs, cfg.Pr.MaxConcurrent, batchOpts)
	launched := 0
	prepareFailed := 0
	launchFailed := 0
	dispatches := make([]pr.Dispatch, 0, len(results))
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(errOut, "prepare %s failed: %v\n", r.Ref.String(), r.Err)
			prepareFailed++
			continue
		}
		dispatch, err := client.Launch(ctx, r.Session, cfg)
		if err != nil {
			fmt.Fprintf(errOut, "launch %s failed: %v\n", r.Ref.String(), err)
			launchFailed++
			continue
		}
		dispatches = append(dispatches, dispatch)
		fmt.Fprintf(out, "launched clean-room review of %s\n", r.Ref.String())
		launched++
	}
	queued := 0
	if len(overflow) > 0 {
		queued = queuePickedRefs(ctx, client, overflow, batchOpts, maxN, errOut)
	}
	// A launch failure leaves a prepared clean room (workspace + breadcrumb) on
	// disk — Phase 1 keeps it so the review is retryable/tearable. In bulk these
	// accumulate silently, so point the user at the cleanup path.
	if launchFailed > 0 {
		fmt.Fprintf(errOut, "%d review(s) prepared but failed to launch — their clean rooms remain; discard via 'forgectl pr list' then 'pr teardown <breadcrumb>'\n", launchFailed)
	}
	verification := verifyReviewDispatches(ctx, client, dispatches, noVerify)
	slog.Info("Successfully completed bulk launch.", "launched", launched, "prepareFailed", prepareFailed, "launchFailed", launchFailed, "skipped", skipped, "queued", queued, "verify", dispatchVerificationLogValue(verification.State), "gone", len(verification.Gone))
	return dispatchVerificationError(verification)
}

// queuePickedRefs writes a `queued` record for every ref past the
// concurrency cap — either the whole batch (free == 0) or the truncated
// remainder — and prints the one-line summary the design names. It reports
// per-ref queue failures (e.g. a duplicate from an earlier pick) without
// aborting the rest, and returns how many actually landed.
//
// opts is the batch's own PrepareOpts, passed in rather than rebuilt: a queued
// record is the drainer's input, so it must carry exactly the agent and
// provenance the same batch would have launched with.
func queuePickedRefs(ctx context.Context, client *pr.Client, refs []pr.Ref, opts pr.PrepareOpts, maxN int, errOut io.Writer) int {
	n := 0
	for _, ref := range refs {
		if _, err := client.Queue(ctx, ref, opts); err != nil {
			_, _ = fmt.Fprintf(errOut, "queue %s failed: %v\n", ref.String(), err)
			continue
		}
		n++
	}
	if n > 0 {
		_, _ = fmt.Fprintf(errOut, "%d PR(s) queued by the concurrency cap (max %d) — start them with 'forgectl pr drain --once'\n", n, maxN)
	}
	return n
}
