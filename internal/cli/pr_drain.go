package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// defaultDrainInterval is --watch's default pass spacing.
const defaultDrainInterval = 60 * time.Second

// drainWatchRefusalLimit is how many CONSECUTIVE whole-pass refusals --watch
// tolerates before giving up and exiting non-zero. A per-record failure never
// counts against it — only a refusal that launched nothing at all.
const drainWatchRefusalLimit = 3

// newPrDrainCmd builds `forgectl pr drain` — the verb that starts queued
// reviews as concurrency-cap slots free up.
//
// ADR-0008 shape: --json on every arm, an honest exit code, and no
// interactive prompt of any kind (drain never shows one).
func newPrDrainCmd(client *pr.Client, cfg config.Config) *cobra.Command {
	var (
		once     bool
		watch    bool
		interval time.Duration
		dryRun   bool
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Launch queued reviews as concurrency-cap slots free up",
		Long: `drain claims the oldest queued reviews — FIFO, up to however many
concurrency-cap slots are currently free — and launches each through the same
path 'pr <ref>' uses. A launch failure is retried on a later pass; after 3
failed attempts the record is parked in needs-repair instead of retried
forever, and 'forgectl pr repair' is what settles it.

  forgectl pr drain                 one pass, then exit (the default)
  forgectl pr drain --watch         keep draining every --interval (default 60s)
  forgectl pr drain --dry-run       print what a pass would launch, create nothing
  forgectl pr drain --json          emit the pass report as JSON

A drainer killed mid-pass leaves 'preparing' or 'launching' records, which
occupy their slots until 'forgectl pr repair' settles them — the next pass
cannot double-launch.

Exit code for a single pass (the default): 0 when the queue was empty or
every launch succeeded; 1 when the cap or a record could not be read, or any
launch in the pass failed. --watch runs until canceled and exits non-zero
only after three consecutive whole-pass refusals; a per-record failure is
logged and the loop continues.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch && once && cmd.Flags().Changed("once") {
				return fmt.Errorf("--once and --watch cannot be combined")
			}
			if cmd.Flags().Changed("interval") && !watch {
				return fmt.Errorf("--interval only applies to --watch; add --watch, or drop the flag")
			}
			if interval <= 0 {
				interval = defaultDrainInterval
			}
			opts := pr.DrainOpts{DryRun: dryRun}
			if !watch {
				report, err := client.Drain(cmd.Context(), cfg, opts)
				if err != nil {
					return err
				}
				report.Pass = 1
				if err := writeDrainReport(cmd.OutOrStdout(), report, asJSON, dryRun, 0); err != nil {
					return err
				}
				return drainExitCode(report)
			}
			return runDrainWatch(cmd, client, cfg, opts, interval, asJSON, dryRun)
		},
	}
	cmd.Flags().BoolVar(&once, "once", true, "run a single pass and exit (the default)")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep draining on --interval until canceled")
	cmd.Flags().DurationVar(&interval, "interval", defaultDrainInterval, "how often --watch drains (requires --watch)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what a pass would launch and create nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the pass report as JSON")
	return cmd
}

// runDrainWatch loops Drain on interval until ctx is canceled, printing one
// pass line to stdout every time — the design's requirement that the line
// appears even when the slog handler is discarded, which a default install's
// is. A per-record failure never stops the loop; three CONSECUTIVE
// whole-pass refusals do.
func runDrainWatch(cmd *cobra.Command, client *pr.Client, cfg config.Config, opts pr.DrainOpts, interval time.Duration, asJSON, dryRun bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	maxN := pr.MaxConcurrentReviews(cfg.Pr.MaxConcurrent)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "watching: interval=%s cap=%d\n", interval, maxN)

	pass := 0
	consecutiveRefusals := 0
	for {
		pass++
		report, err := client.Drain(ctx, cfg, opts)
		if err != nil {
			return err
		}
		report.Pass = pass
		if werr := writeDrainReport(out, report, asJSON, dryRun, interval); werr != nil {
			return werr
		}
		if report.Refusal != "" {
			consecutiveRefusals++
			if consecutiveRefusals >= drainWatchRefusalLimit {
				return WithExitCode(fmt.Errorf(
					"drain refused %d consecutive passes, last: %s", consecutiveRefusals, report.Refusal), 1)
			}
		} else {
			consecutiveRefusals = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// writeDrainReport prints one pass — JSON or human, dry-run or real — to out.
func writeDrainReport(out io.Writer, report pr.DrainReport, asJSON, dryRun bool, next time.Duration) error {
	if asJSON {
		return writeDrainJSON(out, report)
	}
	writeDrainHuman(out, report, dryRun, next)
	return nil
}

// writeDrainJSON encodes the pass report exactly as DrainReport marshals —
// Items is never null (Drain always returns a non-nil slice).
func writeDrainJSON(out io.Writer, report pr.DrainReport) error {
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// writeDrainHuman renders the one-line-per-pass summary the design names —
// printed regardless of the slog handler, because a default install
// discards it. next is the --watch interval, appended as next=<duration>;
// zero on a single pass, where there is no next one.
func writeDrainHuman(out io.Writer, report pr.DrainReport, dryRun bool, next time.Duration) {
	if report.Refusal != "" {
		_, _ = fmt.Fprintf(out, "pass=%d refused: %s\n", report.Pass, safeTerm(report.Refusal))
		return
	}
	if dryRun {
		if len(report.Items) == 0 {
			_, _ = fmt.Fprintln(out, "nothing queued")
			return
		}
		refs := make([]string, 0, len(report.Items))
		for _, it := range report.Items {
			refs = append(refs, it.Ref)
		}
		_, _ = fmt.Fprintf(out, "%d queued, %d free — would launch %s\n",
			report.Queued, report.Free, strings.Join(refs, ", "))
		return
	}
	if report.Queued == 0 && len(report.Items) == 0 {
		_, _ = fmt.Fprintln(out, "nothing queued")
		return
	}
	line := fmt.Sprintf("pass=%d free=%d queued=%d launching=%d launched=%d failed=%d",
		report.Pass, report.Free, report.Queued, report.Launching, report.Launched, report.Failed)
	if next > 0 {
		line += fmt.Sprintf(" next=%s", next)
	}
	_, _ = fmt.Fprintln(out, line)
	for _, it := range report.Items {
		if it.Outcome == "launched" {
			continue
		}
		_, _ = fmt.Fprintf(out, "  %s: %s -> %s: %s\n", it.Ref, it.FromPhase, it.ToPhase, safeTerm(it.Error))
	}
}

// drainExitCode is the honest code for one pass: 1 when the pass refused
// outright or any launch failed, 0 otherwise — the same "a script can ask
// this" contract `pr repair`'s inspect exit code follows.
func drainExitCode(report pr.DrainReport) error {
	if report.Refusal != "" {
		return WithExitCode(fmt.Errorf("drain pass refused: %s", report.Refusal), 1)
	}
	if report.Failed > 0 {
		return WithExitCode(fmt.Errorf("%d review(s) failed to launch this pass", report.Failed), 1)
	}
	return nil
}
