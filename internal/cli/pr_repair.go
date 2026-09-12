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
		prune          bool
		olderThan      string
		logRetention   string
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

  forgectl pr repair --prune                                      reap set-aside records and compact the audit log

--adopt-window takes no window operand: the window is re-derived from the ref
exactly as every other verb derives it, so no operator-supplied tmux target can
steer it. --rollback refuses while the window is live, refuses when the window
list cannot be read at all, and off a terminal requires --yes. --dry-run prints
what each would do and touches nothing. --history shows the session audit trail,
which carries teardown and cleanup rows beside repair's.

--prune is the housekeeping sweep, and the only arm that UNLINKS: it removes
set-aside records (<name>.json.unreadable-<timestamp>) older than --older-than,
and drops settled rows older than --log-retention from the audit log. A file's
age comes from its NAME, never its mtime, which a rename preserves. It takes no
breadcrumb and no --apply mode, because it sweeps the whole session directory.
It refuses per file: a live window, an unreadable window list, or a file that
changed underfoot stops that file and nothing else.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The whole prune grammar is settled BEFORE dispatch, so no
			// combination reaches a verb that would quietly ignore half of it.
			if err := validatePruneFlags(cmd, pruneGrammar{
				prune:   prune,
				apply:   apply,
				history: history,
				args:    args,
				// A SLICE, not a map: this feeds a refusal message, and map
				// iteration order would make the wording of a two-mode refusal
				// vary between runs.
				modes: []modeFlag{
					{pr.RepairModeAdoptWindow, adoptWindow},
					{pr.RepairModeRollback, rollback},
					{pr.RepairModeForgetIfAbsent, forgetIfAbsent},
				},
			}); err != nil {
				return err
			}
			if prune {
				return runRepairPrune(cmd, client, olderThan, logRetention, dryRun, yes, asJSON)
			}
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
				if err := writeRepairJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else if err := writeRepairHuman(cmd, report, apply); err != nil {
				return err
			}
			// The exit code is decided once, for both output shapes. An inspect
			// that found unsettled records exits 1 — that is the question a
			// script asks `pr repair`, and answering it only in the human text
			// would make `--json` the one caller that cannot hear the answer.
			return repairExitCode(report, apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "act on the named breadcrumb (requires exactly one mode below)")
	cmd.Flags().BoolVar(&adoptWindow, "adopt-window", false, "record the live review window this session already has")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "remove the clean room and the record (destructive)")
	cmd.Flags().BoolVar(&forgetIfAbsent, "forget-if-absent", false, "remove only the record, once its window and clean room are both gone")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen and touch nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm a destructive repair without a terminal prompt")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"items":[…]} to stdout`)
	cmd.Flags().BoolVar(&history, "history", false,
		"show the session audit trail (repair, teardown, cleanup) instead of the current state")
	cmd.Flags().BoolVar(&prune, "prune", false, "remove set-aside records past their retention window and compact the audit log")
	cmd.Flags().StringVar(&olderThan, "older-than", defaultAsideRetention,
		"with --prune: how old a set-aside record's name must say it is before it is removed")
	cmd.Flags().StringVar(&logRetention, "log-retention", defaultLogRetention,
		"with --prune: how far back the audit log keeps settled rows")
	return cmd
}

// Prune's retention defaults. A set-aside record is evidence someone may still
// need to read, and the audit log is sometimes the last pointer to a clean
// room, so both windows are generous: the cost of keeping a file too long is
// disk, and the cost of dropping it too early is unrecoverable.
const (
	defaultAsideRetention = "30d"
	defaultLogRetention   = "90d"
)

// modeFlag pairs an --apply mode's flag name with whether it was given, so a
// refusal can name the one the operator actually typed.
type modeFlag struct {
	name string
	set  bool
}

// pruneGrammar is everything the prune refusals need to see at once: the arm
// flags, the positional operand, and the three --apply modes.
type pruneGrammar struct {
	prune   bool
	apply   bool
	history bool
	args    []string
	modes   []modeFlag
}

