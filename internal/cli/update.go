package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
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
	return newUpdateCmdForClient(client, deps.Cfg.Update, deps.Theme)
}

// newUpdateCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *update.Client
// (mirrors newNetCmdForClient) without going through the full module.Deps
// wiring.
func newUpdateCmdForClient(client *updatepkg.Client, cfg config.UpdateConfig, th theme.Theme) *cobra.Command {
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

Every run writes a transcript to stderr and to a timestamped log file
(default: ` + "`forgectl config`" + `'s config dir, update-logs/; override with
[update] log_dir): a status line per step as it finishes, plus that step's
own captured output underneath it — for ` + "`check`" + `, the output IS the point
(brew's outdated list, softwareupdate's available updates, npm's outdated
table). The log file holds every step's full output (a failed command's
too) and a failed step's error text, each line escaped. The terminal shows the output escaped too,
except brew's, which is relayed from its taps and stays in the log file;
` + "`check`" + ` shows brew's outdated list rebuilt from formula names and versions
only. A failed step's line names the failed command and its exit status and
points at the log file (or, if it could not be opened, at --json); the
summary ends with the log file's path. Each step's
line appears only once that step completes, not as its command runs — a
slow step (a ten-minute ` + "`brew upgrade`" + `) prints nothing until it finishes,
so the transcript streams step-by-step, not byte-by-byte.
stdout carries ONLY the summary — the per-step recap in human mode, or the
` + "`--json`" + ` machine-readable report (which repeats each step's captured
output in its own field) — never the transcript, so
` + "`forgectl update run --json | jq`" + ` stays byte-clean.

CAUTION: run's destructive steps compound past their own individual risk.
brew's upgrade and cleanup run back to back in the same pass, so cleanup
removes the exact Cellar versions upgrade just superseded — the rollback
path clean.go's own --caches disclosure names is gone before you'd notice
upgrade broke something. go's cache clean wipes ` + "`~/go/pkg/mod`" + ` — the
module cache — machine-wide, for every project, not just one; the next
build of anything forces a full re-download. The confirmation prompt names
these effects when the relevant step is selected — decline (or omit
` + "`--yes`" + `) if you need to think about either first.

