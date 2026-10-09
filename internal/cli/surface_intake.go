package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/surface/intake"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface intake gh` turns open GitHub issues carrying an eligible label
// into queue rows. The gate and the brief live in internal/surface/intake;
// this file reads the checkout's origin, asks GitHub, and writes the queue.

// intakeMaxPages bounds the issue pages one run reads: 20 pages of
// intake.PageSize is 500 open labeled issues.
const intakeMaxPages = 20

// intakeItem is one issue in `surface intake gh --json`: enqueued (or, with
// --dry-run, would be) or skipped with a reason. Additive changes only
// (ADR-0008 rule 2).
type intakeItem struct {
	Number      int    `json:"number"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	Author      string `json:"author,omitempty"`
	BriefSHA256 string `json:"brief_sha256,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// intakeResult is what `surface intake gh --json` prints.
type intakeResult struct {
	DryRun   bool         `json:"dry_run"`
	Repo     string       `json:"repo"`
	GitHub   string       `json:"github"`
	Authors  []string     `json:"authors"`
	Labels   []string     `json:"labels"`
	Enqueued []intakeItem `json:"enqueued"`
	Skipped  []intakeItem `json:"skipped"`
	// Stopped says why the run ended before reading every eligible issue:
	// max_per_run, a full queue, the page limit, or a GitHub read failure.
	Stopped string `json:"stopped,omitempty"`
}

type intakeOptions struct {
	Repo    string
	Label   string
	Profile string
	Model   string
	Harness string
	DryRun  bool
	JSON    bool
}

func newSurfaceIntakeCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intake",
		Short: "Queue briefs from labeled GitHub issues",
		Long: `intake turns work items from another system into queue rows for surface drain.
gh reads open GitHub issues carrying an eligible label.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSurfaceIntakeGHCmd(deps))
	return cmd
}

func newSurfaceIntakeGHCmd(deps module.Deps) *cobra.Command {
	opts := intakeOptions{}
	cmd := &cobra.Command{
		Use:   "gh --repo <path>",
		Short: "Queue a brief for each open GitHub issue the operator labeled",
		Long: `gh reads the open issues of the repository --repo's origin remote names (on
github.com only) that carry an eligible label, and queues one row per issue
for surface drain. The label is the request. Removing it stops future
intake runs from taking the issue; a row already queued stays until
"forgectl surface dequeue <name>".

An issue is taken only when all of these hold, read from one GraphQL query:
its author is on [surface.intake] authors (default: the repository owner,
for a user-owned repository whose owner is the account gh is authenticated
as; any other repository needs authors set);
for each eligible label it carries, the latest labeling is by an allowed
author with no later removal; its body was never edited, or last edited
strictly before that labeling; its title was not renamed at or after it; it
was never transferred. Logins compare case-insensitively and exactly, and a
bot is never allowed.

The row's brief is a fixed set of rules with the issue's title and body
fenced as data between two lines carrying a random nonce. It tells the
worker not to read the issue or anything linked from it, never to create
issues or labels, and to open a draft pull request whose body says
"Closes #<number>". The rules are instructions, not a sandbox.

The row is named gh<number>-<repo name> (cut to 48 characters) and records
source gh:<owner>/<repo>#<number> and the issue's author. An existing row of
that name, in any state, skips the issue (a failed row: dequeue it to
retry). Each skip names the issue and its reason, and the run goes on.
[surface.intake] labels (default queue:drain, a label no triage sweep
applies) are the eligible labels; --label narrows to one of them. [surface.intake]
max_per_run (default 5) caps the rows one run adds. --harness, --model and
--profile apply to every row, as on enqueue.

--dry-run reads GitHub and the queue and writes nothing. --json prints
{"dry_run","repo","github","authors","labels","enqueued":[{"number","name",
"source","author","brief_sha256"}],"skipped":[{"number","name","source",
"author","reason"}],"stopped"}; with --dry-run, "enqueued" lists what would
be queued.

