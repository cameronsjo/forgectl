package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/launch"
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
	Labeler     string `json:"labeler,omitempty"`
	LabeledAt   string `json:"labeled_at,omitempty"`
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

// intakeConfirmer shows a person the candidates a run would queue and
// returns nil only when that person typed "yes" at a terminal. stdin is the
// command's input, which must itself be a terminal.
type intakeConfirmer func(stdin io.Reader, candidates []intakeCandidate) error

// intakeDeps is what `surface intake gh` runs with: the module's deps, the
// confirmer, and the environment lookup the drain-worker refusal reads.
// newSurfaceIntakeGHCmd fills confirm with confirmIntakeAtTerminal and
// getenv with os.Getenv; tests build the command with their own. No flag or
// variable replaces the confirmer, so production has no path that skips it.
type intakeDeps struct {
	module.Deps
	confirm intakeConfirmer
	getenv  func(string) string
}

// intakeCandidate is one issue a run would queue, as the confirmation shows
// it. Title is the issue's raw title; the confirmer sanitizes it.
type intakeCandidate struct {
	Number      int
	Title       string
	Author      string
	Labeler     string
	LabeledAt   string
	Name        string
	Source      string
	URL         string
	Body        string
	BriefSHA256 string
}

func newSurfaceIntakeGHCmd(deps module.Deps) *cobra.Command {
	return newSurfaceIntakeGHCmdWith(intakeDeps{Deps: deps, confirm: confirmIntakeAtTerminal, getenv: os.Getenv})
}

