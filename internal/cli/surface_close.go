package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface close <name>` closes a worker's herdr workspace, then removes its
// worktree only when that provably loses no work. It never deletes the branch.

const (
	closeTimeout = 60 * time.Second
	// removeTimeout bounds `git worktree remove` on its own clock, so the
	// deadline of the steps before it cannot kill git partway through a
	// deletion and leave a half-removed worktree.
	removeTimeout = 2 * time.Minute
	// launchSettleAfter is how long a launch may sit at stage pending or
	// worktree before list calls it an orphan and close acts on it. A launch
	// moves past both in seconds; a younger row may be one still running.
	launchSettleAfter = 10 * time.Minute
)

// What close did to the workspace.
const (
	closeWorkspaceClosed   = "closed"
	closeWorkspaceGone     = "already-gone"
	closeWorkspaceEarlier  = "closed-earlier"
	closeWorkspaceNone     = "none"
	closeWorkspaceRefused  = "refused"
	closeWorktreeRemoved   = "removed"
	closeWorktreeKept      = "kept"
	closeWorktreeGone      = "gone"
	closeWorktreeUntouched = "untouched"

	// Preview values (--dry-run): what close would do, not what it did.
	closeWorkspaceWouldClose = "would-close"
	closeWorktreeWouldRemove = "would-remove"
	closeWorktreeWouldKeep   = "would-keep"
)

// closeResult is what `surface close --json` prints. Additive changes only
// (ADR-0008 rule 2).
type closeResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// Closed is false only when close refused: the workspace could not be
	// proven closed or gone, so nothing was touched.
	Closed    bool   `json:"closed"`
	Workspace string `json:"workspace"`
	Worktree  string `json:"worktree"`
	// KeptBecause names every removal check that failed, or --keep-worktree.
	KeptBecause []string `json:"kept_because,omitempty"`
	// Forgotten reports that the ledger row was removed: the workspace is
	// closed and no worktree remains.
	Forgotten bool   `json:"forgotten"`
	Reason    string `json:"reason,omitempty"`
	Note      string `json:"note,omitempty"`
}

type closeOptions struct {
	Repo         string
	Name         string
	KeepWorktree bool
	JSON         bool
	DryRun       bool
}

func newSurfaceCloseCmd(deps module.Deps) *cobra.Command {
	opts := closeOptions{}
	cmd := &cobra.Command{
		Use:   "close <name>",
		Short: "Close a worker's workspace; remove its worktree only if no work is lost",
		Long: `close closes the named worker's herdr workspace, then removes its worktree
when all of these hold, and keeps it otherwise, naming each check that failed:

  - git status --porcelain --ignored is empty (ignored files count as work)
  - HEAD is on a branch
  - the branch has no commits missing from its upstream, or, with no
    upstream, none beyond the commit the worktree started at
  - no stash entry was made on the branch

The branch is never deleted. The worktree path comes from git worktree list,
never from the ledger. close works on a worker at any stage, so it cleans up
after a launch that died partway. It refuses, touching nothing, when herdr
cannot be read, when the workspace cannot be proven to be forgectl's, or
when the row is a launch at pending or worktree less than ten minutes old,
which may still be running. A row at stage closed skips herdr.

When the workspace is closed and no worktree remains, the ledger row is
removed. A kept worktree keeps the row at stage "closed"; run close again
once the work is saved.

--dry-run runs the checks that need no write (the launch-in-flight refusal, the
ledger stage, and the read-only worktree inspection) and prints what close
would do, changing nothing: herdr is not asked, so it cannot say whether herdr
would refuse, and a workspace that would be closed reads "would-close". It
exits 1 where close would refuse, so close --dry-run && close stops where
close would. git status runs without optional locks, so the preview takes no
index.lock; a worktree in the middle of a rebase or bisect can list as a
detached HEAD, which keeps it (would-keep). With --json it prints {"name","branch","dry_run","refused",
"workspace","worktree","kept_because","would_forget","reason","note"}.

Exit 0: the workspace is closed or gone (the worktree may be kept). Exit 1:
refused. Exit 2: a usage or setup error.

  forgectl surface close fix-login
  forgectl surface close fix-login --keep-worktree --json
  forgectl surface close fix-login --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceClose(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().BoolVar(&opts.KeepWorktree, "keep-worktree", false, "close the workspace and keep the worktree")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","branch","closed","workspace","worktree","kept_because","forgotten","reason","note"} as JSON (with --dry-run, the preview's keys)`)
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "print what close would do and change nothing (herdr is not asked)")
	return cmd
}

