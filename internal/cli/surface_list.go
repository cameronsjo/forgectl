package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface list` shows the workers in a repo's ledger for this herdr session,
// each checked against herdr and git.

// listTimeout bounds the whole list: one herdr probe per worker, and git.
const listTimeout = 60 * time.Second

// Worker states. A herdr error is unreadable, never gone: only a complete
// listing on the server the reference was taken against can prove a
// workspace gone.
const (
	workerPresent    = "present"
	workerGone       = "gone"
	workerUnreadable = "unreadable"
)

// listResult is what `surface list --json` prints. Additive changes only
// (ADR-0008 rule 2).
type listResult struct {
	Repo    string    `json:"repo"`
	Session string    `json:"session"`
	Workers []listRow `json:"workers"`
}

// listRow is one worker. PR state is not here: it needs GitHub, and list is
// polled; `surface status` (T10) carries it.
type listRow struct {
	Name    string       `json:"name"`
	Harness string       `json:"harness,omitempty"`
	Repo    string       `json:"repo"`
	Branch  string       `json:"branch"`
	Stage   worker.Stage `json:"stage"`
	// State is the workspace's state: present, gone, or unreadable.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Orphan marks a worker whose workspace is gone or whose launch never
	// finished: a row the next coordinator should close.
	Orphan bool `json:"orphan"`
	// Worktree is where the worker's worktree lives, and WorktreePresent
	// whether git lists it there now.
	Worktree        string    `json:"worktree"`
	WorktreePresent bool      `json:"worktree_present"`
	WorkspaceID     string    `json:"workspace_id,omitempty"`
	PaneID          string    `json:"pane_id,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	Transcript      string    `json:"transcript,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	Marker          string    `json:"marker,omitempty"`
	Failure         string    `json:"failure,omitempty"`
	Recovery        string    `json:"recovery,omitempty"`
}

type listOptions struct {
	Repo    string
	Orphans bool
	JSON    bool
}

func newSurfaceListCmd(deps module.Deps) *cobra.Command {
	opts := listOptions{}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a repo's workers, checked against herdr and git",
		Long: `list shows every worker in the repo's ledger for the current herdr session,
each checked against herdr: present (its workspace is there and carries
forgectl's ownership marker), gone, or unreadable. A herdr error is
unreadable, never gone, and close refuses an unreadable worker.

--orphans shows only the rows a coordinator should close: those whose
workspace is gone, and those at any stage other than launched (failed,
closed, or a launch that stopped at pending or worktree more than ten
minutes ago). A younger launch may still be running and is never listed.

Exit 0: listed (unreadable workers included). Exit 2: a usage or setup
error, such as a ledger that cannot be read.

  forgectl surface list
  forgectl surface list --orphans --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSurfaceList(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the workers were launched from (project name or path)")
	cmd.Flags().BoolVar(&opts.Orphans, "orphans", false, "show only workers whose workspace is gone or whose launch never finished")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"repo","session","workers":[{"name","harness","repo","branch","stage","state","reason","orphan","worktree","worktree_present","workspace_id","pane_id","session_id","transcript","started_at","marker","failure","recovery"}]} as JSON`)
	return cmd
}

// workerLedgerContext is a repo's ledger and the adapter for its session.
type workerLedgerContext struct {
	herdr *herdradapter.Adapter
	led   *worker.Ledger
	top   string
}

// openWorkerLedger resolves repo and opens its ledger for the current herdr
// session. Every failure is a usage or setup error (exit 2). warn takes the
// herdr adapter's setup warnings.
func openWorkerLedger(ctx context.Context, warn io.Writer, deps module.Deps, repo string) (*workerLedgerContext, error) {
	adapter, err := newHerdrAdapter(warn)
	if err != nil {
		return nil, WithExitCode(err, exitUsage)
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return nil, errors.New("forgectl: the herdr adapter has an unexpected type")
	}
	target, err := projects.New(deps.Runner).ResolveTarget(repo)
	if err != nil {
		return nil, WithExitCode(err, exitUsage)
	}
	top, err := worker.RepoTop(ctx, deps.Runner, target)
	if err != nil {
		return nil, WithExitCode(err, exitUsage)
	}
	led, err := worker.Open(top, herdr.Session())
	if err != nil {
		return nil, WithExitCode(err, exitUsage)
	}
	return &workerLedgerContext{herdr: herdr, led: led, top: top}, nil
}

