package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface ready <name>` waits until a worker's harness is at its input
// prompt, and never answers a dialog to get it there.

const (
	defaultReadyTimeout  = 120 * time.Second
	defaultReadyInterval = time.Second
)

// readyResult is what `surface ready --json` prints. Additive changes only
// (ADR-0008 rule 2).
type readyResult struct {
	Name     string      `json:"name"`
	Harness  string      `json:"harness"`
	State    ready.State `json:"state"`
	Blocking string      `json:"blocking,omitempty"`
	Reason   string      `json:"reason,omitempty"`
	Input    string      `json:"input,omitempty"`
	WaitedMS int64       `json:"waited_ms"`
}

// readyStateGone and readyStateUnreadable extend ready.State for outcomes the
// predicates never see: the workspace is gone, or herdr could not be read.
const (
	readyStateGone       ready.State = "gone"
	readyStateUnreadable ready.State = "unreadable"
)

type readyOptions struct {
	Repo     string
	Name     string
	Timeout  time.Duration
	Interval time.Duration
	JSON     bool
}

func newSurfaceReadyCmd(deps module.Deps) *cobra.Command {
	opts := readyOptions{}
	cmd := &cobra.Command{
		Use:   "ready <name>",
		Short: "Wait until a worker's harness is at its input prompt",
		Long: `ready waits until the named worker's harness is at its input prompt.

A worker counts as ready only when three signals agree: no known blocking
screen is showing, herdr detects the expected agent as idle or done, and the
harness's own input prompt is visible. herdr's status alone is a hint, never
proof. All three can be set from inside the worker's pane, so ready guards
against accidents, not against a worker trying to look ready.

ready never answers a dialog. A blocking screen (folder-trust dialog,
plan-approval dialog, permission prompt, npm's "Ok to proceed?") ends the wait
at once with exit 1, naming the screen; answer it in the worker's pane.

Predicates are built in, and can be replaced by
<config dir>/forgectl/surface-ready.toml. They are never read from a repo or
worktree.

Exit 0: ready. Exit 1: blocked, gone, unreadable, or not ready by --timeout.
Exit 2: a usage or setup error (no such worker, a ledger or predicate file
that cannot be used, a harness with no predicates).

  forgectl surface ready fix-login
  forgectl surface ready fix-login --timeout 5m --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceReady(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", defaultReadyTimeout, "how long to wait for the prompt")
	cmd.Flags().DurationVar(&opts.Interval, "interval", defaultReadyInterval, "how often to read the pane")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","harness","state","blocking","reason","input","waited_ms"} as JSON`)
	return cmd
}

func runSurfaceReady(cmd *cobra.Command, deps module.Deps, opts readyOptions) error {
	if opts.Timeout <= 0 || opts.Interval <= 0 {
		return WithExitCode(errors.New("--timeout and --interval must be positive"), exitUsage)
	}
	w, err := openWorker(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo, opts.Name)
	if err != nil {
		return err
	}

	// The deadline bounds the herdr calls too: a wedged server that accepts
	// the socket and never answers must end as unreadable, not hang.
	// One interval of slack, so the loop's own timeout check (which knows the
	// last verdict) normally ends the wait before the deadline cuts a read.
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.Timeout+opts.Interval)
	defer cancel()
	res := waitReady(ctx, readyLoop{
		read:     w.read,
		evaluate: w.evaluate,
		now:      time.Now,
		sleep:    resume.SleepContext,
		timeout:  opts.Timeout,
		interval: opts.Interval,
	})
	res.Name, res.Harness = w.row.Name, w.row.Harness
	return reportReady(cmd, res, opts.JSON)
}

// openedWorker is one launched worker, ready to be read and written: its
// ledger row and reference, the adapter that reaches its pane, and the
// readiness predicates for its harness.
type openedWorker struct {
	herdr *herdradapter.Adapter
	led   *worker.Ledger
	row   worker.Row
	ref   backend.Ref
	table *ready.Table
}

func (w *openedWorker) read(ctx context.Context) (ready.Screen, error) {
	return w.herdr.WorkerScreen(ctx, w.ref)
}

func (w *openedWorker) evaluate(s ready.Screen) ready.Verdict {
	return w.table.Evaluate(w.row.Harness, s)
}

