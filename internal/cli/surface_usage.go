package cli

import (
	"fmt"
	"io"
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

func newSurfaceUsageCmd(_ module.Deps) *cobra.Command {
	var (
		asJSON bool
		days   int
	)
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Sum what the drain's workers cost over the last N days",
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

  forgectl surface usage
  forgectl surface usage --days 7 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"days","total_usd","by_day":[{"day","usd","rows"}],"skipped","unpriced"} as JSON`)
	cmd.Flags().IntVar(&days, "days", defaultSpendDays, "window in UTC days, counting today (1-365)")
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
