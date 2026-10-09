package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface enqueue`, `dequeue` and `queue` manage the machine's queue of
// briefs that `surface drain` launches as workers.

// queueRowView is one queue row as `surface queue --json` and `surface
// enqueue --json` print it. The brief text is never printed. Additive changes
// only (ADR-0008 rule 2).
type queueRowView struct {
	Name        string    `json:"name"`
	Repo        string    `json:"repo"`
	Batch       string    `json:"batch,omitempty"`
	State       string    `json:"state"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
	LaunchID    string    `json:"launch_id,omitempty"`
	Session     string    `json:"session,omitempty"`
	Profile     string    `json:"profile,omitempty"`
	Model       string    `json:"model,omitempty"`
	Harness     string    `json:"harness"`
	Source      string    `json:"source,omitempty"`
	Author      string    `json:"author,omitempty"`
	Labeler     string    `json:"labeler,omitempty"`
	LabeledAt   string    `json:"labeled_at,omitempty"`
	BriefSHA256 string    `json:"brief_sha256"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
	StateAt     time.Time `json:"state_at"`
	// AgeSeconds is how long the row has been in its state.
	AgeSeconds int64 `json:"age_seconds"`
}

func viewQueueRow(r worker.QueueRow, now time.Time) queueRowView {
	return queueRowView{
		Name: r.Name, Repo: r.Repo, Batch: r.Batch, State: string(r.State), Attempts: r.Attempts,
		LastError: r.LastError, LaunchID: r.LaunchID, Session: r.Session, Profile: r.Profile, Model: r.Model, Harness: r.Launch().Harness,
		Source: r.Source, Author: r.Author, Labeler: r.Labeler, LabeledAt: r.LabeledAt, BriefSHA256: r.BriefSHA256, EnqueuedAt: r.EnqueuedAt, StateAt: r.StateAt, AgeSeconds: int64(max(now.Sub(r.StateAt), 0) / time.Second),
	}
}

// enqueueResult is what `surface enqueue --json` prints: the row, and whether
// this call added it.
type enqueueResult struct {
	Added bool `json:"added"`
	queueRowView
}

// queueResult is what `surface queue --json` prints.
type queueResult struct {
	Rows []queueRowView `json:"rows"`
}

type enqueueOptions struct {
	Repo    string
	Name    string
	Brief   string
	Batch   string
	Profile string
	Model   string
	Harness string
	JSON    bool
}