Exit 0: the run finished, whatever it skipped. Exit 1: the queue filled, or
GitHub could not be read; what was queued before stays queued. Exit 2: a
usage or setup error (an invalid [surface.intake], an origin not on
github.com, no authors set for a repository the gh account does not own).

  forgectl surface intake gh --repo forgectl --dry-run
  forgectl surface intake gh --repo . --label queue:drain --json
  forgectl surface intake gh --repo forgectl --harness codex --model gpt-5`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceIntakeGH(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", "", "local checkout whose origin names the GitHub repository (project name or path) — required")
	cmd.Flags().StringVar(&opts.Label, "label", "", "take only issues with this eligible label")
	cmd.Flags().StringVar(&opts.Profile, "profile", "", "[surface.profiles] name every row's worker runs under (main: the drain's own CLAUDE_CONFIG_DIR)")
	cmd.Flags().StringVar(&opts.Model, "model", "", "model every row's worker runs on, instead of the launch profile's")
	cmd.Flags().StringVar(&opts.Harness, "harness", worker.DefaultQueueHarness, "harness the drain launches every row with: claude, codex, or pi")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "list what would be queued and skipped; write nothing")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"dry_run","repo","github","authors","labels","enqueued","skipped","stopped"} as JSON`)
	return cmd
}

func runSurfaceIntakeGH(cmd *cobra.Command, deps module.Deps, opts intakeOptions) error {
	if opts.Repo == "" {
		return WithExitCode(errors.New("intake gh needs --repo"), exitUsage)
	}
	settings, err := deps.Cfg.Surface.Intake.Resolve()
	if err != nil {
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	labels := settings.Labels
	if opts.Label != "" {
		i := slices.IndexFunc(labels, func(l string) bool { return strings.EqualFold(l, opts.Label) })
		if i < 0 {
			return WithExitCode(fmt.Errorf("--label %s is not one of [surface.intake] labels", termsafe.QuoteArgMax(opts.Label, 0)), exitUsage)
		}
		labels = []string{labels[i]}
	}
	if err := checkQueueLaunchFlags(deps, opts.Profile, opts.Model, opts.Harness); err != nil {
		return err
	}
	ctx := cmd.Context()
	target, err := projects.New(deps.Runner).ResolveTarget(opts.Repo)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	top, err := worker.RepoTop(ctx, deps.Runner, target)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	owner, repo, err := githubOrigin(ctx, deps.Runner, top)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	q, err := worker.OpenQueue()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	in := intakeRun{
		ctx: ctx, gh: githubauth.Runner(deps.Runner, githubauth.DefaultHost), q: q,
		top: top, owner: owner, repo: repo, labels: labels, configured: settings.Authors, max: settings.MaxPerRun,
		launch: worker.QueueLaunch{Profile: opts.Profile, Model: opts.Model, Harness: opts.Harness},
		dryRun: opts.DryRun, now: time.Now, nonce: worker.NewMarker,
	}
	res, runErr := in.run()
	if err := reportIntake(cmd.OutOrStdout(), res, opts.JSON); err != nil {
		return err
	}
	return runErr
}

// githubOrigin reads owner and repository from top's origin remote, which
// must be on github.com. Only the parsed host is ever printed, never the URL,
// which can carry a credential.
func githubOrigin(ctx context.Context, run exec.Runner, top string) (owner, repo string, err error) {
	origin, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "remote", "get-url", "origin")
	if err != nil {
		return "", "", errors.New("intake: this checkout has no origin remote")
	}
	host, owner, repo, ok := pr.ParseRemoteURL(origin)
	if !ok {
		return "", "", errors.New("intake: origin's URL is not a form forgectl can read")
	}
	if host != githubauth.DefaultHost && host != "ssh.github.com" {
		return "", "", fmt.Errorf("intake: origin's host %s is not github.com; intake reads GitHub issues only", termsafe.QuoteTextMax(host, 40))
	}
	if !pr.ValidOwnerRepoPart(owner) || !pr.ValidOwnerRepoPart(repo) {
		return "", "", errors.New("intake: origin's owner or repository name is not one forgectl will use")
	}
	return owner, repo, nil
}

