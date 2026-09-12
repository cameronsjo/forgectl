package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newPrRepairCmd builds `forgectl pr repair` — the recovery verb for a review
// session whose record and whose reality disagree.
//
// ADR-0008 shape: `--json` on every arm, no interactive prompt off a TTY (the
// destructive arm requires `--yes` there), and an honest exit code — 1 when a
// record was left unsettled, 0 when there was nothing to settle.
func newPrRepairCmd(client *pr.Client) *cobra.Command {
	var (
		apply          bool
		adoptWindow    bool
		rollback       bool
		forgetIfAbsent bool
		dryRun         bool
		yes            bool
		asJSON         bool
		history        bool
	)
	cmd := &cobra.Command{
		Use:   "repair [breadcrumb]",
		Short: "Inspect and settle review sessions whose record and reality disagree",
		Long: `repair reports every review session stuck between phases — a slot reserved
whose clone never finished, a window recorded as launching that no longer
exists, a record parked in needs-repair by a failure that could not prove its
own outcome — and settles the one you name.

  forgectl pr repair                                              list what needs settling
  forgectl pr repair <breadcrumb> --apply --adopt-window          record the live window this session really has
  forgectl pr repair <breadcrumb> --apply --rollback              remove the clean room and the record
  forgectl pr repair <breadcrumb> --apply --forget-if-absent      remove only a record whose window and clean room are both gone

--adopt-window takes no window operand: the window is re-derived from the ref
exactly as every other verb derives it, so no operator-supplied tmux target can
steer it. --rollback refuses while the window is live, refuses when the window
list cannot be read at all, and off a terminal requires --yes. --dry-run prints
what each would do and touches nothing. --history shows the audit trail.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if history {
				return runRepairHistory(cmd, client, asJSON)
			}
			var record string
			if len(args) == 1 {
				record = args[0]
			}
			report, err := client.Repair(cmd.Context(), pr.RepairOpts{
				Record:         record,
				Apply:          apply,
				AdoptWindow:    adoptWindow,
				Rollback:       rollback,
				ForgetIfAbsent: forgetIfAbsent,
				DryRun:         dryRun,
				Yes:            yes,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return writeRepairJSON(cmd.OutOrStdout(), report)
			}
			return writeRepairHuman(cmd, report, apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "act on the named breadcrumb (requires exactly one mode below)")
	cmd.Flags().BoolVar(&adoptWindow, "adopt-window", false, "record the live review window this session already has")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "remove the clean room and the record (destructive)")
	cmd.Flags().BoolVar(&forgetIfAbsent, "forget-if-absent", false, "remove only the record, once its window and clean room are both gone")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen and touch nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm a destructive repair without a terminal prompt")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"items":[…]} to stdout`)
	cmd.Flags().BoolVar(&history, "history", false, "show the repair audit trail instead of the current state")
	return cmd
}

// runRepairHistory prints the audit trail. It is a separate arm rather than a
// mode of the report because the two answer different questions — what is
// wrong now, versus what was done about it.
func runRepairHistory(cmd *cobra.Command, client *pr.Client, asJSON bool) error {
	rows, err := client.RepairHistory(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if asJSON {
		if rows == nil {
			rows = []pr.RepairRow{}
		}
		enc := termsafe.JSONEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "no repairs recorded")
		return nil
	}
	for _, r := range rows {
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			r.TS.Format("2006-01-02T15:04:05Z07:00"), r.Mode, r.Outcome,
			safeTerm(r.Ref), termsafe.QuotePathIfUnsafe(r.RecordPath))
	}
	return nil
}

// writeRepairJSON encodes the report as an object (not a bare array) so a
// later field — a refusal summary, a pass count — is an additive change rather
// than a shape change. Items is never null: a caller ranges over it
// unconditionally.
func writeRepairJSON(out io.Writer, report pr.RepairReport) error {
	if report.Items == nil {
		report.Items = []pr.RepairItem{}
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// writeRepairHuman renders the report and sets the exit code. An inspect that
// found unsettled records exits 1: `pr repair` is the verb a script runs to ask
// whether anything needs a human, and exiting 0 on "four sessions are stuck"
// would make that question unanswerable from the exit status.
func writeRepairHuman(cmd *cobra.Command, report pr.RepairReport, apply bool) error {
	out := cmd.OutOrStdout()
	if len(report.Items) == 0 {
		_, _ = fmt.Fprintln(out, "no records need repair")
		return nil
	}
	for _, it := range report.Items {
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			safeTerm(it.Ref), it.FromPhase, windowObservation(it), workspaceObservation(it),
			termsafe.QuotePathIfUnsafe(it.RecordPath))
		if it.Reason != "" {
			_, _ = fmt.Fprintf(out, "  reason: %s\n", safeTerm(it.Reason))
		}
		if it.Outcome != "" && it.Outcome != "inspect" {
			_, _ = fmt.Fprintf(out, "  %s\n", it.Outcome)
		}
	}
	if apply {
		return nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"%d record(s) need settling — see `forgectl pr repair --help` for the three ways to settle one\n", len(report.Items))
	return WithExitCode(fmt.Errorf("%d review session(s) are unsettled", len(report.Items)), 1)
}

// windowObservation renders what tmux said, never what the record claims — the
// distinction `pr repair` exists to surface.
func windowObservation(it pr.RepairItem) string {
	if it.WindowLive {
		return "window live"
	}
	return "no window"
}

func workspaceObservation(it pr.RepairItem) string {
	if it.WorkspaceExists {
		return "clean room present"
	}
	return "no clean room"
}
