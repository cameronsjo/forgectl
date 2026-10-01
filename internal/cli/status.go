package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/status"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// statusModule declares the read-only cross-project overview (forgectl#13,
// lane 1). It owns no config section: it reads the ones its sources' own
// modules own ([github], [clean], [bench]).
var statusModule = module.Manifest{
	Name: "status",
	Tier: module.TierExtension,
	New:  newStatusCmd,
}

// statusDefaultTimeout is each section's default deadline. It bounds the
// `pr dash` gh searches, the clean walk and the bench probes alike, so one
// stalled source cannot hold the other three.
const statusDefaultTimeout = 20 * time.Second

// Human-view caps. The JSON report carries every row; the text view is an
// overview, so each list stops at a fixed count and says how many it hid.
const (
	statusProjectRowsMax = 10
	statusPRRowsMax      = 5
	statusNameMaxRunes   = 80
	statusTitleMaxRunes  = 72
	statusReasonMaxRunes = 120
)

// statusSources are the four read paths the overview composes. The command
// builder wires the shipped ones; tests inject fakes through
// newStatusCmdForSources.
type statusSources struct {
	Git   status.Source[statusGitJSON]
	PRs   status.Source[prDashJSON]
	Clean status.Source[statusCleanJSON]
	Bench status.Source[bench.Report]
}

// statusReportJSON is the `status --json` wire shape: one status.Section per
// source, each always present. See docs/commands/status.md.
type statusReportJSON struct {
	Git   status.Section[statusGitJSON]   `json:"git"`
	PRs   status.Section[prDashJSON]      `json:"prs"`
	Clean status.Section[statusCleanJSON] `json:"clean"`
	Bench status.Section[bench.Report]    `json:"bench"`
}

// statusGitJSON is the git section: every local project under the projects
// root with its working-tree state, plus the counts the human line prints.
// Dirty and ahead overlap (a tree can be both); clean + dirty-or-ahead +
// unknown + not_a_repo covers every row.
type statusGitJSON struct {
	Root     string              `json:"root"`
	Total    int                 `json:"total"`
	Clean    int                 `json:"clean"`
	Dirty    int                 `json:"dirty"`
	Ahead    int                 `json:"ahead"`
	Unknown  int                 `json:"unknown"`
	NotARepo int                 `json:"not_a_repo"`
	Projects []statusProjectJSON `json:"projects"`
}

// statusProjectJSON is one local project. Status is the same object
// `projects list --json` carries per cloned repo.
type statusProjectJSON struct {
	Name   string             `json:"name"`
	Path   string             `json:"path"`
	Status projects.GitStatus `json:"status"`
}

// statusCleanJSON is the clean section: the `clean --json` dry-run totals
// without the per-target rows (`clean --json` lists those).
type statusCleanJSON struct {
	Root                  string `json:"root"`
	TotalReclaimableBytes int64  `json:"total_reclaimable_bytes"`
	Reclaimable           int    `json:"reclaimable"`
	Skipped               int    `json:"skipped"`
}

// newStatusCmd builds `forgectl status` over the shipped read paths.
func newStatusCmd(deps module.Deps) *cobra.Command {
	return newStatusCmdForSources(defaultStatusSources(deps), deps.Theme)
}

// defaultStatusSources wires each section to the read path its own verb
// uses, so the overview reports what the individual commands report.
func defaultStatusSources(deps module.Deps) statusSources {
	return statusSources{
		Git: func(ctx context.Context) (statusGitJSON, []string, error) {
			client := projects.New(deps.Runner)
			found, err := client.Discover(ctx)
			if err != nil {
				return statusGitJSON{}, nil, err
			}
			return newStatusGit(client.ProjectsDir(), found), nil, nil
		},
		PRs: func(ctx context.Context) (prDashJSON, []string, error) {
			client := pr.New(deps.Runner, prGitHubHostOption(deps.Runner, deps.Cfg.Github.Host))
			dash, notes, err := client.Dash(ctx)
			if err != nil {
				return prDashJSON{}, nil, err
			}
			// err discarded: "" degrades to an empty store on read, as in pr dash.
			reviewedPath, _ := config.PrReviewedPath()
			store := pr.LoadReviewed(reviewedPath, pr.WithDefaultHost(client.GitHubHost()))
			return buildPrDashJSON(dash, store), notes, nil
		},
		Clean: func(ctx context.Context) (statusCleanJSON, []string, error) {
			client := cleanpkg.New(deps.Runner, cleanpkg.WithCleanConfig(deps.Cfg.Clean))
			// Preview is the scan-and-classify pair `clean --json` runs, behind
			// an API with no apply parameter: this section cannot delete by
			// construction, not by a literal someone could flip.
			root, preview, err := client.Preview(ctx)
			if err != nil {
				return statusCleanJSON{}, nil, err
			}
			return newStatusClean(root, preview), nil, nil
		},
		Bench: func(ctx context.Context) (bench.Report, []string, error) {
			return bench.Status(ctx, deps.Cfg, deps.Runner, bench.NewHTTPProber()), nil, nil
		},
	}
}