// intakeRun is one intake run's inputs and its seams.
type intakeRun struct {
	ctx        context.Context
	gh         exec.Runner
	q          *worker.Queue
	top        string
	owner      string
	repo       string
	labels     []string
	configured []string
	max        int
	launch     worker.QueueLaunch
	dryRun     bool
	now        func() time.Time
	nonce      func() (string, error)
}

// errIntakeStopped marks a run that ended early on something other than
// max_per_run or the page limit; the command exits 1 on it.
var errIntakeStopped = errors.New("intake stopped")

// run reads GitHub page by page and queues each issue the gate takes. It
// returns the result so far with every error, so the caller can print what
// was queued before the run stopped.
func (in intakeRun) run() (intakeResult, error) {
	res := intakeResult{
		DryRun: in.dryRun, Repo: in.top, GitHub: in.owner + "/" + in.repo, Labels: in.labels,
		Authors: []string{}, Enqueued: []intakeItem{}, Skipped: []intakeItem{},
	}
	rows, err := in.q.Rows()
	if err != nil {
		return res, WithExitCode(termsafe.Error(err), exitUsage)
	}
	var rules intake.Rules
	after := ""
	for page := 0; ; page++ {
		if page == intakeMaxPages {
			res.Stopped = fmt.Sprintf("read %d pages of issues, the most one run reads; run intake again for the rest", intakeMaxPages)
			return res, nil
		}
		p, err := in.fetch(after)
		if err != nil {
			res.Stopped = "GitHub could not be read: " + err.Error()
			return res, fmt.Errorf("%w: %w", errIntakeStopped, termsafe.Error(err))
		}
		if !strings.EqualFold(p.OwnerLogin, in.owner) {
			// GitHub followed a rename or a transfer to another owner. The
			// default author would then be someone the checkout never named.
			res.Stopped = fmt.Sprintf("GitHub names the repository's owner %q, origin names %q; update origin", p.OwnerLogin, in.owner)
			return res, WithExitCode(errors.New("intake: "+res.Stopped), exitUsage)
		}
		if page == 0 {
			authors, err := intake.Authors(p.OwnerType, p.OwnerLogin, p.ViewerLogin, in.configured)
			if err != nil {
				res.Stopped = err.Error()
				return res, WithExitCode(err, exitUsage)
			}
			rules = intake.Rules{Authors: authors, Labels: in.labels}
			res.Authors = authors
		}
		for _, is := range p.Issues {
			item, enqueue := in.consider(is, rules, rows)
			if !enqueue {
				res.Skipped = append(res.Skipped, item.view)
				continue
			}
			if len(res.Enqueued) >= in.max {
				res.Stopped = fmt.Sprintf("max_per_run (%d) reached; run intake again for the rest", in.max)
				return res, nil
			}
			if in.dryRun {
				res.Enqueued = append(res.Enqueued, item.view)
				continue
			}
			row, added, err := in.q.EnqueueFrom(item.view.Name, in.top, item.brief, "", in.launch,
				worker.QueueOrigin{Source: item.view.Source, Author: item.view.Author}, in.now())
			switch {
			case errors.Is(err, worker.ErrQueueFull):
				res.Stopped = "the queue is full; dequeue finished rows first"
				return res, fmt.Errorf("%w: %w", errIntakeStopped, termsafe.Error(err))
			case err != nil:
				item.view.Reason = err.Error()
				item.view.BriefSHA256 = ""
				res.Skipped = append(res.Skipped, item.view)
			case !added:
				item.view.Reason = fmt.Sprintf("already in the queue (state %s)", row.State)
				item.view.BriefSHA256 = ""
				res.Skipped = append(res.Skipped, item.view)
			default:
				res.Enqueued = append(res.Enqueued, item.view)
			}
		}
		if !p.HasNextPage {
			return res, nil
		}
		after = p.EndCursor
	}
}

