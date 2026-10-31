package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
	"github.com/cameronsjo/forgectl/internal/workerwatch/ghsource"
)

// `surface watch` is a live, read-only view of chief-of-staff workers in hc.

const (
	defaultWatchInterval = 5 * time.Second
	minWatchInterval     = 2 * time.Second
	// maxWorkersFileBytes caps the workers file read.
	maxWorkersFileBytes = 1 << 20
	// watchSnapshotTimeout bounds the one collect --json or a pipe runs.
	watchSnapshotTimeout = 2 * time.Minute
)

var errWatchNoSource = errors.New("surface watch needs --workers FILE, --cos, or both")

type watchOptions struct {
	Workers  string
	Cos      bool
	JSON     bool
	Interval time.Duration
	GHTTL    time.Duration
}

// watchRuntime is the seam `surface watch` is tested through.
type watchRuntime struct {
	stdoutIsTerminal func(io.Writer) bool
	stdinIsTerminal  func(io.Reader) bool
	newCollector     func(opts watchOptions, entries []workerwatch.Entry, notes []string) *workerwatch.Collector
	hc               func() workerwatch.CLI
	run              func(ctx context.Context, opts tui.WorkersOptions) error
}

func productionWatchRuntime(deps module.Deps) watchRuntime {
	hc := func() workerwatch.CLI { return workerwatch.CLI{Run: deps.Runner, Stream: exec.OSRunner{}} }
	return watchRuntime{
		stdoutIsTerminal: docsReadOutputIsTerminal,
		stdinIsTerminal:  docsReadInputIsTerminal,
		hc:               hc,
		newCollector: func(opts watchOptions, entries []workerwatch.Entry, notes []string) *workerwatch.Collector {
			return &workerwatch.Collector{
				HC:      hc(),
				Git:     ghsource.GitCLI{Run: deps.Runner},
				GH:      &ghsource.GitHub{Run: githubauth.Runner(deps.Runner, githubauth.DefaultHost)},
				Reports: workerwatch.ScanReportDir,
				Entries: entries,
				Notes:   notes,
				Cos:     opts.Cos,
				GHTTL:   opts.GHTTL,
				// The same allowlist forgectl sweep counts markers by.
				Reviewers: deps.Cfg.Sweep.Reviewers,
			}
		},
		run: tui.RunWorkers,
	}
}

func newSurfaceWatchCmd(deps module.Deps) *cobra.Command {
	return newSurfaceWatchCmdWith(deps.Theme, productionWatchRuntime(deps))
}

func newSurfaceWatchCmdWith(th theme.Theme, rt watchRuntime) *cobra.Command {
	opts := watchOptions{}
	cmd := &cobra.Command{
		Use:   "watch --workers FILE | --cos",
		Short: "Watch chief-of-staff workers live: status, reports, PRs",
		Long: `watch shows one row per chief-of-staff worker running in hc (herdr-core):
its hc address, the status the agent reports about itself, its age and
model, the report files in its brief dir (which end in REPORT-COMPLETE,
and the newest), and its PR: number, head, CI rollup and the latest
cadence-review marker. A blocked worker is marked NEEDS YOU, with the hc
command that shows its dialog. The selected row's screen is shown below
the table and re-read every 2s.

watch only reads. It never sends keys, kills, or briefs a terminal, and
it strips escape sequences from worker screens before showing them.

Workers come from --workers, a file of name|worktree|brief dir lines (the
chief-of-staff workers file), and with --cos from every live hc terminal
named cos-*. Updates follow hc events; if the stream ends, watch polls
every --interval. GitHub is read once per worker per --gh-ttl, at most two
reads per refresh.

Off a terminal it prints one plain snapshot; --json prints one snapshot
as JSON.

Keys: up/down (or j/k) select, r refresh now, q quit.

REVIEW sums cadence-review markers the way forgectl sweep does: crit and
imp over every marker at the PR's head, counting only [sweep] reviewers
when that list is set. With the list empty, every reviewer name counts
(sweep counts none and refuses to run), so the watch errs alarming. STATUS and REVIEW are claims: the agent reports its
own status, and any session on your gh login can post a marker. Press ?
in the view for the full key and column list.

Exit 0: the view closed, or the snapshot printed. Exit 1: the snapshot
printed, but hc could not be read at all (not on PATH, or every read
failed; hc_unavailable says which). Exit 2: a usage error, or the workers
file could not be read.

  forgectl surface watch --workers workers.txt
  forgectl surface watch --cos
  forgectl surface watch --cos --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceWatch(cmd, th, rt, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Workers, "workers", "", "chief-of-staff workers file (name|worktree|brief dir per line)")
	cmd.Flags().BoolVar(&opts.Cos, "cos", false, "also watch every live hc terminal named cos-*")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print one snapshot as JSON: {"taken","workers":[...],"notes"}`)
	cmd.Flags().DurationVar(&opts.Interval, "interval", defaultWatchInterval, fmt.Sprintf("poll interval when hc events are unavailable (at least %s)", minWatchInterval))
	cmd.Flags().DurationVar(&opts.GHTTL, "gh-ttl", workerwatch.DefaultGHTTL, fmt.Sprintf("how long a worker's GitHub read is reused (at least %s)", workerwatch.MinGHTTL))
	return cmd
}

