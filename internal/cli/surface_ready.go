package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
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
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	State    string `json:"state"`
	Blocking string `json:"blocking,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Input    string `json:"input,omitempty"`
	WaitedMS int64  `json:"waited_ms"`
}

// readyStateGone and readyStateUnreadable extend ready.State for outcomes the
// predicates never see: the workspace is gone, or herdr could not be read.
const (
	readyStateGone       = "gone"
	readyStateUnreadable = "unreadable"
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
proof.

ready never answers a dialog. A blocking screen (folder-trust dialog,
plan-approval dialog, permission prompt, npm's "Ok to proceed?") ends the wait
at once with exit 1, naming the screen; answer it in the worker's pane.

Predicates are built in, and can be replaced by
<config dir>/forgectl/surface-ready.toml. They are never read from a repo or
worktree.

Exit 0: ready. Exit 1: blocked, gone, unreadable, or not ready by --timeout.

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
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "print the result as JSON")
	return cmd
}

func runSurfaceReady(cmd *cobra.Command, deps module.Deps, opts readyOptions) error {
	if err := worker.ValidName(opts.Name); err != nil {
		return WithExitCode(fmt.Errorf("name: %w", err), 2)
	}
	if opts.Timeout <= 0 || opts.Interval <= 0 {
		return WithExitCode(errors.New("--timeout and --interval must be positive"), 2)
	}
	adapter, err := newHerdrAdapter(cmd.ErrOrStderr())
	if err != nil {
		return WithExitCode(err, 2)
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return errors.New("forgectl: the herdr adapter has an unexpected type")
	}

	ctx := cmd.Context()
	target, err := projects.New(deps.Runner).ResolveTarget(opts.Repo)
	if err != nil {
		return WithExitCode(err, 2)
	}
	top, err := worker.RepoTop(ctx, deps.Runner, target)
	if err != nil {
		return WithExitCode(err, 2)
	}
	led, err := worker.Open(top, herdr.Session())
	if err != nil {
		return err
	}
	row, ref, err := launchedWorker(led, opts.Name)
	if err != nil {
		return WithExitCode(err, 2)
	}

	path, err := config.SurfaceReadyPredicatesPath()
	if err != nil {
		return err
	}
	table, err := ready.Load(path)
	if err != nil {
		return termsafe.Error(err)
	}

	res := waitReady(ctx, readyLoop{
		read:     func(ctx context.Context) (ready.Screen, error) { return herdr.WorkerScreen(ctx, ref) },
		evaluate: func(s ready.Screen) ready.Verdict { return table.Evaluate(row.Harness, s) },
		now:      time.Now,
		sleep:    sleepCtx,
		timeout:  opts.Timeout,
		interval: opts.Interval,
	})
	res.Name, res.Harness = row.Name, row.Harness
	return reportReady(cmd, res, opts.JSON)
}

// launchedWorker finds the row named name and decodes its reference. Only a
// launched row has a pane to read.
func launchedWorker(led *worker.Ledger, name string) (worker.Row, backend.Ref, error) {
	rows, err := led.Rows()
	if err != nil {
		return worker.Row{}, backend.Ref{}, err
	}
	for _, r := range rows {
		if r.Name != name {
			continue
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
	return worker.Row{}, backend.Ref{}, fmt.Errorf("no worker named %q in this repo's ledger", name)
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
		case err != nil:
			last = readyResult{State: readyStateUnreadable, Reason: termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
		default:
			v := l.evaluate(s)
			last = readyResult{State: string(v.State), Blocking: v.Blocking, Reason: v.Reason, Input: v.Input}
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

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reportReady prints the result and returns exit 1 for anything but ready.
// The input text comes from another pane's screen, so it is printed through
// termsafe and only in JSON, where it is a quoted string.
func reportReady(cmd *cobra.Command, r readyResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		r.Input = termsafe.SafeLineMax(r.Input, 200)
		// termsafe:allow-raw-json every string field passed through termsafe or is a fixed token
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, string(data)); err != nil {
			return err
		}
	} else if r.State == string(ready.StateReady) {
		if _, err := fmt.Fprintf(out, "%s: ready\n", r.Name); err != nil {
			return err
		}
	}
	if r.State == string(ready.StateReady) {
		return nil
	}
	return WithExitCode(fmt.Errorf("worker %s is %s: %s", r.Name, r.State, termsafe.SafeLineMax(r.Reason, 300)), 1)
}