func runSurfaceClose(cmd *cobra.Command, deps module.Deps, opts closeOptions) error {
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), 2)
	}
	w, err := openWorkerLedger(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo)
	if err != nil {
		return err
	}
	rows, err := w.led.Rows()
	if err != nil {
		return WithExitCode(err, 2)
	}
	row, ok := findRow(rows, opts.Name)
	if !ok {
		return WithExitCode(fmt.Errorf("no worker named %q in this repo's ledger", opts.Name), 2)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), closeTimeout)
	defer cancel()
	if opts.DryRun {
		return renderClosePlan(cmd.OutOrStdout(), planClose(ctx, row, opts.KeepWorktree, time.Now(), func(ctx context.Context) (worker.WorktreeFacts, error) {
			return worker.InspectWorktree(ctx, deps.Runner, w.top, row.Name, row.Base)
		}), opts.JSON)
	}
	res := closeWorker(ctx, row, opts.KeepWorktree, time.Now(), closeSteps{
		close: w.herdr.Close,
		inspect: func(ctx context.Context) (worker.WorktreeFacts, error) {
			return worker.InspectWorktree(ctx, deps.Runner, w.top, row.Name, row.Base)
		},
		remove: func(ctx context.Context, path string) error {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
			defer cancel()
			return worker.RemoveWorktree(ctx, deps.Runner, w.top, path)
		},
		// Both act only on the row close read: a launch that reused the name
		// meanwhile is left alone.
		forget: func() error { return w.led.RemoveIf(row.Name, worker.SameRow(row)) },
		markClosed: func() error {
			return w.led.UpdateIf(row.Name, worker.SameRow(row), func(r *worker.Row) { r.Stage = worker.StageClosed })
		},
	})
	return reportClose(cmd, res, opts.JSON)
}

func findRow(rows []worker.Row, name string) (worker.Row, bool) {
	for _, r := range rows {
		if r.Name == name {
			return r, true
		}
	}
	return worker.Row{}, false
}

// closeSteps are close's I/O, injected so the order and the refusals can be
// tested without herdr or git.
type closeSteps struct {
	close      func(context.Context, backend.Ref) backend.CloseResult
	inspect    func(context.Context) (worker.WorktreeFacts, error)
	remove     func(context.Context, string) error
	forget     func() error
	markClosed func() error
}

// workspaceDecision is what the ledger row alone decides about the workspace:
// refuse, skip herdr (an earlier close, or nothing was ever created), or ask
// herdr to close ref. closeWorker and planClose both read it, so the preview
// and the real close cannot decide differently.
type workspaceDecision struct {
	// refuse, when set, is the reason close refuses and touches nothing.
	refuse string
	// state is the workspace outcome when herdr is not asked.
	state string
	// ref is the workspace to close when ask is true.
	ref backend.Ref
	ask bool
	// note is advice for the operator, carried to the result.
	note string
}

// decideWorkspace decides the workspace step from the row and the clock.
func decideWorkspace(row worker.Row, now time.Time) workspaceDecision {
	if launchInFlight(row, now) {
		return workspaceDecision{refuse: fmt.Sprintf("its launch may still be running (stage %s, started %s ago); retry after %s", row.Stage, now.Sub(row.StartedAt).Truncate(time.Second), launchSettleAfter)}
	}
	switch {
	case row.Stage == worker.StageClosed:
		// An earlier close closed the workspace and kept the worktree. herdr
		// is not asked again: after a restart it could only refuse.
		return workspaceDecision{state: closeWorkspaceEarlier}
	case len(row.Ref) == 0:
		d := workspaceDecision{state: closeWorkspaceNone}
		if row.Recovery != "" {
			d.note = "a failed launch may have left a herdr workspace labeled " + row.Recovery +
				"; forgectl cannot prove it is its own, so close it in herdr if it is still there"
		}
		return d
	}
	ref, err := backend.DecodeRef(row.Ref)
	if err != nil {
		return workspaceDecision{refuse: "the ledger reference does not decode"}
	}
	return workspaceDecision{ref: ref, ask: true}
}

// worktreeDecision is what the inspected worktree decides: keep it (and why),
// find it gone, or remove it at path.
type worktreeDecision struct {
	// outcome is closeWorktreeKept, closeWorktreeGone or closeWorktreeRemoved;
	// closeWorktreeRemoved here means "remove it", and closeWorker turns a
	// failed removal into kept.
	outcome string
	path    string
	because []string
}

