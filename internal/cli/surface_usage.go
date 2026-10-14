package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface usage` (atelier P6a) sums what the drain's workers cost: the
// lines prune moved into usage-daily.jsonl plus the priced queue rows not
// yet pruned. It reads only; the atelier pane polls it for its spend panel.

const defaultSpendDays = 30

func newSurfaceUsageCmd(deps module.Deps) *cobra.Command {
	var (
		asJSON bool
		days   int
		repo   string
	)
	cmd := &cobra.Command{
		Use:   "usage [name]",
		Short: "Sum what the drain's workers cost over the last N days, or price one worker",
		Long: `usage sums the cost of the drain's workers by UTC day: the lines surface
prune wrote to usage-daily.jsonl, plus the queue rows that carry a cost_usd
and have not been pruned yet. A row is counted once by (name, launch_id); when
it is in both places the file line counts. Rows without a cost (the session
was not priced) are not counted, so the total is a floor.

--days is the window, counting today (UTC); default 30, at most 365.

--json prints {"days","total_usd","by_day":[{"day","usd","rows"}],"skipped","unpriced"}.
skipped counts usage-daily.jsonl lines that did not parse or are not valid
(no name, a bad day, a cost outside 0 to 1,000,000), and lines or rows dated
after today; they are left out. unpriced counts finished rows (closed or failed
after a launch) in the window with no cost: while it is above zero, total_usd
is a floor and the text says "partial". total_usd rounds the raw sum, so it
can differ by a cent from the sum of the rounded days.

Exit 0: summed. Exit 1: the queue or usage file could not be read. Exit 2: a
--days out of range.

With a worker name, usage prices that worker's session instead: it reads the
worker's ledger row (in --repo's ledger for this herdr session) and runs
"cadence-hooks metrics price" over the transcript the row records. Nothing is
asked of GitHub, so it works for a worker in a repository with no GitHub
remote. --json prints {"name","repo","harness","transcript","usage","reason"},
where usage is {"costUsd","byModel","priced","unpricedModels"} as in surface
status, or null with reason saying why. A codex or pi worker records no
transcript, so it has no usage here; a transcript file that does not exist
yet is "transcript not found", not $0.00. --days does not apply.