func runSurfaceList(cmd *cobra.Command, deps module.Deps, opts listOptions) error {
	w, err := openWorkerLedger(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo)
	if err != nil {
		return err
	}
	rows, err := w.led.Rows()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), listTimeout)
	defer cancel()
	listed, err := worker.ListedWorktrees(ctx, deps.Runner, w.top)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	now := time.Now()
	res := listResult{Repo: w.top, Session: w.herdr.Session(), Workers: []listRow{}}
	for _, r := range rows {
		row := reconcileRow(ctx, w.herdr, r, w.top, listed, now)
		if opts.Orphans && !row.Orphan {
			continue
		}
		res.Workers = append(res.Workers, row)
	}
	return reportList(cmd, res, opts.JSON)
}

// reconcileRow checks one ledger row against herdr and git.
//
// debt: one Probe per row, each re-reading herdr's readiness and full
// workspace listing; upgrade to one listing per call when the atelier pane
// (P6) polls list.
func reconcileRow(ctx context.Context, probe backend.Prober, r worker.Row, top string, listed map[string]bool, now time.Time) listRow {
	path := worker.WorktreePath(top, r.Name)
	row := listRow{
		Name: r.Name, Harness: r.Harness, Repo: top, Branch: r.Branch, Stage: r.Stage,
		Worktree: path, WorktreePresent: listed[path],
		SessionID: r.SessionID, Transcript: r.Transcript, StartedAt: r.StartedAt,
		Failure: r.Failure, Recovery: r.Recovery,
	}
	if r.Brief != nil {
		row.Marker = r.Brief.Marker
	}
	row.State, row.Reason = probeWorkspace(ctx, probe, r, &row)
	// A launch still inside its settle window is not an orphan yet: close
	// would refuse it.
	row.Orphan = !launchInFlight(r, now) && (row.State == workerGone || r.Stage != worker.StageLaunched)
	return row
}

// probeWorkspace probes the row's workspace. A row with no reference never
// started one. An identity mismatch is unreadable: a restarted server or a
// workspace that does not carry our marker cannot prove ours is gone.
func probeWorkspace(ctx context.Context, probe backend.Prober, r worker.Row, row *listRow) (string, string) {
	if len(r.Ref) == 0 {
		if r.Recovery != "" {
			return workerGone, "no workspace was recorded; a failed launch may have left one labeled " + r.Recovery
		}
		return workerGone, "no workspace was recorded"
	}
	ref, err := backend.DecodeRef(r.Ref)
	if err != nil {
		return workerUnreadable, "the ledger reference does not decode"
	}
	if id, err := ref.HerdrIdentity(); err == nil {
		row.WorkspaceID, row.PaneID = id.Workspace(), id.Pane()
	}
	res := probe.Probe(ctx, ref)
	switch res.State() {
	case backend.ProbePresent:
		return workerPresent, ""
	case backend.ProbeGone:
		return workerGone, "the herdr workspace is gone"
	case backend.ProbeIdentityMismatch:
		return workerUnreadable, "herdr restarted, or the workspace id no longer carries forgectl's marker (" + causeText(res) + ")"
	default:
		return workerUnreadable, "herdr could not be read (" + causeText(res) + ")"
	}
}

func causeText(r interface {
	Cause() (backend.StartCause, bool)
}) string {
	if c, ok := r.Cause(); ok {
		return c.Error()
	}
	return "no cause recorded"
}

func reportList(cmd *cobra.Command, r listResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		return writeJSON(out, r)
	}
	if len(r.Workers) == 0 {
		_, err := fmt.Fprintln(out, "no workers")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tSTATE\tSTAGE\tHARNESS\tBRANCH\tWORKTREE\tNOTE"); err != nil {
		return err
	}
	for _, w := range r.Workers {
		wt := "present"
		if !w.WorktreePresent {
			wt = "none"
		}
		note := w.Reason
		if w.Orphan {
			note = strings.TrimSpace("orphan " + note)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			termsafe.SafeLineMax(w.Name, 64), w.State, termsafe.SafeLineMax(string(w.Stage), 16),
			termsafe.SafeLineMax(w.Harness, 16), termsafe.SafeLineMax(w.Branch, 80), wt,
			termsafe.SafeLineMax(note, 200)); err != nil {
			return err
		}
	}
	return tw.Flush()
}
