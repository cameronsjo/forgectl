package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface forget <name> --surface hc` drops a worker's ledger row when hc
// proves its terminal is gone, without asking hc to act on anything
// (forgectl#1386). It is close's way out for the one case close cannot
// settle: a core whose state was wiped reports a new id, so close calls every
// row taken on the old core an identity mismatch and refuses it.

// forgetTimeout bounds the two hc reads forget makes.
const forgetTimeout = 30 * time.Second

// forgetResult is what `surface forget --json` prints. Additive changes only
// (ADR-0008 rule 2).
type forgetResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// Forgotten is false when forget refused: the row is unchanged.
	Forgotten bool `json:"forgotten"`
	// Reason says why the row was dropped, or why forget refused.
	Reason string `json:"reason"`
	// Worktree is where the worker's worktree would be. forget never reads
	// or removes it.
	Worktree string `json:"worktree"`
	Note     string `json:"note,omitempty"`
}

type forgetOptions struct {
	Repo    string
	Surface string
	Name    string
	// LegacyRow lets a row with no recorded core selection have a changed
	// core id decided by the address read alone.
	LegacyRow bool
	JSON      bool
}

func newSurfaceForgetCmd(deps module.Deps) *cobra.Command {
	opts := forgetOptions{}
	cmd := &cobra.Command{
		Use:   "forget <name>",
		Short: "Drop an hc worker's ledger row once hc proves its terminal is gone",
		Long: `forget removes the named worker's ledger row and nothing else, when hc
proves the worker's terminal is not on the core: the core's id is not the one
the worker was started on (its state was wiped, which takes every terminal
with it), hc has no terminal at the recorded address, or that address now
holds a terminal forgectl did not start. It asks hc only to read (hc server
status, hc get), and never kills, removes, or types anything.

A row at stage closed (an earlier close closed its terminal and kept the
worktree) is removed without asking hc once git no longer lists that worktree
and nothing exists at its path; while the worktree is still there, run
surface close again once its work is saved.

It refuses, changing nothing, when hc cannot be read, when the reference was
taken under another HC_SESSION, when forgectl's own terminal is still there
(live or finished: run surface close), and for a row close settles without hc
(no recorded terminal, or a launch less than ten minutes old).

The worktree and the branch are left as they are: forget only reads git
(git worktree list, for a row at stage closed). Inspect the worktree, save
its work, and remove it with git worktree remove.

A --repo naming a worker's worktree that no longer exists resolves to the
repository that worktree was in, with a note.

A row launched before forgectl recorded its core selection (hc_selection)
refuses when the core id changed, since another HC_SOCKET or XDG path alone
reaches a different core while the worker's may still run. --legacy-row
decides such a row by the recorded address on the core this environment
reaches: pass it only from the environment the worker was launched under.
The address and creation-time read still runs, so a worker still there
refuses. --legacy-row has no effect on a row that records its selection, or
when the core id is unchanged.

Only --surface hc is supported. A herdr worker's close settles a restarted
server by forgectl's ownership marker.

Exit 0: the row was removed. Exit 1: refused. Exit 2: a usage or setup error.

  forgectl surface forget fix-login --surface hc
  forgectl surface forget fix-login --surface hc --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceForget(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	addWorkerSurfaceFlag(cmd, &opts.Surface)
	cmd.Flags().BoolVar(&opts.LegacyRow, "legacy-row", false, "for a row with no recorded core selection, decide a changed core id by the recorded address on this core (run it from the worker's launch environment)")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","branch","forgotten","reason","worktree","note"} as JSON`)
	return cmd
}

// errForgetHcOnly refuses forget on a surface other than hc.
var errForgetHcOnly = errors.New("forget supports --surface hc only; a herdr worker's close settles a restarted server by forgectl's ownership marker")

func runSurfaceForget(cmd *cobra.Command, deps module.Deps, opts forgetOptions) error {
	if err := refuseInDrainWorker(os.Getenv, "surface forget"); err != nil {
		return err
	}
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	if err := checkWorkerSurface(opts.Surface); err != nil {
		return err
	}
	if opts.Surface != workerSurfaceHc {
		return WithExitCode(errForgetHcOnly, exitUsage)
	}
	w, err := openSurfaceLedger(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo, opts.Surface)
	if err != nil {
		return err
	}
	rows, err := w.led.Rows()
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	row, ok := findRow(rows, opts.Name)
	if !ok {
		return WithExitCode(fmt.Errorf("no worker named %q in this repo's ledger; forgectl surface list --surface hc shows the workers", opts.Name), exitUsage)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), forgetTimeout)
	defer cancel()
	res := forgetWorker(ctx, row, time.Now(), opts.LegacyRow, forgetSteps{
		abandoned: w.hc.Abandoned,
		worktreeGone: func(ctx context.Context) (bool, error) {
			facts, err := worker.InspectWorktree(ctx, deps.Runner, w.top, row.Name, row.Base)
			if err != nil {
				return false, err
			}
			return !facts.Present(), nil
		},
		remove: func() error { return w.led.RemoveIf(row.Name, worker.SameRow(row)) },
	})
	res.Worktree = worker.WorktreePath(w.top, row.Name)
	return reportForget(cmd, res, opts.JSON)
}