// decideWorktree decides the worktree step. inspect is called only when the
// worktree is not being kept anyway, so --keep-worktree never reads git.
func decideWorktree(ctx context.Context, keep bool, inspect func(context.Context) (worker.WorktreeFacts, error)) worktreeDecision {
	if keep {
		return worktreeDecision{outcome: closeWorktreeKept, because: []string{"--keep-worktree"}}
	}
	facts, err := inspect(ctx)
	switch {
	case err != nil:
		return worktreeDecision{outcome: closeWorktreeKept, because: []string{"git could not be read: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}}
	case !facts.Present():
		return worktreeDecision{outcome: closeWorktreeGone}
	}
	if blockers := worker.RemovalBlockers(facts); len(blockers) > 0 {
		return worktreeDecision{outcome: closeWorktreeKept, because: blockers}
	}
	return worktreeDecision{outcome: closeWorktreeRemoved, path: facts.Path}
}

// keepsRow reports whether the ledger row stays: a kept worktree keeps it (at
// stage closed); anything else forgets it.
func (d worktreeDecision) keepsRow() bool { return d.outcome == closeWorktreeKept }

// closeWorker closes the workspace first, so the harness stops before its
// worktree is judged, then decides the worktree, then the ledger row.
func closeWorker(ctx context.Context, row worker.Row, keepWorktree bool, now time.Time, s closeSteps) closeResult {
	res := closeResult{Name: row.Name, Branch: row.Branch, Worktree: closeWorktreeUntouched}
	refuse := func(reason string) closeResult {
		res.Closed, res.Workspace, res.Reason = false, closeWorkspaceRefused, reason
		return res
	}

	ws := decideWorkspace(row, now)
	if ws.refuse != "" {
		return refuse(ws.refuse)
	}
	res.Note = ws.note
	if ws.ask {
		cr := s.close(ctx, ws.ref)
		switch cr.State() {
		case backend.CloseClosed:
			res.Workspace = closeWorkspaceClosed
		case backend.CloseAlreadyGone:
			res.Workspace = closeWorkspaceGone
		case backend.CloseIdentityMismatch:
			return refuse("herdr restarted, or the workspace id no longer carries forgectl's marker (" + causeText(cr) + ")")
		case backend.CloseUnreadable:
			return refuse("herdr could not be read (" + causeText(cr) + ")")
		default:
			return refuse("herdr did not close the workspace (" + causeText(cr) + ")")
		}
	} else {
		res.Workspace = ws.state
	}
	res.Closed = true

	wt := decideWorktree(ctx, keepWorktree, s.inspect)
	res.Worktree, res.KeptBecause = wt.outcome, wt.because
	if wt.outcome == closeWorktreeRemoved {
		if err := s.remove(ctx, wt.path); err != nil {
			res.Worktree = closeWorktreeKept
			res.KeptBecause = []string{termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
		}
	}

	if res.Worktree == closeWorktreeKept {
		if err := s.markClosed(); err != nil {
			res.Note = joinNote(res.Note, "the ledger row could not be marked closed: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
		}
		return res
	}
	if err := s.forget(); err != nil && !errors.Is(err, worker.ErrNoRow) {
		res.Note = joinNote(res.Note, "the ledger row could not be removed: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
		return res
	}
	res.Forgotten = true
	return res
}

// planClose is closeWorker's --dry-run: the same two decisions (decideWorkspace
// and decideWorktree), with herdr not asked and nothing removed. It reads the
// worktree (inspect is read-only) and changes nothing.
func planClose(ctx context.Context, row worker.Row, keepWorktree bool, now time.Time, inspect func(context.Context) (worker.WorktreeFacts, error)) closePlan {
	plan := closePlan{Name: row.Name, Branch: row.Branch, DryRun: true, Worktree: closeWorktreeUntouched}
	ws := decideWorkspace(row, now)
	if ws.refuse != "" {
		plan.Refused, plan.Workspace, plan.Reason = true, closeWorkspaceRefused, ws.refuse
		return plan
	}
	plan.Note = ws.note
	if ws.ask {
		plan.Workspace = closeWorkspaceWouldClose
	} else {
		plan.Workspace = ws.state
	}

	wt := decideWorktree(ctx, keepWorktree, inspect)
	plan.KeptBecause = wt.because
	switch wt.outcome {
	case closeWorktreeRemoved:
		plan.Worktree = closeWorktreeWouldRemove
	case closeWorktreeKept:
		plan.Worktree = closeWorktreeWouldKeep
	default:
		plan.Worktree = wt.outcome
	}
	plan.WouldForget = !wt.keepsRow()
	return plan
}

// launchInFlight reports a row whose launch may still be running: stage
// pending or worktree, started less than launchSettleAfter ago.
func launchInFlight(row worker.Row, now time.Time) bool {
	if row.Stage != worker.StagePending && row.Stage != worker.StageWorktree {
		return false
	}
	return now.Sub(row.StartedAt) < launchSettleAfter
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// reportClose prints the result; a refusal is exit 1.
func reportClose(cmd *cobra.Command, r closeResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, r); err != nil {
			return err
		}
		if !r.Closed {
			return newSilentCodedError(1)
		}
		return nil
	}
	if !r.Closed {
		return WithExitCode(fmt.Errorf("worker %s: close refused, nothing was touched: %s", termsafe.SafeLineMax(r.Name, 64), termsafe.SafeLineMax(r.Reason, 300)), 1)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: workspace %s, worktree %s", termsafe.SafeLineMax(r.Name, 64), r.Workspace, r.Worktree)
	if r.Forgotten {
		b.WriteString(", ledger row removed")
	}
	fmt.Fprintf(&b, "; branch %s left as is\n", termsafe.SafeLineMax(r.Branch, 120))
	for _, k := range r.KeptBecause {
		fmt.Fprintf(&b, "  kept: %s\n", termsafe.SafeLineMax(k, 300))
	}
	if r.Note != "" {
		fmt.Fprintf(&b, "  note: %s\n", termsafe.SafeLineMax(r.Note, 300))
	}
	_, err := fmt.Fprint(out, b.String())
	return err
}
