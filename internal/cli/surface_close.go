package cli

import (
	"context"
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

const closeTimeout = 60 * time.Second

// What close did to the workspace.
const (
	closeWorkspaceClosed   = "closed"
	closeWorkspaceGone     = "already-gone"
	closeWorkspaceNone     = "none"
	closeWorkspaceRefused  = "refused"
	closeWorktreeRemoved   = "removed"
	closeWorktreeKept      = "kept"
	closeWorktreeGone      = "gone"
	closeWorktreeUntouched = "untouched"
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
cannot be read or the workspace cannot be proven to be forgectl's.

When the workspace is closed and no worktree remains, the ledger row is
removed. A kept worktree keeps the row at stage "closed"; run close again
once the work is saved.

Exit 0: the workspace is closed or gone (the worktree may be kept). Exit 1:
refused. Exit 2: a usage or setup error.

  forgectl surface close fix-login
  forgectl surface close fix-login --keep-worktree --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceClose(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().BoolVar(&opts.KeepWorktree, "keep-worktree", false, "close the workspace and keep the worktree")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "print the result as JSON")
	return cmd
}

func runSurfaceClose(cmd *cobra.Command, deps module.Deps, opts closeOptions) error {
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), 2)
	}
	w, err := openWorkerLedger(cmd, deps, opts.Repo)
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
	res := closeWorker(ctx, row, opts.KeepWorktree, closeSteps{
		close: w.herdr.Close,
		inspect: func(ctx context.Context) (worker.WorktreeFacts, error) {
			return worker.InspectWorktree(ctx, deps.Runner, w.top, row.Name, row.Base)
		},
		remove: func(ctx context.Context, path string) error {
			return worker.RemoveWorktree(ctx, deps.Runner, w.top, path)
		},
		forget: func() error { return w.led.Remove(row.Name) },
		markClosed: func() error {
			return w.led.Update(row.Name, func(r *worker.Row) { r.Stage = worker.StageClosed })
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

// closeWorker closes the workspace first, so the harness stops before its
// worktree is judged, then decides the worktree, then the ledger row.
func closeWorker(ctx context.Context, row worker.Row, keepWorktree bool, s closeSteps) closeResult {
	res := closeResult{Name: row.Name, Branch: row.Branch, Worktree: closeWorktreeUntouched}
	refuse := func(reason string) closeResult {
		res.Closed, res.Workspace, res.Reason = false, closeWorkspaceRefused, reason
		return res
	}

	switch {
	case len(row.Ref) == 0:
		res.Workspace = closeWorkspaceNone
		if row.Recovery != "" {
			res.Note = "a failed launch may have left a herdr workspace labeled " + row.Recovery +
				"; forgectl cannot prove it is its own, so close it in herdr if it is still there"
		}
	default:
		ref, err := backend.DecodeRef(row.Ref)
		if err != nil {
			return refuse("the ledger reference does not decode")
		}
		cr := s.close(ctx, ref)
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
	}
	res.Closed = true

	if keepWorktree {
		res.Worktree, res.KeptBecause = closeWorktreeKept, []string{"--keep-worktree"}
	} else {
		facts, err := s.inspect(ctx)
		switch {
		case err != nil:
			res.Worktree = closeWorktreeKept
			res.KeptBecause = []string{"git could not be read: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
		case !facts.Present():
			res.Worktree = closeWorktreeGone
		default:
			if blockers := worker.RemovalBlockers(facts); len(blockers) > 0 {
				res.Worktree, res.KeptBecause = closeWorktreeKept, blockers
			} else if err := s.remove(ctx, facts.Path); err != nil {
				res.Worktree = closeWorktreeKept
				res.KeptBecause = []string{termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
			} else {
				res.Worktree = closeWorktreeRemoved
			}
		}
	}

	if res.Worktree == closeWorktreeKept {
		if err := s.markClosed(); err != nil {
			res.Note = joinNote(res.Note, "the ledger row could not be marked closed: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
		}
		return res
	}
	if err := s.forget(); err != nil {
		res.Note = joinNote(res.Note, "the ledger row could not be removed: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
		return res
	}
	res.Forgotten = true
	return res
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
		return WithExitCode(fmt.Errorf("worker %s: close refused, nothing was touched: %s", r.Name, termsafe.SafeLineMax(r.Reason, 300)), 1)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: workspace %s, worktree %s", r.Name, r.Workspace, r.Worktree)
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