// openWorker finds the launched worker name in repo's ledger and loads its
// harness's predicates. Every failure is a usage or setup error (exit 2).
// warn takes the herdr adapter's setup warnings.
func openWorker(ctx context.Context, warn io.Writer, deps module.Deps, repo, name string) (*openedWorker, error) {
	if err := worker.ValidName(name); err != nil {
		return nil, WithExitCode(fmt.Errorf("name: %w", err), exitUsage)
	}
	wl, err := openWorkerLedger(ctx, warn, deps, repo)
	if err != nil {
		return nil, err
	}
	herdr, led := wl.herdr, wl.led
	row, ref, err := launchedWorker(led, name)
	if err != nil {
		return nil, WithExitCode(err, exitUsage)
	}

	path, err := config.SurfaceReadyPredicatesPath()
	if err != nil {
		return nil, WithExitCode(err, exitUsage)
	}
	table, err := ready.Load(path)
	if err != nil {
		return nil, WithExitCode(termsafe.Error(err), exitUsage)
	}
	if !table.Has(row.Harness) {
		return nil, WithExitCode(fmt.Errorf("no readiness predicates for harness %q (built-in table, or %s)", row.Harness, path), exitUsage)
	}
	return &openedWorker{herdr: herdr, led: led, row: row, ref: ref, table: table}, nil
}

// launchedWorker finds the row named name and decodes its reference. Only a
// launched row has a pane to read.
func launchedWorker(led *worker.Ledger, name string) (worker.Row, backend.Ref, error) {
	rows, err := led.Rows()
	if err != nil {
		return worker.Row{}, backend.Ref{}, err
	}
	r, ok := findRow(rows, name)
	if !ok {
		return worker.Row{}, backend.Ref{}, fmt.Errorf("no worker named %q in this repo's ledger", name)
	}
	if r.Stage != worker.StageLaunched {
		return worker.Row{}, backend.Ref{}, fmt.Errorf("worker %q is at stage %q, want %q", name, r.Stage, worker.StageLaunched)
	}
	ref, err := backend.DecodeRef(r.Ref)
	if err != nil {
		return worker.Row{}, backend.Ref{}, fmt.Errorf("worker %q: its ledger reference does not decode: %w", name, err)
	}
	return r, ref, nil
}

// readyLoop is the wait, with its I/O and clock injected so the decision can
// be tested without herdr.
type readyLoop struct {
	read     func(context.Context) (ready.Screen, error)
	evaluate func(ready.Screen) ready.Verdict
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	timeout  time.Duration
	interval time.Duration
}

// waitReady reads and evaluates until the worker is ready, a blocking screen
// shows, the workspace is gone, or the timeout passes.
//
// A blocking screen ends the wait at once: it needs the operator, and waiting
// longer only delays saying so. An unreadable pane is retried, because one
// failed herdr call says nothing about the worker; it is reported only if it
// is still the last thing seen at the timeout.
func waitReady(ctx context.Context, l readyLoop) readyResult {
	start := l.now()
	var last readyResult
	for {
		s, err := l.read(ctx)
		switch {
		case errors.Is(err, herdradapter.ErrWorkerGone):
			return finish(readyResult{State: readyStateGone, Reason: "the worker's herdr workspace is gone"}, start, l.now())
		case err != nil && ctx.Err() != nil && last.State != "":
			// The deadline cut this read short. The last real verdict says
			// more than the cancellation does.
			last.Reason = fmt.Sprintf("not ready after %s: %s", l.timeout, last.Reason)
			return finish(last, start, l.now())
		case err != nil:
			last = readyResult{State: readyStateUnreadable, Reason: termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
		default:
			v := l.evaluate(s)
			last = readyResult{State: v.State, Blocking: v.Blocking, Reason: v.Reason, Input: v.Input}
			if v.State == ready.StateReady || v.State == ready.StateBlocked {
				return finish(last, start, l.now())
			}
		}
		if l.now().Sub(start)+l.interval > l.timeout {
			last.Reason = fmt.Sprintf("not ready after %s: %s", l.timeout, last.Reason)
			return finish(last, start, l.now())
		}
		if err := l.sleep(ctx, l.interval); err != nil {
			last.Reason = "wait canceled: " + err.Error()
			return finish(last, start, l.now())
		}
	}
}

func finish(r readyResult, start, end time.Time) readyResult {
	r.WaitedMS = end.Sub(start).Milliseconds()
	return r
}

// reportReady prints the result and returns exit 1 for anything but ready.
//
// With --json the verdict on stdout is the whole answer, so a non-ready result
// exits 1 with no second error object on stderr (docs/json-contract.md).
// writeJSON escapes every string field for the terminal: the reason and input
// can carry text from another pane's screen.
func reportReady(cmd *cobra.Command, r readyResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, r); err != nil {
			return err
		}
		if r.State == ready.StateReady {
			return nil
		}
		return newSilentCodedError(1)
	}
	if r.State == ready.StateReady {
		_, err := fmt.Fprintf(out, "%s: ready\n", r.Name)
		return err
	}
	return WithExitCode(fmt.Errorf("worker %s is %s: %s", r.Name, r.State, termsafe.SafeLineMax(r.Reason, 300)), exitFailed)
}