Exit codes: 0 every selected step ok (or cleanly skipped), 1 one or more
steps failed, 2 a harness error (bad --only name, log/report I/O failure).`,
	}
	cmd.AddCommand(newUpdateCheckCmd(client, cfg, th), newUpdateRunCmd(client, cfg, th))
	return cmd
}

// newUpdateCheckCmd builds `update check` — always safe, never mutates.
func newUpdateCheckCmd(client *updatepkg.Client, cfg config.UpdateConfig, th theme.Theme) *cobra.Command {
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
			return runUpdatePass(cmd, client, cfg, updatepkg.Options{CheckOnly: true}, only, asJSON, th)
		},
	}
	addRosterFlags(cmd, &only, &asJSON)
	return cmd
}

// newUpdateRunCmd builds `update run` — applies maintenance; destructive
// steps require --yes.
func newUpdateRunCmd(client *updatepkg.Client, cfg config.UpdateConfig, th theme.Theme) *cobra.Command {
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
			return runUpdatePass(cmd, client, cfg, updatepkg.Options{Yes: yes}, only, asJSON, th)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt and apply destructive steps non-interactively (cron/CI) — brew upgrade --formula + cleanup (removes superseded Cellar versions), go clean (wipes the module cache machine-wide), npm update -g")
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
func confirmDestructiveBatch(client *updatepkg.Client, only []string, stderr io.Writer, th theme.Theme) bool {
	names := client.DestructiveStepNames(only)
	if len(names) == 0 {
		return false
	}
	if !isTerminal() {
		return false
	}
	prompt := fmt.Sprintf("Apply %d destructive step(s) (%s)?", len(names), strings.Join(names, ", ")) + destructiveCaveat(names)
	ok, err := confirmUpdateDestructive(th, prompt)
	if err != nil {
		fmt.Fprintf(stderr, "warning: confirmation prompt failed (%v); destructive step(s) will be skipped\n", err)
		return false
	}
	return ok
}

// destructiveCaveat names the roster's two effects a bare step-name list
// doesn't disclose — brew's upgrade+cleanup combo removing the very Cellar
// versions it just superseded (the rollback path), and go clean wiping the
// module cache for every project, not just one — appended to the
// confirmation prompt so consent is informed about the blast radius, not
// just which steps are selected. Mirrors clean.go's hasBrewTarget pattern:
// the caveat only appears when the relevant step is actually in play.
func destructiveCaveat(names []string) string {
	var caveats []string
	if slices.Contains(names, updatepkg.StepBrew) {
		caveats = append(caveats, "brew's cleanup removes the Cellar versions upgrade just superseded — the rollback path")
	}
	if slices.Contains(names, updatepkg.StepGo) {
		caveats = append(caveats, "go clean wipes the module cache machine-wide, for every project")
	}
	if len(caveats) == 0 {
		return ""
	}
	return " (" + strings.Join(caveats, "; ") + ")"
}

// runUpdatePass resolves --only against cfg's default roster and the
// client's real step names, gates any destructive step behind --yes or an
// interactive confirmation, opens the transcript (stderr + a timestamped
// log file, best-effort), runs the roster, prints the summary (JSON or
// human), and maps the outcome to an exit code.
func runUpdatePass(cmd *cobra.Command, client *updatepkg.Client, cfg config.UpdateConfig, opts updatepkg.Options, only []string, asJSON bool, th theme.Theme) error {
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
	if !opts.CheckOnly && !opts.Yes && confirmDestructiveBatch(client, resolvedOnly, cmd.ErrOrStderr(), th) {
		opts.Yes = true
	}

	tr, closeLog := openTranscript(cfg, cmd.ErrOrStderr())
	defer closeLog()
	if tr.path != "" {
		_, _ = fmt.Fprintf(tr.both(), "logging transcript to %s\n", termsafe.QuotePath(tr.path))
	}

	checkOnly := opts.CheckOnly
	ref := tr.ref()
	opts.OnStep = func(res updatepkg.Result) {
		writeStepLine(tr.both(), res, ref)
		writeStepOutput(tr.term, res, checkOnly, ref)
		if tr.file != nil {
			writeStepDetail(tr.file, res)
		}
	}
	report := client.Run(ctx, opts)

	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeUpdateJSON(out, report); err != nil {
			return WithExitCode(err, 2)
		}
	} else {
		printUpdateSummary(out, report, ref, tr.path)
	}

	if report.Failed() {
		// Categorical (#778): report.Err() carries each failed command's
		// stderr. The step names are this package's own constants; the
		// transcript file holds the rest, escaped.
		// Under --json the summary on stdout is the verdict (forgectl#862).
		return jsonVerdict(WithExitCode(termsafe.Categorical("update: "+failedStepNames(report.Results)+" failed; "+tr.pointer(), report.Err()), 1), asJSON)
	}
	return nil
}

// updateTranscript is where a run's transcript goes: term (stderr) and, when
// it could be opened, file, a timestamped log under update-logs/. The two get
// different renderings of a step's subprocess text (#778): the terminal gets
// the fixed, categorical form, and the file gets the text itself, each line
// escaped with termsafe.SafeLine so `cat` on it is safe too.
type updateTranscript struct {
	term io.Writer
	file io.Writer // nil when the log file could not be opened
	path string    // the file's path; "" exactly when file is nil
}

// both is a writer for lines that go to the terminal and the file alike.
func (t updateTranscript) both() io.Writer {
	if t.file == nil {
		return t.term
	}
	return io.MultiWriter(t.term, t.file)
}

// pointer tells the reader where a step's full output and error text are:
// the transcript file when there is one, and otherwise the two ways to get
// them back (--json carries them, and log_level = "debug" logs them). It
// names the file's path, quoted with QuotePath so a path holding a space
// (macOS's Application Support) copy-pastes whole (#808). pointer is used
// once per run, in the returned error. The path is also the run's first
// stderr line ("logging transcript to") and the human summary's last stdout
// line (printUpdateSummary), and the root handler prints the returned error to
// stderr, so in human mode stderr carries the path twice. Every other line
// says ref instead.
func (t updateTranscript) pointer() string {
	if t.path != "" {
		return "see the transcript " + termsafe.QuotePath(t.path)
	}
	return noTranscriptPointer
}

// ref is pointer without the path, for the lines a run repeats (every FAIL
// line, brew's note): the path is in the run's first line and its summary.
func (t updateTranscript) ref() string {
	if t.path != "" {
		return "see the transcript"
	}
	return noTranscriptPointer
}

// noTranscriptPointer is pointer and ref when no transcript file was opened.
const noTranscriptPointer = `rerun with --json, or set log_level = "debug", for the details`

// openTranscript opens the run's transcript: stderr, plus a timestamped log
// file when one can be opened. Opening the log file reuses
// config.OpenAppendFile's shared mkdir+open trio (the same one
// config.SetupLogger's own log file goes through); this caller's fallback
// differs from SetupLogger's own silent one — it warns to stderr before
// degrading, since the transcript here is the CLI's whole point, not an
// incidental side channel.
func openTranscript(cfg config.UpdateConfig, stderr io.Writer) (updateTranscript, func()) {
	noFile := updateTranscript{term: stderr}
	dir := cfg.LogDir
	if dir == "" {
		d, err := config.UpdateLogDir()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not determine update log directory: %s (continuing with stderr only)\n", safeText(termsafe.Error(err).Error()))
			return noFile, func() {}
		}
		dir = d
	}

	name := "update-" + time.Now().Format("20060102-150405") + ".log"
	logPath := filepath.Join(dir, name)
	f, err := config.OpenAppendFile(logPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: could not open update log file %s: %s (continuing with stderr only)\n", termsafe.QuotePath(logPath), safeText(termsafe.Error(err).Error()))
		return noFile, func() {}
	}
	config.PruneUpdateLogs(dir)
	return updateTranscript{term: stderr, file: f, path: logPath}, func() { _ = f.Close() }
}

// writeStepLine writes one line of the transcript for a completed step —
// the CLI's Options.OnStep hook, called as each step finishes so the
// transcript streams rather than appearing only once the whole roster is
// done.
//
// ref ends a FAIL line: where the step's output and error text are.
func writeStepLine(w io.Writer, res updatepkg.Result, ref string) {
	dur := res.Duration.Round(time.Millisecond)
	switch {
	case res.Skipped:
		fmt.Fprintf(w, "skip  %-15s %s\n", res.Name, res.SkipReason)
	case res.Failed():
		_, _ = fmt.Fprintf(w, "FAIL  %-15s (%s) %s\n", res.Name, dur, stepFailure(res.Err, ref))
	default:
		fmt.Fprintf(w, "ok    %-15s (%s)\n", res.Name, dur)
	}
}

// stepFailure words a failed step for the terminal from fixed text (#778):
// err carries the child's stderr (a CommandError) and, for brew, text the
// tap's server and git transport send, so it is never rendered here. The
// failed command's name comes from the argv this binary built
// (SequenceError) and the exit status is an integer; ref says where the
// error text itself is (writeStepDetail puts it in the transcript file).
func stepFailure(err error, ref string) string {
	what := "failed"
	var seqErr *updatepkg.SequenceError
	if errors.As(err, &seqErr) {
		what = safeText(seqErr.Command) + " failed"
	}
	var cmdErr *exec.CommandError
	if errors.As(err, &cmdErr) && cmdErr.ExitCode >= 0 {
		what += " (exit " + strconv.Itoa(cmdErr.ExitCode) + ")"
	}
	return what + "; " + ref
}

// writeStepDetail writes a step's captured output and, when it failed, its
// error text to the transcript FILE, never the terminal: this is the
// recoverable detail the terminal's fixed wording points at. Each line is
// escaped with termsafe.SafeLine, so the file is inert under `cat` too.
//
// A failed command's stdout is in its CommandError, not Result.Output, when
// the step ran that one command (go clean, npm update -g), so it is written
// from there unless Output already holds it, as runSequence's does (#808).
func writeStepDetail(w io.Writer, res updatepkg.Result) {
	writeOutputLines(w, res.Output)
	if !res.Failed() {
		return
	}
	if extra := failedCommandOutput(res); extra != "" {
		writeOutputLines(w, extra)
	}
	for _, line := range strings.Split(transcriptErrorText(res.Err), "\n") {
		_, _ = fmt.Fprintf(w, "      error: %s\n", termsafe.SafeLine(line))
	}
}

// failedCommandOutput is a failed step's stdout that Result.Output does not
// already hold: a step that ran one command (go clean, npm update -g) keeps
// that command's stdout only in its CommandError (#808). The transcript file
// and --json both carry it, so the no-transcript pointer's "rerun with
// --json" is true for it too (#810). It is "" for a step that did not fail.
//
// It is redacted (redact.Text) as runSequence redacts the same stdout into
// Output (#941), so a line holding a credential shape reads as the marker in
// both renderers, and the Contains check compares like with like: against the
// raw text it would miss a sequence's redacted copy and write it twice.
func failedCommandOutput(res updatepkg.Result) string {
	if !res.Failed() {
		return ""
	}
	var cmdErr *exec.CommandError
	if !errors.As(res.Err, &cmdErr) || cmdErr.Output == "" {
		return ""
	}
	if out := redact.Text(cmdErr.Output); !strings.Contains(res.Output, out) {
		return out
	}
	return ""
}

// writeOutputLines writes captured output to the transcript file, one
// escaped line each.
func writeOutputLines(w io.Writer, output string) {
	if output == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "      | %s\n", termsafe.SafeLine(strings.TrimSuffix(line, "\r")))
	}
}

// transcriptErrorText is a failed step's error text for the transcript file.
// A SequenceError reads "<command>: <err>", and when err is a CommandError it
// names the command again ("brew update: brew update: …"), so the file gets
// the CommandError's text alone (#808). --json keeps Error() unchanged.
//
// A type assertion, not errors.As: only an error that IS the SequenceError
// can be replaced by its cause without dropping a wrapper's own text.
func transcriptErrorText(err error) string {
	if seqErr, ok := err.(*updatepkg.SequenceError); ok {
		var cmdErr *exec.CommandError
		if errors.As(seqErr.Err, &cmdErr) {
			return seqErr.Err.Error()
		}
	}
	return err.Error()
}

// failedStepNames lists the failed steps by name, comma-separated.
func failedStepNames(results []updatepkg.Result) string {
	var names []string
	for _, res := range results {
		if res.Failed() {
			names = append(names, safeLabel(res.Name))
		}
	}
	return strings.Join(names, ", ")
}

// stepOutputLineMaxRunes caps one rendered line of a step's captured output.
const stepOutputLineMaxRunes = 500

// brewTokenPattern is one field of a line `brew outdated` prints: a formula
// name (tap-qualified or not) or, in its verbose form, a version, a
// parenthesized version, a comparison sign, or a word of a bracketed note
// such as "[pinned at 1.2]". Nothing it matches can drive a terminal.
var brewTokenPattern = regexp.MustCompile(`^[A-Za-z0-9@._+/(),<>=!\[\]-]{1,128}$`)

// brewLineMaxTokens bounds a rebuilt brew line: "name (1.0) < 2.0" is four,
// and "[pinned at 1.0]" adds three.
const brewLineMaxTokens = 8

// writeStepOutput writes a step's captured output to the TERMINAL, indented
// under its status line: the final recap (printUpdateSummary) stays a terse
// one-line-per-step summary, and for `check` a step's output IS the point.
// Omitted entirely when Output is empty (a step that produced no stdout).
// The transcript file gets the full text from writeStepDetail instead.
//
// Output is subprocess text, so it is never written raw (#778). brew's stays
// off the terminal, as `forgectl upgrade`'s does (#777): brew relays what the
// tap's server and git transport send. ref says where it is instead. The
// one exception is `update check`, whose deliverable is brew's outdated list,
// so that list is rebuilt from formula-name and version tokens, with a count
// of any line that is not one. Every other step's lines are escaped and
// capped.
func writeStepOutput(w io.Writer, res updatepkg.Result, checkOnly bool, ref string) {
	if res.Output == "" {
		return
	}
	if res.Name != updatepkg.StepBrew {
		for _, line := range strings.Split(res.Output, "\n") {
			line = strings.TrimSuffix(line, "\r")
			_, _ = fmt.Fprintf(w, "      %s\n", termsafe.SafeLineMax(line, stepOutputLineMaxRunes))
		}
		return
	}
	slog.Debug("brew output.", "step", res.Name, "output", res.Output)
	if !checkOnly {
		_, _ = fmt.Fprintf(w, "      (brew's output is not shown here; %s)\n", ref)
		return
	}
	hidden := 0
	for _, line := range strings.Split(res.Output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if rebuilt, ok := brewTokens(line); ok {
			_, _ = fmt.Fprintf(w, "      %s\n", rebuilt)
			continue
		}
		hidden++
	}
	if hidden > 0 {
		_, _ = fmt.Fprintf(w, "      (%d line(s) of brew output not shown: not a formula line; %s)\n", hidden, ref)
	}
}

// brewTokens rebuilds one line of `brew outdated` from its fields, when every
// field is a brewTokenPattern token and there are at most brewLineMaxTokens.
func brewTokens(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || len(fields) > brewLineMaxTokens {
		return "", false
	}
	for _, f := range fields {
		if !brewTokenPattern.MatchString(f) {
			return "", false
		}
	}
	return strings.Join(fields, " "), true
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
// mode IS the summary), a totals line, and, when a transcript file was
// opened, its path: the one place the summary names it (#808).
func printUpdateSummary(out io.Writer, report updatepkg.Report, ref, transcriptPath string) {
	for _, res := range report.Results {
		writeStepLine(out, res, ref)
	}
	ok, skipped, failed := tallyResults(report.Results)
	_, _ = fmt.Fprintf(out, "\n%d ok, %d skipped, %d failed\n", ok, skipped, failed)
	if transcriptPath != "" {
		_, _ = fmt.Fprintf(out, "transcript: %s\n", termsafe.QuotePath(transcriptPath))
	}
}

// updateStepJSON is one step's --json wire shape.
type updateStepJSON struct {
	Name        string `json:"name"`
	Destructive bool   `json:"destructive"`
	Skipped     bool   `json:"skipped"`
	SkipReason  string `json:"skipReason,omitempty"`
	Failed      bool   `json:"failed"`
	Error       string `json:"error,omitempty"`
	// Output is the step's captured stdout — for `check`, this IS the
	// deliverable (brew's outdated list, npm's outdated table, …), so a
	// `--json` consumer must not have to go scrape the stderr transcript
	// to get it. A failed single-command step's stdout, which only its
	// CommandError holds, is included too (#810): the no-transcript pointer
	// sends the reader here for it.
	Output     string `json:"output,omitempty"`
	DurationMs int64  `json:"durationMs"`
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
			Output:      res.Output,
			DurationMs:  res.Duration.Milliseconds(),
		}
		if extra := failedCommandOutput(res); extra != "" {
			if step.Output != "" && !strings.HasSuffix(step.Output, "\n") {
				step.Output += "\n"
			}
			step.Output += extra
		}
		if res.Err != nil {
			step.Error = res.Err.Error()
		}
		j.Steps = append(j.Steps, step)
	}
	j.Ok, j.Skipped, j.Failed = tallyResults(report.Results)
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(j)
}