// newStatusCmdForSources is the test seam.
func newStatusCmdForSources(src statusSources, th theme.Theme) *cobra.Command {
	var (
		asJSON  bool
		strict  bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Read-only overview: local git state, PRs, reclaimable space, bench health",
		Long: `status composes four read paths that already ship into one overview:

  git    working-tree state of every local project (as projects discovery sees it)
  prs    the pr dash sections: active reviews, PRs awaiting you, your open PRs
  clean  the clean dry-run total: reclaimable dep/build directories
  bench  the bench status health card: hearth and chronicle

It changes nothing. The sections run concurrently, each under its own
--timeout. A section whose source fails, panics or runs out of time is
reported as failed, and the rest still report: a failed section never fails
the command. --strict exits 1 after the report when any section is not ok.

  forgectl status                  the overview
  forgectl status --json           every section, every row, for scripts
  forgectl status --json --strict  same, but exit 1 when a section degraded or failed
  forgectl status --timeout 5s     give each section five seconds`,
		Args: cobra.NoArgs,
		// Mirrors projects list: --strict fails AFTER the report is written,
		// and cobra's usage text must never follow it onto stdout.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return errors.New("invalid --timeout: must be greater than zero")
			}
			report := collectStatus(cmd.Context(), src, timeout)
			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), report); err != nil {
					return err
				}
			} else {
				out := th.Writer(cmd.OutOrStdout(), os.Environ())
				renderStatus(out, report, th.Marks())
			}
			if n := report.notOK(); strict && n > 0 {
				return jsonVerdict(WithExitCode(fmt.Errorf("%d section(s) degraded or failed (--strict)", n), 1), asJSON)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		`emit {"git":S,"prs":S,"clean":S,"bench":S} to stdout, each S {"state":"ok|degraded|failed","error":...,"notes":[...],"data":...}; data is null only when state is failed`)
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 1 when any section is degraded or failed (the report is still written)")
	cmd.Flags().DurationVar(&timeout, "timeout", statusDefaultTimeout, "deadline for each section; a section that misses it is reported as failed")
	return cmd
}

// collectStatus runs the four sources concurrently, each under timeout.
func collectStatus(ctx context.Context, src statusSources, timeout time.Duration) statusReportJSON {
	var r statusReportJSON
	var wg sync.WaitGroup
	wg.Go(func() { r.Git = status.Collect(ctx, timeout, src.Git) })
	wg.Go(func() { r.PRs = status.Collect(ctx, timeout, src.PRs) })
	wg.Go(func() { r.Clean = status.Collect(ctx, timeout, src.Clean) })
	wg.Go(func() { r.Bench = status.Collect(ctx, timeout, src.Bench) })
	wg.Wait()
	return r
}

// notOK counts the sections that are not StateOK.
func (r statusReportJSON) notOK() int {
	n := 0
	for _, s := range []status.State{r.Git.State, r.PRs.State, r.Clean.State, r.Bench.State} {
		if s != status.StateOK {
			n++
		}
	}
	return n
}

// newStatusGit folds a discovery result into the git section.
func newStatusGit(root string, found []projects.Project) statusGitJSON {
	g := statusGitJSON{Root: root, Total: len(found), Projects: make([]statusProjectJSON, 0, len(found))}
	for _, p := range found {
		g.Projects = append(g.Projects, statusProjectJSON{Name: p.Name, Path: p.Dir, Status: p.Status})
		switch p.Status.State {
		case projects.StatusNotRepo:
			g.NotARepo++
		case projects.StatusOK:
			dirty := p.Status.Modified > 0 || p.Status.Untracked > 0
			if dirty {
				g.Dirty++
			}
			if p.Status.Ahead > 0 {
				g.Ahead++
			}
			if !dirty && p.Status.Ahead == 0 {
				g.Clean++
			}
		default:
			g.Unknown++
		}
	}
	return g
}

