package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	updatepkg "github.com/cameronsjo/forgectl/internal/update"
)

// updateModule declares the weekly-maintenance extension (ADR-0005): the
// [update] config section's owner, no alias surface.
var updateModule = module.Manifest{
	Name:      "update",
	Tier:      module.TierExtension,
	ConfigKey: "update",
	New:       newUpdateCmd,
}

// newUpdateCmd builds `forgectl update` over the registry Deps.
func newUpdateCmd(deps module.Deps) *cobra.Command {
	client := updatepkg.New(deps.Runner)
	return newUpdateCmdForClient(client, deps.Cfg.Update)
}

// newUpdateCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *update.Client
// (mirrors newNetCmdForClient) without going through the full module.Deps
// wiring.
func newUpdateCmdForClient(client *updatepkg.Client, cfg config.UpdateConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Run weekly package-manager and OS maintenance as independently-scoped steps",
		Long: `update runs the weekly maintenance roster — brew, softwareupdate
(check-only, never installs), the go toolchain, and npm — as INDEPENDENTLY
SCOPED steps: one step's failure is recorded on that step alone and never
prevents the others from running.

  forgectl update check            report-only, no mutation, safe to run any time
  forgectl update run               INTERACTIVE: a destructive step prompts once
                                     for the whole batch — decline (or no
                                     terminal) skips them, reported as skipped;
                                     non-destructive steps (softwareupdate)
                                     always run regardless
  forgectl update run --yes         skip the prompt — apply every destructive
                                     step non-interactively (cron/CI)
  forgectl update run --only brew,go   restrict to a subset of the roster

Every run streams a full transcript to stderr and to a timestamped log file
(default: ` + "`forgectl config`" + `'s config dir, update-logs/; override with
[update] log_dir). stdout carries ONLY the summary — the per-step recap in
human mode, or the ` + "`--json`" + ` machine-readable report — never the transcript,
so ` + "`forgectl update run --json | jq`" + ` stays byte-clean.

Exit codes: 0 every selected step ok (or cleanly skipped), 1 one or more
steps failed, 2 a harness error (bad --only name, log/report I/O failure).`,
	}
	cmd.AddCommand(newUpdateCheckCmd(client, cfg), newUpdateRunCmd(client, cfg))
	return cmd
}

// newUpdateCheckCmd builds `update check` — always safe, never mutates.
func newUpdateCheckCmd(client *updatepkg.Client, cfg config.UpdateConfig) *cobra.Command {
	var only []string
	var asJSON bool

	cmd := &cobra.Command{
		Use: "check",
		// SilenceUsage/SilenceErrors mirror preflight's own setting: --json
		// is spec'd to emit ONLY the machine-readable summary on stdout, and
		// cobra's default auto-usage-on-error would otherwise append usage
		// text to that same stream on a failed step (exit 1).
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Report what each maintenance step would find — no mutation, always safe",
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdatePass(cmd, client, cfg, updatepkg.Options{CheckOnly: true}, only, asJSON)
		},
	}
	addRosterFlags(cmd, &only, &asJSON)
	return cmd
}