Exit 0: priced (priced is false when a model had no price, and costUsd then
covers only the priced models). Exit 1: not priced; the reason says why.
Exit 2: a usage error (no such worker, --days with a name).

  forgectl surface usage
  forgectl surface usage --days 7 --json
  forgectl surface usage fix-login --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if cmd.Flags().Changed("days") {
					return WithExitCode(errors.New("--days sums the drain by day; it does not apply to one worker"), exitUsage)
				}
				return runWorkerUsage(cmd, deps, repo, args[0], asJSON, func(ctx context.Context, transcript string) *statusUsage {
					return priceTranscript(ctx, deps.Runner, osexec.LookPath, transcript)
				})
			}
			if cmd.Flags().Changed("repo") {
				return WithExitCode(errors.New("--repo names a worker's ledger; give the worker's name"), exitUsage)
			}
			if days < 1 || days > drain.MaxSpendDays {
				return WithExitCode(fmt.Errorf("--days must be 1-%d, not %d", drain.MaxSpendDays, days), exitUsage)
			}
			rows, usage, err := readSpendState()
			if err != nil {
				// The state could not be read: the verb ran and failed (ADR-0015
				// exit 1), as in surface prune.
				return WithExitCode(termsafe.Error(err), exitFailed)
			}
			return reportSpend(cmd.OutOrStdout(), drain.SumSpend(usage, rows, time.Now(), days), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"days","total_usd","by_day":[{"day","usd","rows"}],"skipped","unpriced"}, or with a name {"name","repo","harness","transcript","usage","reason"}, as JSON`)
	cmd.Flags().IntVar(&days, "days", defaultSpendDays, "window in UTC days, counting today (1-365)")
	cmd.Flags().StringVar(&repo, "repo", ".", "with a name: repository the worker was launched from (project name or path)")
	return cmd
}

// readSpendState reads the queue rows and usage-daily.jsonl.
func readSpendState() ([]worker.QueueRow, []byte, error) {
	q, err := worker.OpenQueue()
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Rows()
	if err != nil {
		return nil, nil, err
	}
	files, err := worker.OpenDrainFiles()
	if err != nil {
		return nil, nil, err
	}
	usage, err := files.ReadUsage()
	if err != nil {
		return nil, nil, err
	}
	return rows, usage, nil
}

func reportSpend(out io.Writer, s drain.Spend, asJSON bool) error {
	if asJSON {
		return writeJSON(out, s)
	}
	var b strings.Builder
	for _, d := range s.ByDay {
		fmt.Fprintf(&b, "%s  $%.2f  %s\n", d.Day, d.USD, plural(d.Rows, "worker", "workers"))
	}
	if s.Unpriced > 0 {
		fmt.Fprintf(&b, "total over %d days: $%.2f (partial: %s unpriced)\n", s.Days, s.TotalUSD, plural(s.Unpriced, "finished worker", "finished workers"))
	} else {
		fmt.Fprintf(&b, "total over %d days: $%.2f\n", s.Days, s.TotalUSD)
	}
	if s.Skipped > 0 {
		fmt.Fprintf(&b, "skipped %s of usage state that did not parse or was out of range\n", plural(s.Skipped, "entry", "entries"))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// workerUsageResult is what `surface usage <name> --json` prints. Additive
// changes only (ADR-0008 rule 2).
type workerUsageResult struct {
	Name       string       `json:"name"`
	Repo       string       `json:"repo"`
	Harness    string       `json:"harness"`
	Transcript string       `json:"transcript"`
	Usage      *statusUsage `json:"usage"`
	Reason     string       `json:"reason,omitempty"`
}

// runWorkerUsage prices one worker's transcript from its ledger row. It asks
// nothing of GitHub (forgectl#1241).
func runWorkerUsage(cmd *cobra.Command, deps module.Deps, repo, name string, asJSON bool, price func(context.Context, string) *statusUsage) error {
	if err := worker.ValidName(name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	w, err := openWorkerLedger(cmd.Context(), cmd.ErrOrStderr(), deps, repo)
	if err != nil {
		return err
	}
	rows, err := w.led.Rows()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	row, ok := findRow(rows, name)
	if !ok {
		return WithExitCode(fmt.Errorf("no worker named %q in %s's ledger", name, w.top), exitUsage)
	}
	return reportWorkerUsage(cmd, priceWorker(cmd.Context(), row, w.top, price), asJSON)
}

// priceWorker prices row's transcript, or says why it cannot.
func priceWorker(ctx context.Context, row worker.Row, top string, price func(context.Context, string) *statusUsage) workerUsageResult {
	res := workerUsageResult{Name: row.Name, Repo: top, Harness: row.Harness, Transcript: row.Transcript}
	switch {
	case row.Transcript == "" && row.Harness != "" && row.Harness != "claude":
		res.Reason = "a " + row.Harness + " worker records no transcript forgectl can price"
	case row.Transcript == "":
		res.Reason = "the worker's ledger row records no transcript"
	case !transcriptExists(row.Transcript):
		// Priced, a missing file would read as $0.00: the session has not
		// written it yet, or it was moved or removed.
		res.Reason = "transcript not found: the session has not written it yet, or it was moved or removed"
	default:
		if res.Usage = price(ctx, row.Transcript); res.Usage == nil {
			res.Reason = "the transcript could not be priced: cadence-hooks is not on PATH, its metrics price failed, or it answered in an unexpected shape"
		}
	}
	return res
}

// transcriptExists reports whether path names a regular file.
func transcriptExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// reportWorkerUsage prints res and exits 1 when nothing was priced. With
// --json the object on stdout is the whole answer.
func reportWorkerUsage(cmd *cobra.Command, res workerUsageResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, res); err != nil {
			return err
		}
		if res.Usage == nil {
			return newSilentCodedError(exitFailed)
		}
		return nil
	}
	if res.Usage == nil {
		return WithExitCode(fmt.Errorf("worker %s: no usage: %s", res.Name, res.Reason), exitFailed)
	}
	u := res.Usage
	if u.Priced {
		_, err := fmt.Fprintf(out, "%s: $%.2f\n", res.Name, u.CostUSD)
		return err
	}
	_, err := fmt.Fprintf(out, "%s: partial, $%.2f for the priced models only; no price for %s\n",
		res.Name, u.CostUSD, strings.Join(u.UnpricedModels, ", "))
	return err
}