// validatePruneFlags enforces the argument grammar BEFORE any I/O, in
// validateRepairOpts's style: a malformed invocation never reaches the
// filesystem, the lifecycle lock, or tmux.
//
// EVERY REFUSAL NAMES BOTH SIDES, and the direction is symmetric: a retention
// flag without --prune refuses, and an operand or a mode flag WITH --prune
// refuses. A flag or operand that silently does nothing is the shape where an
// operator believes a scope was applied and it was not — and under --prune that
// belief is expensive, because the operand reads as "sweep this one record"
// while the sweep is directory-wide and unlinks.
func validatePruneFlags(cmd *cobra.Command, g pruneGrammar) error {
	if g.prune && g.apply {
		return fmt.Errorf("--prune and --apply cannot be combined: " +
			"--apply settles the one record you name, --prune sweeps every record that was already set aside")
	}
	if g.prune && g.history {
		return fmt.Errorf("--prune and --history cannot be combined: " +
			"--history reads the audit trail, --prune rewrites it")
	}
	if g.prune {
		for _, m := range g.modes {
			if m.set {
				return fmt.Errorf("--prune and %s cannot be combined: "+
					"%s settles the one record you name, --prune sweeps the whole session directory", m.name, m.name)
			}
		}
		if len(g.args) > 0 {
			return fmt.Errorf("--prune takes no breadcrumb: it sweeps every set-aside record in the session "+
				"directory, so naming %s would not scope it — drop the operand, "+
				"or settle that one record with --apply instead",
				termsafe.QuotePath(g.args[0]))
		}
		return nil
	}
	for _, name := range []string{"older-than", "log-retention"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s only applies to --prune; add --prune, or drop the flag", name)
		}
	}
	return nil
}

// runRepairPrune is the housekeeping arm. It exits 0 on success: unlike the
// inspect, this is an ACTION, and "how many files did you remove" is not the
// yes/no question an exit code answers.
func runRepairPrune(cmd *cobra.Command, client *pr.Client, olderThan, logRetention string, dryRun, yes, asJSON bool) error {
	aside, err := pr.ParseRetention(olderThan)
	if err != nil {
		return fmt.Errorf("--older-than: %w", err)
	}
	logWindow, err := pr.ParseRetention(logRetention)
	if err != nil {
		return fmt.Errorf("--log-retention: %w", err)
	}
	report, err := client.Prune(cmd.Context(), pr.PruneOpts{
		OlderThan:    aside,
		LogRetention: logWindow,
		DryRun:       dryRun,
		Yes:          yes,
	})
	if err != nil {
		return err
	}
	if asJSON {
		return writePruneJSON(cmd.OutOrStdout(), report)
	}
	return writePruneHuman(cmd.OutOrStdout(), report)
}

// writePruneJSON encodes {"items":[…],"log":{…}} — an object, never a bare
// array, because the sweep answers two questions and a list of files could only
// carry one of them. Items is never null.
func writePruneJSON(out io.Writer, report pr.PruneReport) error {
	if report.Items == nil {
		report.Items = []pr.PruneItem{}
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// writePruneHuman prints one row per file plus one line for the log, so the
// two halves of the sweep are both visible without --json.
func writePruneHuman(out io.Writer, report pr.PruneReport) error {
	if len(report.Items) == 0 {
		_, _ = fmt.Fprintln(out, "no set-aside records to prune")
	}
	for _, it := range report.Items {
		age := it.Age
		if age == "" {
			// A blank column would read as "brand new" rather than "nobody
			// could tell", which is the difference that decides whether the
			// file was removable at all.
			age = "?"
		}
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n", age, it.Outcome, termsafe.QuotePathIfUnsafe(it.Path))
		if it.Reason != "" {
			_, _ = fmt.Fprintf(out, "  reason: %s\n", safeTerm(it.Reason))
		}
		if it.Error != "" {
			_, _ = fmt.Fprintf(out, "  error: %s\n", safeTerm(it.Error))
		}
	}
	_, _ = fmt.Fprintf(out, "log\t%s\t%s (dropped %d, kept %d)\n",
		report.Log.Outcome, termsafe.QuotePathIfUnsafe(report.Log.Path), report.Log.Dropped, report.Log.Kept)
	if report.Log.Error != "" {
		_, _ = fmt.Fprintf(out, "  error: %s\n", safeTerm(report.Log.Error))
	}
	return nil
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
		// The verb column says which command removed the thing. An empty verb
		// renders "-" because the row predates the field — the log is
		// hand-editable and was written before teardown and cleanup recorded
		// themselves, so silence there is unknown, never "this was a repair".
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.TS.Format("2006-01-02T15:04:05Z07:00"), safeTerm(dashIfEmpty(r.Verb)), safeTerm(dashIfEmpty(r.Mode)), safeTerm(r.Outcome),
			safeTerm(r.Ref), termsafe.QuotePathIfUnsafe(r.RecordPath))
		// The note is not optional detail. A shrunken ref or workspace stays
		// well-formed, so without this line the default reader sees a truncated
		// value as a complete one — the exact mistake the note exists to
		// prevent, in the one view that was dropping it.
		if r.RecordNote != "" {
			_, _ = fmt.Fprintf(out, "  note: %s\n", safeTerm(r.RecordNote))
		}
		// A compaction row's record_path names the log itself, so without its
		// detail the row says only "prune touched this file" — the count of
		// what it dropped lives nowhere else in this view.
		if r.Detail != "" {
			_, _ = fmt.Fprintf(out, "  detail: %s\n", safeTerm(r.Detail))
		}
	}
	return nil
}