// newUpdateRunCmd builds `update run` — applies maintenance; destructive
// steps require --yes.
func newUpdateRunCmd(client *updatepkg.Client, cfg config.UpdateConfig) *cobra.Command {
	var only []string
	var asJSON, yes bool

	cmd := &cobra.Command{
		Use: "run",
		// See newUpdateCheckCmd's identical comment — same stdout-purity
		// requirement applies here.
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Apply weekly maintenance (destructive steps require --yes)",
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdatePass(cmd, client, cfg, updatepkg.Options{Yes: yes}, only, asJSON)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt and apply destructive steps (brew upgrade/cleanup, go clean, npm update) non-interactively — for cron/CI")
	addRosterFlags(cmd, &only, &asJSON)
	return cmd
}

// addRosterFlags registers the --only/--json pair shared by `update check`
// and `update run` — the two subcommands' flag sets are otherwise identical
// (run adds only --yes on top).
func addRosterFlags(cmd *cobra.Command, only *[]string, asJSON *bool) {
	cmd.Flags().StringSliceVar(only, "only", nil, "restrict to a subset of the roster (comma-separated step names)")
	cmd.Flags().BoolVar(asJSON, "json", false, "emit a machine-readable summary to stdout")
}

// confirmUpdateDestructive is `update run`'s confirmation seam — mirrors
// confirmAnyFile (env.go): a package-level var so a test can stub the huh
// prompt without a real tty, rather than exercising confirm() itself.
var confirmUpdateDestructive = confirm

// confirmDestructiveBatch decides whether a bare `update run` (no --yes)
// should treat its destructive steps as consented-to, following the house
// preview→confirm→apply convention (internal/cli/confirm.go; clean.go,
// branch.go). Three outcomes, all returning false except the one explicit
// yes:
//   - nothing destructive in the selected subset → nothing to gate, false
//     (moot either way — Options.Yes only affects destructive steps)
//   - a real terminal → one prompt naming every affected step; declining,
//     or the prompt itself erroring, both resolve to false
//   - no terminal → false without ever prompting
//
// false is never treated as an error here — it leaves the destructive
// steps exactly as Skipped as they were before this gate existed
// (Options.Yes simply stays unset), never a harness failure.
func confirmDestructiveBatch(client *updatepkg.Client, only []string, stderr io.Writer) bool {
	names := client.DestructiveStepNames(only)
	if len(names) == 0 {
		return false
	}
	if !isTerminal() {
		return false
	}
	ok, err := confirmUpdateDestructive(fmt.Sprintf("Apply %d destructive step(s) (%s)?", len(names), strings.Join(names, ", ")))
	if err != nil {
		fmt.Fprintf(stderr, "warning: confirmation prompt failed (%v); destructive step(s) will be skipped\n", err)
		return false
	}
	return ok
}

// runUpdatePass resolves --only against cfg's default roster and the
// client's real step names, gates any destructive step behind --yes or an
// interactive confirmation, opens the transcript (stderr + a timestamped
// log file, best-effort), runs the roster, prints the summary (JSON or
// human), and maps the outcome to an exit code.
func runUpdatePass(cmd *cobra.Command, client *updatepkg.Client, cfg config.UpdateConfig, opts updatepkg.Options, only []string, asJSON bool) error {
	ctx := cmd.Context()

	resolvedOnly := only
	if len(resolvedOnly) == 0 {
		resolvedOnly = cfg.Roster
	}
	if err := client.ValidateOnly(resolvedOnly); err != nil {
		return WithExitCode(err, 2)
	}
	opts.Only = resolvedOnly

	// `update check` (CheckOnly) never applies anything, so it never needs
	// this gate. `update run --yes` already carries explicit consent — no
	// prompt. Only a bare `update run` with a destructive step selected
	// reaches the confirmation.
	if !opts.CheckOnly && !opts.Yes && confirmDestructiveBatch(client, resolvedOnly, cmd.ErrOrStderr()) {
		opts.Yes = true
	}

	transcript, closeLog, logPath := openTranscript(cfg, cmd.ErrOrStderr())
	defer closeLog()
	if logPath != "" {
		fmt.Fprintf(transcript, "logging transcript to %s\n", logPath)
	}

	opts.OnStep = func(res updatepkg.Result) {
		writeStepLine(transcript, res)
	}
	report := client.Run(ctx, opts)

	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeUpdateJSON(out, report); err != nil {
			return WithExitCode(err, 2)
		}
	} else {
		printUpdateSummary(out, report)
	}

	if report.Failed() {
		return WithExitCode(fmt.Errorf("update: %w", report.Err()), 1)
	}
	return nil
}

