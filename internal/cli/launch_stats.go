package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const launchStatsLong = `stats reports what the local launch-statistics store contains.

Collection is off unless [launch] usage_stats = true in config.toml. Each
recorded row holds exactly seven fields: schema version, a UTC timestamp to the
second, the fixed event name, the harness, the resolved model, the session mode
(new/resume/fork/unknown), and the posture (default/builder/agents). Nothing
identifying you, your machine, or your work is recorded — no directory, project,
repository, branch, session id, arguments, prompt, environment, tasks, host,
user, or process id, and no hash of any of them.

Those seven fields are still sensitive: exact timestamps describe when you work,
a model label can name an internal deployment, and the session and posture
counts describe how you work. Aggregating them locally does not change that.

Nothing is uploaded. There is no network call and no device identifier, and the
retired claunch wrapper's own log is never imported. Rows are kept until you
delete them; setting usage_stats = false stops new rows without removing old
ones. "forgectl launch doctor" prints the exact file paths.`

func newLaunchStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats [days]",
		Short: "Report the local launch-statistics store",
		Long:  launchStatsLong,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			var days *int64
			if len(args) == 1 {
				parsed, err := launch.ParseUsageDays(args[0])
				if err != nil {
					return termsafe.Error(err)
				}
				days = &parsed
			}

			aggregate, err := launch.ReadUsage(days, usageNow())
			if err != nil {
				// A busy or refused store yields no partial report: printing
				// half a count as if it were the whole one is worse than
				// printing nothing.
				return termsafe.Error(err)
			}

			if err := writeUsageReport(cmd.OutOrStdout(), aggregate, asJSON); err != nil {
				return termsafe.Error(err)
			}
			if aggregate.SkippedRows > 0 {
				// The complete report is already on stdout; this is the only
				// diagnostic, rendered once by the root error handler. The
				// command deliberately prints no warning of its own, so a
				// caller sees exactly one JSON object and one message.
				return WithExitCode(fmt.Errorf(
					"usage statistics skipped %d unreadable or unsupported row(s)", aggregate.SkippedRows), 1)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "emit the aggregate as JSON instead of a human report")
	return cmd
}

func writeUsageReport(out io.Writer, aggregate launch.UsageAggregateV1, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(out)
		// The stored model string is the operator's own value and stays raw in
		// the machine contract; only the human report below quotes it.
		encoder.SetEscapeHTML(false)
		return encoder.Encode(aggregate)
	}
	return writeUsageHumanReport(out, aggregate)
}

func writeUsageHumanReport(out io.Writer, aggregate launch.UsageAggregateV1) error {
	window := "all time"
	if aggregate.Window.Days != nil && aggregate.Window.Cutoff != nil {
		window = fmt.Sprintf("last %d day(s), since %s", *aggregate.Window.Days, *aggregate.Window.Cutoff)
	}
	fmt.Fprintf(out, "window    %s\n", window)
	fmt.Fprintf(out, "attempts  %d\n", aggregate.TotalAttempts)
	fmt.Fprintf(out, "first     %s\n", orNone(aggregate.FirstTS))
	fmt.Fprintf(out, "last      %s\n", orNone(aggregate.LastTS))

	for _, section := range []struct {
		label  string
		counts map[string]int
		render func(string) string
	}{
		{"harness", aggregate.Counts.Harness, termsafe.SafeLine},
		{"model", aggregate.Counts.Model, func(key string) string { return termsafe.SafeLine(launch.UsageModelLabel(key)) }},
		{"session", aggregate.Counts.SessionMode, termsafe.SafeLine},
		{"posture", aggregate.Counts.Posture, termsafe.SafeLine},
	} {
		if len(section.counts) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s\n", section.label)
		for _, row := range launch.SortUsageCounts(section.counts) {
			fmt.Fprintf(out, "  %-24s %d\n", section.render(row.Key), row.Count)
		}
	}
	return nil
}

func orNone(value *string) string {
	if value == nil {
		return "—"
	}
	return termsafe.SafeLine(*value)
}