func runSurfaceWatch(cmd *cobra.Command, th theme.Theme, rt watchRuntime, opts watchOptions) error {
	if opts.Workers == "" && !opts.Cos {
		return WithExitCode(errWatchNoSource, exitUsage)
	}
	if opts.Interval < minWatchInterval {
		return WithExitCode(fmt.Errorf("--interval must be at least %s", minWatchInterval), exitUsage)
	}
	if opts.GHTTL < workerwatch.MinGHTTL {
		return WithExitCode(fmt.Errorf("--gh-ttl must be at least %s", workerwatch.MinGHTTL), exitUsage)
	}
	var entries []workerwatch.Entry
	var notes []string
	if opts.Workers != "" {
		var err error
		entries, notes, err = readWorkersFile(opts.Workers)
		if err != nil {
			return WithExitCode(err, exitUsage)
		}
	}
	c := rt.newCollector(opts, entries, notes)

	interactive := !opts.JSON && rt.stdoutIsTerminal(cmd.OutOrStdout()) && rt.stdinIsTerminal(cmd.InOrStdin()) && !termIsDumb()
	if !interactive {
		ctx, cancel := context.WithTimeout(cmd.Context(), watchSnapshotTimeout)
		defer cancel()
		snap := c.Collect(ctx, false)
		var err error
		if opts.JSON {
			err = writeJSON(cmd.OutOrStdout(), snap)
		} else {
			err = writeWatchPlain(cmd.OutOrStdout(), snap)
		}
		if err == nil && snap.HCUnavailable != "" {
			// The verdict is already on stdout (hc_unavailable, or a note).
			return newSilentCodedError(exitFailed)
		}
		return err
	}
	hc := rt.hc()
	return rt.run(cmd.Context(), tui.WorkersOptions{
		Collect: c.Collect,
		Read:    hc.Read,
		Events:  hc.Events,
		Poll:    opts.Interval,
		Theme:   th,
	})
}

func readWorkersFile(path string) ([]workerwatch.Entry, []string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the file to read; it is only parsed
	if err != nil {
		return nil, nil, fmt.Errorf("open the workers file %s: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	lr := &io.LimitedReader{R: f, N: maxWorkersFileBytes + 1}
	entries, notes, err := workerwatch.ParseWorkers(lr)
	if lr.N <= 0 {
		notes = append(notes, fmt.Sprintf("the workers file is over %d bytes; only its start was read, and its last line may be cut", maxWorkersFileBytes))
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the workers file %s: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	return entries, notes, nil
}

// writeWatchPlain is the off-terminal snapshot: one line per worker, then
// any notes. Every worker-supplied value is escaped.
func writeWatchPlain(out io.Writer, snap workerwatch.Snapshot) error {
	var b strings.Builder
	for _, w := range snap.Workers {
		status := w.Status
		switch {
		case w.HCError != "":
			status = "gone"
		case status == "":
			status = "-"
		}
		if w.NeedsYou() {
			status += " NEEDS-YOU"
		}
		pr := "-"
		if w.PR != nil {
			pr = fmt.Sprintf("#%d", w.PR.Number)
			if len(w.PR.Head) >= 7 {
				pr += "@" + w.PR.Head[:7]
			}
			if w.PR.Checks != "" {
				pr += " ci=" + strings.ToLower(w.PR.Checks)
			}
			switch r := w.PR.Review; {
			case r == nil:
				pr += " review=none"
			case r.AtHead == 0:
				pr += " review=stale"
			default:
				pr += fmt.Sprintf(" review crit=%d imp=%d markers=%d", r.Crit, r.Imp, r.AtHead)
			}
			if w.PRIssue == workerwatch.PRIssueGH {
				pr += " (stale: " + w.PRNote + ")"
			}
		} else if w.PRNote != "" {
			pr = w.PRNote
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\treports: %s\tpr: %s\n",
			termsafe.SafeLineMax(w.Name, 64), orDash(w.Address), status, orDash(w.Model),
			termsafe.SafeLineMax(w.Reports.String(), 200), termsafe.SafeLineMax(pr, 200))
	}
	for _, n := range snap.Notes {
		fmt.Fprintf(&b, "note: %s\n", termsafe.SafeLineMax(n, 300))
	}
	if snap.HCUnavailable != "" {
		fmt.Fprintf(&b, "note: %s\n", termsafe.SafeLineMax(snap.HCUnavailable, 300))
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return termsafe.SafeLineMax(s, 64)
}