// openTranscript returns a writer that duplicates every transcript line to
// stderr and to a timestamped log file, plus a closer and the log file's
// path (empty if the file couldn't be opened). Opening the log file reuses
// config.OpenAppendFile's shared mkdir+open trio (the same one
// config.SetupLogger's own log file goes through); this caller's fallback
// differs from SetupLogger's own silent one — it warns to stderr before
// degrading, since the transcript here is the CLI's whole point, not an
// incidental side channel.
func openTranscript(cfg config.UpdateConfig, stderr io.Writer) (w io.Writer, closeLog func(), path string) {
	dir := cfg.LogDir
	if dir == "" {
		if d, err := config.UpdateLogDir(); err == nil {
			dir = d
		}
	}
	if dir == "" {
		return stderr, func() {}, ""
	}

	name := "update-" + time.Now().Format("20060102-150405") + ".log"
	logPath := filepath.Join(dir, name)
	f, err := config.OpenAppendFile(logPath)
	if err != nil {
		fmt.Fprintf(stderr, "warning: could not open update log file %s: %v (continuing with stderr only)\n", logPath, err)
		return stderr, func() {}, ""
	}
	return io.MultiWriter(stderr, f), func() { _ = f.Close() }, logPath
}

// writeStepLine writes one line of the transcript for a completed step —
// the CLI's Options.OnStep hook, called as each step finishes so the
// transcript streams rather than appearing only once the whole roster is
// done.
func writeStepLine(w io.Writer, res updatepkg.Result) {
	dur := res.Duration.Round(time.Millisecond)
	switch {
	case res.Skipped:
		fmt.Fprintf(w, "skip  %-15s %s\n", res.Name, res.SkipReason)
	case res.Failed():
		fmt.Fprintf(w, "FAIL  %-15s (%s) %v\n", res.Name, dur, res.Err)
	default:
		fmt.Fprintf(w, "ok    %-15s (%s)\n", res.Name, dur)
	}
}

// tallyResults counts ok/skipped/failed across results — shared by
// printUpdateSummary's totals line and writeUpdateJSON's summary fields, so
// the two can never disagree on what counts as which.
func tallyResults(results []updatepkg.Result) (ok, skipped, failed int) {
	for _, res := range results {
		switch {
		case res.Skipped:
			skipped++
		case res.Failed():
			failed++
		default:
			ok++
		}
	}
	return ok, skipped, failed
}

// printUpdateSummary writes the human-readable summary to stdout: one line
// per step (identical shape to the transcript, since stdout in non-JSON
// mode IS the summary) plus a totals line.
func printUpdateSummary(out io.Writer, report updatepkg.Report) {
	for _, res := range report.Results {
		writeStepLine(out, res)
	}
	ok, skipped, failed := tallyResults(report.Results)
	fmt.Fprintf(out, "\n%d ok, %d skipped, %d failed\n", ok, skipped, failed)
}

// updateStepJSON is one step's --json wire shape.
type updateStepJSON struct {
	Name        string `json:"name"`
	Destructive bool   `json:"destructive"`
	Skipped     bool   `json:"skipped"`
	SkipReason  string `json:"skipReason,omitempty"`
	Failed      bool   `json:"failed"`
	Error       string `json:"error,omitempty"`
	DurationMs  int64  `json:"durationMs"`
}

// updateReportJSON is `update check`/`update run --json`'s stdout wire
// shape — a summary only; the full transcript never appears here (it went
// to stderr + the log file as the roster ran).
type updateReportJSON struct {
	Steps   []updateStepJSON `json:"steps"`
	Ok      int              `json:"ok"`
	Skipped int              `json:"skipped"`
	Failed  int              `json:"failed"`
}

// writeUpdateJSON encodes report as updateReportJSON to out.
func writeUpdateJSON(out io.Writer, report updatepkg.Report) error {
	j := updateReportJSON{Steps: make([]updateStepJSON, 0, len(report.Results))}
	for _, res := range report.Results {
		step := updateStepJSON{
			Name:        res.Name,
			Destructive: res.Destructive,
			Skipped:     res.Skipped,
			SkipReason:  res.SkipReason,
			Failed:      res.Failed(),
			DurationMs:  res.Duration.Milliseconds(),
		}
		if res.Err != nil {
			step.Error = res.Err.Error()
		}
		j.Steps = append(j.Steps, step)
	}
	j.Ok, j.Skipped, j.Failed = tallyResults(report.Results)
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(j)
}