func newSurfaceEnqueueCmd(deps module.Deps) *cobra.Command {
	opts := enqueueOptions{}
	cmd := &cobra.Command{
		Use:   "enqueue --repo <path> --name <slug> --brief <file>",
		Short: "Queue a brief for surface drain to launch as a worker",
		Long: `enqueue adds a row to the machine's queue, $XDG_STATE_HOME/forgectl/surface/queue.json,
for surface drain to launch as a worker on its own branch. --harness names the
worker's harness: claude (the default), codex, or pi. The drain launches the
row with it.

--repo is a project name or path; the row records the repository's top level.
--name is the worker name, unique across the whole queue (1-48 characters of
a-z, 0-9 and '-'). --brief is a path to the brief file (no '@'), read once and
stored as text: at most 64 KiB, checked as a launch brief, and it may not
start with '@'. --batch tags the row for listing. --profile names a
[surface.profiles] entry whose config_dir the worker runs under as
CLAUDE_CONFIG_DIR (main, or none, keeps the drain's own); the row stores the
name, and the drain resolves it from the config file when it launches the
row. --profile is for claude workers only. --model replaces the launch
profile's model for this worker, passed as the harness's --model (a plain
token: letters, digits, '.', '-', '_', '[', ']', not starting with '-').

enqueue is idempotent on the name: the same brief again changes nothing and
prints the row's current state; a different brief, or another repository, is
refused naming both brief hashes, as is another --harness, --profile or --model. To replace a row, dequeue it first. The
queue file stays under 768 KiB; an enqueue past that is refused before
anything is written. --json prints {"added","name","repo","batch","state",
"attempts","last_error","launch_id","session","profile","model","harness",
"source","author","labeler","labeled_at","brief_sha256","enqueued_at","state_at","age_seconds"}.

Exit 0: queued, or the name already holds this brief, in any state (read
"state": exit 0 does not mean the row is still queued). Exit 1: refused (the name
holds another brief, repository, launch setting, or intake origin (source,
author, labeler, labeled_at),
or the queue is full). Exit 2: a usage or
setup error, such as an unusable brief or an unreadable queue file.

  forgectl surface enqueue --repo forgectl --name fix-login --brief brief.md
  forgectl surface enqueue --repo . --name docs-pass --brief b.md --batch oct-07 --json
  forgectl surface enqueue --repo forgectl --name tidy --brief b.md --profile work --model sonnet
  forgectl surface enqueue --repo forgectl --name lint-pass --brief b.md --harness codex`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceEnqueue(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", "", "repository the worker runs in (project name or path) — required")
	cmd.Flags().StringVar(&opts.Name, "name", "", "worker name, unique across the queue — required")
	cmd.Flags().StringVar(&opts.Brief, "brief", "", "path to the brief file, at most 64 KiB — required")
	cmd.Flags().StringVar(&opts.Batch, "batch", "", "batch id to tag the row with (a-z, 0-9 and '-')")
	cmd.Flags().StringVar(&opts.Profile, "profile", "", "[surface.profiles] name the worker runs under (main: the drain's own CLAUDE_CONFIG_DIR)")
	cmd.Flags().StringVar(&opts.Model, "model", "", "model the worker runs on, instead of the launch profile's")
	cmd.Flags().StringVar(&opts.Harness, "harness", worker.DefaultQueueHarness, "harness the drain launches the worker with: claude, codex, or pi")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"added","name","repo","batch","state","attempts","last_error","launch_id","session","profile","model","harness","brief_sha256","enqueued_at","state_at","age_seconds"} as JSON`)
	return cmd
}

func runSurfaceEnqueue(cmd *cobra.Command, deps module.Deps, opts enqueueOptions) error {
	if err := refuseInDrainWorker(os.Getenv, "surface enqueue"); err != nil {
		return err
	}
	if opts.Repo == "" || opts.Name == "" || opts.Brief == "" {
		return WithExitCode(errors.New("enqueue needs --repo, --name and --brief"), exitUsage)
	}
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	if strings.HasPrefix(opts.Brief, "@") {
		return WithExitCode(errors.New("--brief takes a path to the brief file; drop the leading '@'"), exitUsage)
	}
	if err := checkQueueLaunchFlags(deps, opts.Profile, opts.Model, opts.Harness); err != nil {
		return err
	}
	target, err := projects.New(deps.Runner).ResolveTarget(opts.Repo)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	top, err := worker.RepoTop(cmd.Context(), deps.Runner, target)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	text, err := readBriefArg("@" + opts.Brief)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	if err := worker.CheckQueueBrief(text); err != nil {
		return WithExitCode(err, exitUsage)
	}
	q, err := worker.OpenQueue()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	now := time.Now()
	row, added, err := q.EnqueueLaunch(opts.Name, top, text, opts.Batch, worker.QueueLaunch{Profile: opts.Profile, Model: opts.Model, Harness: opts.Harness}, now)
	switch {
	case errors.Is(err, worker.ErrQueueNameTaken), errors.Is(err, worker.ErrQueueFull):
		return termsafe.Error(err)
	case err != nil:
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	return reportEnqueue(cmd.OutOrStdout(), enqueueResult{Added: added, queueRowView: viewQueueRow(row, now)}, opts.JSON)
}

// checkQueueLaunchFlags refuses a --model, --harness or --profile a queue row
// cannot carry, as a usage error.
func checkQueueLaunchFlags(deps module.Deps, profile, model, harness string) error {
	if model != "" {
		if err := config.CheckModelName(model); err != nil {
			return WithExitCode(fmt.Errorf("--model: %w", err), exitUsage)
		}
	}
	switch harness {
	case "claude":
	case "codex", "pi":
		if profile != "" && profile != config.MainProfile {
			return WithExitCode(errProfileNotClaude, exitUsage)
		}
	default:
		return WithExitCode(fmt.Errorf("--harness %s: want claude, codex, or pi", termsafe.QuoteArgMax(harness, 0)), exitUsage)
	}
	// Only the name is stored; resolving it now refuses a name the config
	// does not define, and the drain resolves it again at launch.
	if _, err := deps.Cfg.Surface.ProfileConfigDir(profile, os.UserHomeDir); err != nil {
		return WithExitCode(termsafe.Error(fmt.Errorf("--profile: %w", err)), exitUsage)
	}
	return nil
}

func reportEnqueue(out io.Writer, r enqueueResult, asJSON bool) error {
	if asJSON {
		return writeJSON(out, r)
	}
	name := termsafe.SafeLineMax(r.Name, 64)
	if r.Added {
		_, err := fmt.Fprintf(out, "queued %s (brief sha256 %s)\n", name, r.BriefSHA256)
		return err
	}
	_, err := fmt.Fprintf(out, "%s is already queued with this brief: %s\n", name, termsafe.SafeLineMax(r.State, 16))
	return err
}

func newSurfaceDequeueCmd(_ module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "dequeue <name>",
		Short: "Remove a queue row whose worker is not live",
		Long: `dequeue removes the named row from the queue. It refuses a row whose worker
may be running (claimed, launched, needs-you): run surface close first. After
dequeue the same name can be enqueued again, which is how a failed row is
retried. --json prints the removed row as it was: {"name","repo","batch",
"state","attempts","last_error","launch_id","session","profile","model","harness",
"source","author","labeler","labeled_at","brief_sha256","enqueued_at","state_at","age_seconds"}.

Exit 0: removed. Exit 1: refused, the worker is live. Exit 2: no such row,
or a usage or setup error.

  forgectl surface dequeue fix-login`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSurfaceDequeue(cmd, args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print the removed row as {"name","repo","batch","state","attempts","last_error","launch_id","session","profile","model","harness","source","author","labeler","labeled_at","brief_sha256","enqueued_at","state_at","age_seconds"} JSON`)
	return cmd
}