// newStatusClean folds a clean dry-run preview into the clean section.
func newStatusClean(root string, preview cleanpkg.Result) statusCleanJSON {
	reclaimable := countReclaimable(preview.Items)
	return statusCleanJSON{
		Root:                  root,
		TotalReclaimableBytes: preview.TotalReclaimable,
		Reclaimable:           reclaimable,
		Skipped:               len(preview.Items) - reclaimable,
	}
}

// statusSectionView is one section as both human views show it: the text
// overview (renderStatus) and the cockpit (status --tui). Headline is the
// sentence after the label, computed once here so the two views cannot drift
// apart; it is "failed: <error>" for a section with no data.
type statusSectionView struct {
	Label    string
	State    status.State
	Error    string
	Notes    []string
	HasData  bool
	Headline string
}

// The four sections in report order, as indexes into statusSectionViews.
const (
	statusIdxGit = iota
	statusIdxPRs
	statusIdxClean
	statusIdxBench
	statusSectionCount
)

// statusSectionLabels names the sections in report order. The cockpit labels
// its sections from here before any of them has loaded.
var statusSectionLabels = [statusSectionCount]string{
	statusIdxGit:   "git",
	statusIdxPRs:   "prs",
	statusIdxClean: "clean",
	statusIdxBench: "bench",
}

// statusSectionViews folds the report into its four section views, in the
// order both human views print them. A section with no data is failed
// whatever its state says.
func statusSectionViews(r statusReportJSON) [statusSectionCount]statusSectionView {
	views := [statusSectionCount]statusSectionView{
		statusIdxGit:   statusView(statusSectionLabels[statusIdxGit], r.Git.State, r.Git.Error, r.Git.Notes, r.Git.Data != nil),
		statusIdxPRs:   statusView(statusSectionLabels[statusIdxPRs], r.PRs.State, r.PRs.Error, r.PRs.Notes, r.PRs.Data != nil),
		statusIdxClean: statusView(statusSectionLabels[statusIdxClean], r.Clean.State, r.Clean.Error, r.Clean.Notes, r.Clean.Data != nil),
		statusIdxBench: statusView(statusSectionLabels[statusIdxBench], r.Bench.State, r.Bench.Error, r.Bench.Notes, r.Bench.Data != nil),
	}
	if r.Git.Data != nil {
		views[statusIdxGit].Headline = statusGitHeadline(*r.Git.Data)
	}
	if r.PRs.Data != nil {
		views[statusIdxPRs].Headline = statusPRsHeadline(*r.PRs.Data)
	}
	if r.Clean.Data != nil {
		views[statusIdxClean].Headline = statusCleanHeadline(*r.Clean.Data)
	}
	if r.Bench.Data != nil {
		views[statusIdxBench].Headline = statusBenchHeadline(*r.Bench.Data)
	}
	return views
}

// statusView builds one section view without its headline; a failed one gets
// its "failed: <error>" headline here.
func statusView(label string, state status.State, errText string, notes []string, hasData bool) statusSectionView {
	v := statusSectionView{Label: label, State: state, Error: errText, Notes: notes, HasData: hasData}
	if !hasData || (state != status.StateOK && state != status.StateDegraded) {
		v.State = status.StateFailed
		v.HasData = false
		v.Headline = "failed: " + errText
	}
	return v
}

// renderStatus writes the human overview: one glyph-led headline per
// section, then a few indented detail rows. Every value that came from a
// filesystem, a subprocess or a server is escaped and capped here; notes and
// errors were escaped and capped by status.Collect.
func renderStatus(out io.Writer, r statusReportJSON, marks theme.Marks) {
	views := statusSectionViews(r)
	renderStatusSection(out, marks, views[statusIdxGit], func() { renderStatusGit(out, *r.Git.Data) })
	renderStatusSection(out, marks, views[statusIdxPRs], func() { renderStatusPRs(out, *r.PRs.Data) })
	renderStatusSection(out, marks, views[statusIdxClean], func() {})
	renderStatusSection(out, marks, views[statusIdxBench], func() { renderStatusBench(out, *r.Bench.Data, marks) })
}

