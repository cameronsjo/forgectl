package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface read <name>` prints a worker's screen, or with --report only the
// REPORT line its last brief asked for.

const (
	defaultReadLines = 40
	maxReadLines     = 200
	// maxReadLineRunes bounds one printed screen row.
	maxReadLineRunes = 400
)

// readResult is what `surface read --json` prints. Additive changes only
// (ADR-0008 rule 2). Text and Report are the worker's own output: untrusted,
// and escaped for the terminal like every --json string.
type readResult struct {
	Name    string `json:"name"`
	Harness string `json:"harness"`
	Agent   string `json:"agent,omitempty"`
	Status  string `json:"status,omitempty"`
	Text    string `json:"text,omitempty"`
	Marker  string `json:"marker,omitempty"`
	Found   *bool  `json:"found,omitempty"`
	Report  string `json:"report,omitempty"`
}

type readOptions struct {
	Repo   string
	Name   string
	Lines  int
	Report bool
	JSON   bool
}

func newSurfaceReadCmd(deps module.Deps) *cobra.Command {
	opts := readOptions{}
	cmd := &cobra.Command{
		Use:   "read <name>",
		Short: "Print a worker's screen, or its REPORT line",
		Long: `read prints the last --lines rows of the named worker's screen, with control
characters escaped.

With --report it prints only the report for the worker's last brief: the
text after "REPORT <marker>:" on a line below the last echo of that brief, so
the brief's own instruction can never count as its report. A report is the
worker's own claim; check it against git (git -C <worktree> status, git log)
before trusting it.

Exit 0: printed (with --report, a report was found). Exit 1: no report yet,
or the pane could not be read. Exit 2: a usage or setup error, including
--report for a worker with no recorded brief.

  forgectl surface read fix-login
  forgectl surface read fix-login --report --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceRead(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().IntVar(&opts.Lines, "lines", defaultReadLines, fmt.Sprintf("how many screen rows to print (at most %d)", maxReadLines))
	cmd.Flags().BoolVar(&opts.Report, "report", false, "print only the REPORT line for the last brief")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "print the result as JSON")
	return cmd
}

func runSurfaceRead(cmd *cobra.Command, deps module.Deps, opts readOptions) error {
	if opts.Lines <= 0 || opts.Lines > maxReadLines {
		return WithExitCode(fmt.Errorf("--lines must be 1-%d", maxReadLines), 2)
	}
	w, err := openWorker(cmd, deps, opts.Repo, opts.Name)
	if err != nil {
		return err
	}
	if opts.Report && w.row.Brief == nil {
		return WithExitCode(fmt.Errorf("worker %s has no recorded brief, so no report to look for", w.row.Name), 2)
	}
	s, err := w.read(cmd.Context())
	if err != nil {
		reason := "the worker's pane could not be read"
		if errors.Is(err, herdradapter.ErrWorkerGone) {
			reason = "the worker's herdr workspace is gone"
		}
		return WithExitCode(fmt.Errorf("%s: %s", reason, termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)), 1)
	}

	res := readResult{Name: w.row.Name, Harness: w.row.Harness, Agent: s.Agent, Status: s.Status}
	if opts.Report {
		report, found := worker.FindReport(s.Text, w.row.Brief.Marker)
		res.Marker, res.Found, res.Report = w.row.Brief.Marker, &found, report
		return reportRead(cmd, res, opts)
	}
	res.Text = tailLines(s.Text, opts.Lines)
	return reportRead(cmd, res, opts)
}

// tailLines returns the last n lines of text.
func tailLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func reportRead(cmd *cobra.Command, r readResult, opts readOptions) error {
	out := cmd.OutOrStdout()
	if opts.JSON {
		if err := writeJSON(out, r); err != nil {
			return err
		}
		if opts.Report && !*r.Found {
			return newSilentCodedError(1)
		}
		return nil
	}
	if opts.Report {
		if !*r.Found {
			return WithExitCode(fmt.Errorf("worker %s: no REPORT %s line on screen yet", r.Name, r.Marker), 1)
		}
		_, err := fmt.Fprintln(out, termsafe.SafeLineMax(r.Report, 2000))
		return err
	}
	var b strings.Builder
	for line := range strings.SplitSeq(r.Text, "\n") {
		b.WriteString(termsafe.SafeLineMax(line, maxReadLineRunes))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(out, b.String())
	return err
}