func runSurfaceDequeue(cmd *cobra.Command, name string, asJSON bool) error {
	if err := worker.ValidName(name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	q, err := worker.OpenQueue()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	removed, err := q.Dequeue(name)
	switch {
	case errors.Is(err, worker.ErrQueueRowLive):
		return termsafe.Error(err)
	case errors.Is(err, worker.ErrQueueNoRow):
		return WithExitCode(fmt.Errorf("no queue row named %q", name), exitUsage)
	case err != nil:
		return WithExitCode(termsafe.Error(err), exitUsage)
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), viewQueueRow(removed, time.Now()))
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "dequeued %s (was %s)\n", termsafe.SafeLineMax(removed.Name, 64), termsafe.SafeLineMax(string(removed.State), 16))
	return err
}

func newSurfaceQueueCmd(_ module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "List the queue's rows and how long each has been in its state",
		Long: `queue lists every row in the machine's queue: its state, how long it has
been in that state, its attempts, batch, repository, and last error. The
brief text is never printed. States: queued, claimed, launched, needs-you,
reported, failed, closed, expired. A dequeued row is removed, not kept.

--json prints {"rows":[{"name","repo","batch","state","attempts","last_error",
"launch_id","session","profile","model","harness","source","author",
"labeler","labeled_at","brief_sha256","enqueued_at","state_at","age_seconds"}]}.
source, author, labeler and labeled_at are set on rows surface intake made:
gh:<owner>/<repo>#<number>, the issue's author, and the login and RFC 3339
time of the labeling that admitted it.

Exit 0: listed. Exit 2: the queue file cannot be read.

  forgectl surface queue
  forgectl surface queue --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q, err := worker.OpenQueue()
			if err != nil {
				return WithExitCode(err, exitUsage)
			}
			rows, err := q.Rows()
			if err != nil {
				return WithExitCode(termsafe.Error(err), exitUsage)
			}
			return reportQueue(cmd.OutOrStdout(), rows, time.Now(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"rows":[{"name","repo","batch","state","attempts","last_error","launch_id","session","profile","model","harness","source","author","labeler","labeled_at","brief_sha256","enqueued_at","state_at","age_seconds"}]} as JSON`)
	return cmd
}

func reportQueue(out io.Writer, rows []worker.QueueRow, now time.Time, asJSON bool) error {
	res := queueResult{Rows: make([]queueRowView, 0, len(rows))}
	for _, r := range rows {
		res.Rows = append(res.Rows, viewQueueRow(r, now))
	}
	if asJSON {
		return writeJSON(out, res)
	}
	if len(res.Rows) == 0 {
		_, err := fmt.Fprintln(out, "queue is empty")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tSTATE\tAGE\tATTEMPTS\tBATCH\tREPO\tLAST ERROR"); err != nil {
		return err
	}
	for _, r := range res.Rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			termsafe.SafeLineMax(r.Name, 64), termsafe.SafeLineMax(r.State, 16),
			ageText(time.Duration(r.AgeSeconds)*time.Second), r.Attempts,
			termsafe.SafeLineMax(r.Batch, 64), safeColumnPath(r.Repo),
			termsafe.SafeLineMax(r.LastError, 200)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// ageText renders a duration in the largest whole units that fit: 45s, 12m,
// 3h20m, 2d.
func ageText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