// renderStatusSection prints a section's glyph, label and headline, then
// either nothing more (a failed section) or its detail rows followed by its
// degradation notes. rows is only called when the section has data, so it may
// dereference Data.
func renderStatusSection(out io.Writer, marks theme.Marks, v statusSectionView, rows func()) {
	var glyph string
	switch v.State {
	case status.StateOK:
		glyph = marks.OK
	case status.StateDegraded:
		glyph = marks.Warn
	default:
		glyph = marks.Fail
	}
	_, _ = fmt.Fprintf(out, "%s %-5s  %s\n", glyph, v.Label, v.Headline)
	if !v.HasData {
		return
	}
	rows()
	for _, n := range v.Notes {
		_, _ = fmt.Fprintf(out, "    note: %s\n", n)
	}
}

// statusGitHeadline is the git section's headline: the project counts.
func statusGitHeadline(g statusGitJSON) string {
	return fmt.Sprintf("%d project(s) under %s: %d clean, %d dirty, %d ahead, %d unknown",
		g.Total, termsafe.QuotePath(g.Root), g.Clean, g.Dirty, g.Ahead, g.Unknown)
}

// statusPRsHeadline is the prs section's headline: the three dash counts.
func statusPRsHeadline(d prDashJSON) string {
	return fmt.Sprintf("%d active review(s), %d awaiting you, %d open by you",
		len(d.ActiveReviews), len(d.AwaitingYou), len(d.YourOpen))
}

// statusCleanHeadline is the clean section's headline: the dry-run total.
func statusCleanHeadline(c statusCleanJSON) string {
	return fmt.Sprintf("%s reclaimable across %d target(s), %d skipped, under %s",
		formatBytes(c.TotalReclaimableBytes), c.Reclaimable, c.Skipped, termsafe.QuotePath(c.Root))
}

// statusBenchHeadline is the bench section's headline: each component's
// state.
func statusBenchHeadline(b bench.Report) string {
	parts := make([]string, 0, 2)
	for _, c := range []bench.Component{b.Hearth, b.Chronicle} {
		parts = append(parts, termsafe.SafeLineMax(c.Name, statusNameMaxRunes)+" "+termsafe.SafeLineMax(string(c.State), statusNameMaxRunes))
	}
	return strings.Join(parts, ", ")
}

// renderStatusGit prints, under the counts headline, the projects that need
// attention: dirty, ahead, or unreadable. Clean trees and plain directories
// are counted, not listed.
func renderStatusGit(out io.Writer, g statusGitJSON) {
	shown, hidden := 0, 0
	for _, p := range g.Projects {
		label := ""
		switch p.Status.State {
		case projects.StatusNotRepo:
			continue
		case projects.StatusOK:
			label = p.Status.Label()
			if label == "[clean]" {
				continue
			}
		default:
			label = "[status unknown]"
		}
		if shown == statusProjectRowsMax {
			hidden++
			continue
		}
		shown++
		_, _ = fmt.Fprintf(out, "    %s  %s\n", termsafe.QuotePathMax(p.Name, statusNameMaxRunes), label)
	}
	if hidden > 0 {
		_, _ = fmt.Fprintf(out, "    … %d more (status --json lists every project)\n", hidden)
	}
}

// renderStatusPRs prints the PRs awaiting you under the prs headline.
func renderStatusPRs(out io.Writer, d prDashJSON) {
	for i, p := range d.AwaitingYou {
		if i == statusPRRowsMax {
			_, _ = fmt.Fprintf(out, "    … %d more (pr dash)\n", len(d.AwaitingYou)-i)
			break
		}
		_, _ = fmt.Fprintf(out, "    %s  %s\n",
			termsafe.SafeLineMax(p.Ref, statusNameMaxRunes), termsafe.SafeLineMax(p.Title, statusTitleMaxRunes))
	}
}

// renderStatusBench prints a reason line for each component that is not ok,
// under the headline that names every component's state, with the glyph
// vocabulary `bench status` uses. Probe details stay in bench status.
func renderStatusBench(out io.Writer, b bench.Report, marks theme.Marks) {
	for _, c := range []bench.Component{b.Hearth, b.Chronicle} {
		if c.State == bench.StateOK {
			continue
		}
		_, _ = fmt.Fprintf(out, "    %s %s — %s\n", benchGlyph(c.State, marks),
			termsafe.SafeLineMax(c.Name, statusNameMaxRunes), termsafe.SafeLineMax(c.Reason, statusReasonMaxRunes))
	}
}