// forgetSteps are forget's I/O, injected so each refusal is testable without
// hc.
type forgetSteps struct {
	// abandoned is hcadapter.Adapter.Abandoned with the row's recorded core
	// selection: a reason when hc proves the terminal gone, an error
	// otherwise.
	abandoned func(ctx context.Context, ref backend.Ref, launched string, legacy bool) (string, error)
	// worktreeGone reports that git does not list the worker's worktree and
	// nothing exists at its path (worker.WorktreeFacts.Present). It is asked
	// only for a row at stage closed.
	worktreeGone func(ctx context.Context) (bool, error)
	// remove drops the row, only if it is still the row forget read.
	remove func() error
}

// removeRow drops the row through s.remove and reports the outcome in res.
func (s forgetSteps) removeRow(res forgetResult, reason string) forgetResult {
	if err := s.remove(); err != nil {
		if errors.Is(err, worker.ErrNoRow) || errors.Is(err, worker.ErrRowChanged) {
			res.Reason = "the ledger row changed or went away while forget ran; nothing was removed"
			return res
		}
		res.Reason = "the ledger row could not be removed: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
		return res
	}
	res.Forgotten, res.Reason = true, reason
	return res
}

// forgetWorker decides from the row first, then asks hc, then removes the
// row. Every refusal leaves the row as it was.
func forgetWorker(ctx context.Context, row worker.Row, now time.Time, legacy bool, s forgetSteps) forgetResult {
	res := forgetResult{Name: row.Name, Branch: row.Branch}
	refuse := func(reason string) forgetResult {
		res.Reason = reason
		return res
	}
	switch {
	case launchInFlight(row, now):
		return refuse(fmt.Sprintf("its launch may still be running (stage %s, started %s ago); retry after %s", row.Stage, now.Sub(row.StartedAt).Truncate(time.Second), launchSettleAfter))
	case row.Stage == worker.StageClosed:
		// An earlier close closed the terminal and kept the worktree. Once the
		// worktree is gone too there is nothing left for close to do, so the
		// row goes (forgectl#1456). hc is not asked: the terminal is closed.
		gone, err := s.worktreeGone(ctx)
		switch {
		case err != nil:
			return refuse("an earlier close already closed its terminal and kept the worktree, and git could not be read to see whether it is still there: " +
				termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
		case !gone:
			return refuse("an earlier close already closed its terminal and kept the worktree; run surface close again once the work is saved")
		}
		return s.removeRow(res, "an earlier close closed its terminal, and its worktree no longer exists")
	case len(row.Ref) == 0:
		return refuse("the row records no terminal, so surface close settles it without asking hc")
	}
	ref, err := backend.DecodeRef(row.Ref)
	if err != nil {
		return refuse("the ledger reference does not decode, so nothing proves its terminal gone")
	}
	if ref.Kind() != backend.KindHc {
		return refuse("the row's reference is not an hc terminal")
	}
	reason, err := s.abandoned(ctx, ref, row.HcSelection, legacy)
	switch {
	case errors.Is(err, hcadapter.ErrStillThere):
		return refuse("the worker's terminal is still on the core, at its recorded address and creation time: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
	case errors.Is(err, hcadapter.ErrSelectionUnproven):
		return refuse("hc reaches a different core, which proves nothing about the worker's own: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen) +
			"; run surface close under the environment the worker was launched in")
	case err != nil:
		return refuse("hc could not prove the worker's terminal gone: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen))
	}
	res = s.removeRow(res, reason)
	if res.Forgotten {
		res.Note = "the worktree and branch are left as they are; save any work and remove the worktree with git worktree remove"
	}
	return res
}

// reportForget prints the result; a refusal is exit 1.
func reportForget(cmd *cobra.Command, r forgetResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, r); err != nil {
			return err
		}
		if !r.Forgotten {
			return newSilentCodedError(exitFailed)
		}
		return nil
	}
	if !r.Forgotten {
		return WithExitCode(fmt.Errorf("worker %s: forget refused, the ledger row is unchanged: %s", termsafe.SafeLineMax(r.Name, 64), termsafe.SafeLineMax(r.Reason, 300)), exitFailed)
	}
	_, err := fmt.Fprintf(out, "%s: ledger row removed (%s)\n  note: %s\n", termsafe.SafeLineMax(r.Name, 64), termsafe.SafeLineMax(r.Reason, 300), termsafe.SafeLineMax(r.Note, 300))
	return err
}