// fetch runs the query for one page, pinned to github.com.
func (in intakeRun) fetch(after string) (intake.Page, error) {
	args := []string{"api", "graphql", "--hostname", githubauth.DefaultHost,
		"-f", "query=" + intake.Query,
		"-f", "owner=" + in.owner, "-f", "name=" + in.repo,
		"-F", "first=" + strconv.Itoa(intake.PageSize)}
	for _, l := range in.labels {
		args = append(args, "-f", "labels[]="+l)
	}
	if after != "" {
		args = append(args, "-f", "after="+after)
	}
	out, err := in.gh.Run(in.ctx, "gh", args...)
	if err != nil {
		return intake.Page{}, fmt.Errorf("gh api graphql: %w", err)
	}
	return intake.DecodePage([]byte(out))
}

// considered is one issue's outcome before the queue write: the item to
// report and, when the gate took it, the brief.
type considered struct {
	view  intakeItem
	brief string
}

// consider decides one issue: skip, with a reason, or enqueue, with its
// brief. rows is the queue as read when the run began; a row written since
// is caught by the enqueue itself.
func (in intakeRun) consider(is intake.Issue, rules intake.Rules, rows []worker.QueueRow) (considered, bool) {
	c := considered{view: intakeItem{
		Number: is.Number, Name: intake.RowName(is.Number, in.repo), Source: intake.Source(in.owner, in.repo, is.Number),
	}}
	if is.Author != nil {
		c.view.Author = is.Author.Login
	}
	skip := func(reason string) (considered, bool) {
		c.view.Reason = reason
		return c, false
	}
	if err := worker.ValidName(c.view.Name); err != nil {
		return skip("its row name is not usable: " + err.Error())
	}
	for _, r := range rows {
		if r.Name != c.view.Name {
			continue
		}
		if r.Repo != in.top {
			return skip(fmt.Sprintf("the row name %s is taken by a row for %s", r.Name, r.Repo))
		}
		if r.State == worker.QueueFailed {
			return skip("already in the queue (state failed); dequeue it to retry")
		}
		return skip(fmt.Sprintf("already in the queue (state %s)", r.State))
	}
	if err := intake.Admit(is, rules); err != nil {
		return skip(err.Error())
	}
	brief, err := intake.Brief(in.owner+"/"+in.repo, is, in.nonce)
	if err != nil {
		return skip(err.Error())
	}
	if err := worker.CheckQueueBrief(brief); err != nil {
		return skip(err.Error())
	}
	if err := (worker.QueueOrigin{Source: c.view.Source, Author: c.view.Author}).Check(); err != nil {
		return skip(err.Error())
	}
	c.view.BriefSHA256 = worker.BriefSHA256(brief)
	c.brief = brief
	return c, true
}

func reportIntake(out io.Writer, r intakeResult, asJSON bool) error {
	if asJSON {
		return writeJSON(out, r)
	}
	verb := "queued"
	if r.DryRun {
		verb = "would queue"
	}
	for _, it := range r.Enqueued {
		if _, err := fmt.Fprintf(out, "%s %s (#%d)\n", verb, termsafe.SafeLineMax(it.Name, 64), it.Number); err != nil {
			return err
		}
	}
	for _, it := range r.Skipped {
		if _, err := fmt.Fprintf(out, "skipped #%d: %s\n", it.Number, termsafe.SafeLineMax(it.Reason, 300)); err != nil {
			return err
		}
	}
	if r.Stopped != "" {
		if _, err := fmt.Fprintf(out, "stopped: %s\n", termsafe.SafeLineMax(r.Stopped, 300)); err != nil {
			return err
		}
	}
	if len(r.Enqueued) == 0 && len(r.Skipped) == 0 && r.Stopped == "" {
		_, err := fmt.Fprintln(out, "no open issues carry an eligible label")
		return err
	}
	return nil
}