// dashIfEmpty renders an absent column as "-" so a blank cell cannot be read
// as a value the row actually carried.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
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

// writeRepairHuman renders the report. The exit code is repairExitCode's.
func writeRepairHuman(cmd *cobra.Command, report pr.RepairReport, apply bool) error {
	out := cmd.OutOrStdout()
	if len(report.Items) == 0 {
		_, _ = fmt.Fprintln(out, "no records need repair")
		return nil
	}
	for _, it := range report.Items {
		ref := safeTerm(it.Ref)
		if ref == "" {
			// An unreadable record has no ref to print, and a blank first
			// column would read as a row that simply lost its name.
			ref = "(unreadable)"
		}
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			ref, it.FromPhase, windowObservation(it), workspaceObservation(it),
			termsafe.QuotePathIfUnsafe(it.RecordPath))
		if it.Reason != "" {
			_, _ = fmt.Fprintf(out, "  reason: %s\n", safeTerm(it.Reason))
		}
		if it.Error != "" {
			_, _ = fmt.Fprintf(out, "  error: %s\n", safeTerm(it.Error))
		}
		if it.Outcome != "" && it.Outcome != "inspect" {
			_, _ = fmt.Fprintf(out, "  %s\n", it.Outcome)
		}
	}
	if !apply {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"%d record(s) need settling — see `forgectl pr repair --help` for the three ways to settle one\n", len(report.Items))
	}
	return nil
}

// repairExitCode is the honest code for an inspect: 1 when anything needs
// settling, 0 when nothing does.
//
// `pr repair` is the verb a script runs to ask whether a human is needed, so
// exiting 0 on "four sessions are stuck" would make that question unanswerable
// from the exit status. An --apply that reached here succeeded, so it exits 0
// regardless of what the report describes.
func repairExitCode(report pr.RepairReport, apply bool) error {
	if apply || len(report.Items) == 0 {
		return nil
	}
	return WithExitCode(fmt.Errorf("%d review session(s) are unsettled", len(report.Items)), 1)
}

// windowObservation renders what tmux said, never what the record claims — the
// distinction `pr repair` exists to surface.
//
// A nil observation prints "?" rather than a negative: on an unreadable record
// there may be no readable ref to derive a window from, and on an unreadable
// tmux there is no answer about any window. Rendering either as "no window"
// would state a fact nobody established.
func windowObservation(it pr.RepairItem) string {
	return observation(it.WindowLive, "window live", "no window")
}

func workspaceObservation(it pr.RepairItem) string {
	return observation(it.WorkspaceExists, "clean room present", "no clean room")
}

func observation(known *bool, whenTrue, whenFalse string) string {
	switch {
	case known == nil:
		return "?"
	case *known:
		return whenTrue
	default:
		return whenFalse
	}
}