func newSurfaceIntakeGHCmdWith(deps intakeDeps) *cobra.Command {
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
source gh:<owner>/<repo>#<number>, the issue's author, and the labeling that
admitted it: labeler (the login that applied the eligible label) and
labeled_at (when, as RFC 3339). An existing row of that name, in any state,
skips the issue (a failed row: dequeue it to retry). Each skip names the
issue and its reason, and the run goes on. [surface.intake] labels (default
queue:drain, a label no triage sweep applies) are the eligible labels;
--label narrows to one of them. [surface.intake] max_per_run (default 5)
caps the rows one run adds. --harness, --model and --profile apply to every
row, as on enqueue.

A person confirms every run that would queue something. A worker runs with
the operator's gh login, so the author and labeler checks cannot tell the
operator's labeling from a worker's. intake first shows each candidate
(number, title, author, labeler, labeled-at time, row name, brief sha256)
on the terminal, then queues them only if "yes" is typed there. The answer
is read from /dev/tty, never from stdin, and stdin must be a terminal as
well, so a piped "yes" does not count. No terminal, end of input, or any
other answer queues nothing. No flag skips this. A run with nothing to
queue asks nothing. intake refuses to run at all, before reading GitHub,
when FORGECTL_DRAIN_WORKER is set, as it is in every drain worker.

--dry-run reads GitHub and the queue, asks nothing, and writes nothing.
--json prints {"dry_run","repo","github","authors","labels","enqueued":
[{"number","name","source","author","labeler","labeled_at","brief_sha256"}],
"skipped":[{"number","name","source","author","labeler","labeled_at",
"reason"}],"stopped"} on stdout; with --dry-run, "enqueued" lists what
would be queued. The candidate list and the prompt go to the terminal, never
to stdout, with or without --json.

Exit 0: the run finished, whatever it skipped. Exit 1: the answer was not
"yes", the queue filled, or GitHub could not be read. A GitHub read failure
queues nothing; a full queue keeps the rows queued before it. Exit 2: a
usage or setup error (no terminal to confirm at, run inside a drain worker,
an invalid [surface.intake], an origin not on github.com, no authors set
for a repository the gh account does not own).

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
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "list what would be queued and skipped; ask nothing, write nothing")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"dry_run","repo","github","authors","labels","enqueued","skipped","stopped"} as JSON on stdout (the confirmation stays on the terminal)`)
	return cmd
}

// errIntakeInWorker refuses intake inside a drain worker. The marker is a
// courtesy refusal, not a boundary: a worker runs as the operator and can
// unset FORGECTL_DRAIN_WORKER. It stops a worker starting more work by
// accident; nothing on this machine stops one that sets out to.
var errIntakeInWorker = errors.New("intake refuses to run inside a drain worker (" + launch.DrainWorkerEnv +
	" is set): a worker must not queue work for another worker; run intake from your own terminal")

// refuseInDrainWorker refuses verb inside a drain worker, the same courtesy
// refusal intake makes, for the other commands that start a worker (surface
// enqueue and surface launch), drive one (surface brief, the drain's start,
// stop and process), close one or remove its queue row (surface close and
// surface dequeue) or merge a PR (surface merge).
func refuseInDrainWorker(getenv func(string) string, verb string) error {
	if getenv(launch.DrainWorkerEnv) == "" {
		return nil
	}
	return WithExitCode(fmt.Errorf("%s refuses to run inside a drain worker (%s is set): a worker must not start or drive another worker or merge a PR; run it from your own terminal", verb, launch.DrainWorkerEnv), exitUsage)
}

func runSurfaceIntakeGH(cmd *cobra.Command, d intakeDeps, opts intakeOptions) error {
	getenv := d.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// First of all, so a worker that runs intake reads nothing from GitHub.
	// Any non-empty value counts.
	if getenv(launch.DrainWorkerEnv) != "" {
		return WithExitCode(errIntakeInWorker, exitUsage)
	}
	deps := d.Deps
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
		confirm: func(c []intakeCandidate) error {
			if d.confirm == nil {
				return WithExitCode(errIntakeNoTerminal, exitUsage)
			}
			return d.confirm(cmd.InOrStdin(), c)
		},
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
	// confirm asks a person to approve the candidates; nil error means they
	// typed "yes". A real run calls it once, before any queue write.
	confirm func([]intakeCandidate) error
}

// errIntakeStopped marks a run that ended early on something other than
// max_per_run or the page limit; the command exits 1 on it.
var errIntakeStopped = errors.New("intake stopped")

// run reads GitHub page by page and gathers each issue the gate takes, up to
// max_per_run. A dry run reports them; a real run shows them to a person and
// queues them only when that person confirms. It returns the result so far
// with every error, so the caller can print what was queued before the run
// stopped. Nothing is queued before the confirmation, so a GitHub read
// failure queues nothing.
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
	var picked []considered
	after := ""
	for page := 0; ; page++ {
		if page == intakeMaxPages {
			res.Stopped = fmt.Sprintf("read %d pages of issues, the most one run reads; run intake again for the rest", intakeMaxPages)
			return in.finish(res, picked)
		}
		p, err := in.fetch(after)
		if err != nil {
			res.Stopped = "GitHub could not be read, so nothing was queued: " + err.Error()
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
			item, take := in.consider(is, rules, rows)
			if !take {
				res.Skipped = append(res.Skipped, item.view)
				continue
			}
			if len(picked) >= in.max {
				res.Stopped = fmt.Sprintf("max_per_run (%d) reached; run intake again for the rest", in.max)
				return in.finish(res, picked)
			}
			picked = append(picked, item)
		}
		if !p.HasNextPage {
			return in.finish(res, picked)
		}
		after = p.EndCursor
	}
}

// errIntakeNoTerminal refuses a real run nobody can confirm.
var errIntakeNoTerminal = errors.New("intake needs a person at a terminal to confirm; run it from a plain terminal, or use --dry-run")

// errIntakeNotConfirmed reports an answer other than "yes".
var errIntakeNotConfirmed = errors.New(`intake: the answer was not "yes"; nothing was queued`)

// finish ends a run with the issues it gathered. A dry run, or a run that
// gathered nothing, only reports; a real run asks for confirmation and then
// queues each one.
//
// The confirmation is the gate (ADR-0010, 2026-10-09 amendment). A worker
// runs with the operator's gh login, so it could file and label an issue
// that passes every check in intake.Admit; a person reading the candidates
// at a terminal is what tells the operator's request from a worker's.
func (in intakeRun) finish(res intakeResult, picked []considered) (intakeResult, error) {
	if in.dryRun || len(picked) == 0 {
		for _, c := range picked {
			res.Enqueued = append(res.Enqueued, c.view)
		}
		return res, nil
	}
	cands := make([]intakeCandidate, 0, len(picked))
	for _, c := range picked {
		cands = append(cands, intakeCandidate{
			Number: c.view.Number, Title: c.title, Author: c.view.Author, Labeler: c.view.Labeler,
			LabeledAt: c.view.LabeledAt, Name: c.view.Name, Source: c.view.Source, BriefSHA256: c.view.BriefSHA256,
			URL: fmt.Sprintf("https://github.com/%s/%s/issues/%d", in.owner, in.repo, c.view.Number), Body: c.body,
		})
	}
	if err := in.confirm(cands); err != nil {
		for _, c := range picked {
			c.view.Reason = "not confirmed at a terminal"
			c.view.BriefSHA256 = ""
			res.Skipped = append(res.Skipped, c.view)
		}
		res.Stopped = "not confirmed; nothing was queued"
		return res, err
	}
	for _, item := range picked {
		row, added, err := in.q.EnqueueFrom(item.view.Name, in.top, item.brief, "", in.launch, item.origin(), in.now())
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
	return res, nil
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
// report and, when the gate took it, the brief and the issue's raw title,
// which the confirmation shows.
type considered struct {
	view  intakeItem
	brief string
	title string
	body  string
}

// origin is the row origin the item records.
func (c considered) origin() worker.QueueOrigin {
	return worker.QueueOrigin{Source: c.view.Source, Author: c.view.Author, Labeler: c.view.Labeler, LabeledAt: c.view.LabeledAt}
}

// consider decides one issue: skip, with a reason, or take, with its brief.
// rows is the queue as read when the run began; a row written since is
// caught by the enqueue itself.
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
	adm, err := intake.Admit(is, rules)
	if err != nil {
		return skip(err.Error())
	}
	c.view.Labeler, c.view.LabeledAt = adm.Labeler, adm.LabeledAt.UTC().Format(time.RFC3339)
	brief, err := intake.Brief(in.owner+"/"+in.repo, is, in.nonce)
	if err != nil {
		return skip(err.Error())
	}
	if err := worker.CheckQueueBrief(brief); err != nil {
		return skip(err.Error())
	}
	if err := c.origin().Check(); err != nil {
		return skip(err.Error())
	}
	c.view.BriefSHA256 = worker.BriefSHA256(brief)
	c.brief = brief
	c.title = is.Title
	c.body = is.Body
	return c, true
}

// confirmIntakeAtTerminal is the production confirmer: stdin must be a
// terminal, /dev/tty must open as one, and the answer is read from /dev/tty
// (terminalConfirm).
func confirmIntakeAtTerminal(stdin io.Reader, cands []intakeCandidate) error {
	return productionTerminal().confirm(stdin, errIntakeNoTerminal, func(in io.Reader, out io.Writer) error {
		return askIntake(in, out, cands)
	})
}

// askIntake writes the candidates and the question to out and reads one line
// from in. Only a complete line that is exactly "yes", once surrounding
// space is trimmed, confirms; end of input before a newline does not.
func askIntake(in io.Reader, out io.Writer, cands []intakeCandidate) error {
	if err := writeIntakeCandidates(out, cands); err != nil {
		return err
	}
	if _, err := fmt.Fprint(out, `Type "yes" to queue them, anything else to cancel: `); err != nil {
		return err
	}
	if !readYes(in) {
		return WithExitCode(errIntakeNotConfirmed, exitFailed)
	}
	return nil
}

// writeIntakeCandidates lists what a run would queue, one issue per block.
// Title and body are issue text, so each is cut to one sanitized line. The
// body excerpt and URL are there because author and labeler always read as
// the operator: a worker-filed issue with a plain title is only caught by
// reading what it asks for (cameronsjo/forgectl#1205).
func writeIntakeCandidates(out io.Writer, cands []intakeCandidate) error {
	if _, err := fmt.Fprintf(out, "intake would queue %d issue(s), each for an unattended worker that runs as you:\n", len(cands)); err != nil {
		return err
	}
	for _, c := range cands {
		if _, err := fmt.Fprintf(out, "  %s %s\n      url %s\n      body (%d chars): %s\n      author %s, labeled by %s at %s\n      row %s, brief sha256 %s\n",
			termsafe.SafeLineMax(c.Source, 160), termsafe.SafeLineMax(escapeInvisible(c.Title), 100), termsafe.SafeLineMax(c.URL, 200), utf8.RuneCountInString(c.Body), intakeBodyExcerpt(c.Body),
			termsafe.SafeLineMax(c.Author, 40), termsafe.SafeLineMax(c.Labeler, 40),
			termsafe.SafeLineMax(c.LabeledAt, 40), termsafe.SafeLineMax(c.Name, 64), termsafe.SafeLineMax(c.BriefSHA256, 64)); err != nil {
			return err
		}
	}
	return nil
}

// intakeBodyExcerptRunes caps the body excerpt the intake prompt shows.
const intakeBodyExcerptRunes = 240

// intakeBodyExcerpt is the start of an issue body on one sanitized line:
// runs of whitespace, newlines included, collapse to one space.
func intakeBodyExcerpt(body string) string {
	flat := strings.Join(strings.Fields(body), " ")
	if flat == "" {
		return "(empty)"
	}
	return termsafe.SafeLineMax(escapeInvisible(flat), intakeBodyExcerptRunes)
}

// escapeInvisible writes the invisible runes of issue text the intake prompt
// shows next to labeled fields as \uXXXX. SafeLine shows invisible runes (U+2800, the Hangul
// fillers) as themselves, so padding made of them wraps a body onto what
// looks like a fresh "url" or "author" line; they are escaped here
// first (cameronsjo/forgectl#1215). Call it inside a capped SafeLineMax. Only
// the title and body are free text: the other fields are GitHub logins,
// owner/repo names and numbers, which GitHub restricts to ASCII.
func escapeInvisible(s string) string {
	if strings.IndexFunc(s, termsafe.IsInvisibleRune) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if termsafe.IsInvisibleRune(r) {
			if r > 0xffff {
				fmt.Fprintf(&b, "\\U%08X", r)
			} else {
				fmt.Fprintf(&b, "\\u%04X", r)
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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
