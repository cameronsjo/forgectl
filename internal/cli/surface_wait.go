package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface wait <name>` waits until a worker's turn settles: it is back at
// its input prompt and has stayed there.

const (
	defaultWaitTimeout  = 30 * time.Minute
	defaultWaitInterval = 2 * time.Second
	// defaultWaitSettle is how long the worker must stay at its prompt.
	defaultWaitSettle = 5 * time.Second
	// defaultWaitQuiet is how long a worker must sit at its prompt before
	// wait settles without having seen a turn or a report: the turn may have
	// ended before wait started, with no REPORT line.
	defaultWaitQuiet = 30 * time.Second
)

// readyStateSettled is wait's success state.
const readyStateSettled ready.State = "settled"

// waitResult is what `surface wait --json` prints. Additive changes only
// (ADR-0008 rule 2).
type waitResult struct {
	Name     string      `json:"name"`
	Harness  string      `json:"harness"`
	State    ready.State `json:"state"`
	Blocking string      `json:"blocking,omitempty"`
	Reason   string      `json:"reason,omitempty"`
	// TurnSeen reports that herdr showed the worker working during the wait.
	TurnSeen bool `json:"turn_seen"`
	// Report reports that the last brief's REPORT line is on screen.
	Report   bool  `json:"report"`
	WaitedMS int64 `json:"waited_ms"`
}

type waitOptions struct {
	Repo     string
	Name     string
	Timeout  time.Duration
	Interval time.Duration
	Settle   time.Duration
	Quiet    time.Duration
	JSON     bool
}

func newSurfaceWaitCmd(deps module.Deps) *cobra.Command {
	opts := waitOptions{}
	cmd := &cobra.Command{
		Use:   "wait <name>",
		Short: "Wait until a worker's turn settles at its input prompt",
		Long: `wait waits until the named worker's turn has settled: the worker is at its
input prompt (the same three signals surface ready reads) and has stayed there
for --settle.

A worker can sit at its prompt before its turn starts, so wait also needs one
of: herdr reported it working during the wait, the last brief's REPORT line
is on screen, or it has sat at its prompt for --quiet. The screen text itself
is not compared for stability: a harness's status line changes every second.

A blocking screen (a permission prompt, the plan-approval dialog) ends the
wait at once with exit 1, naming it; answer it in the worker's pane. wait
never answers a dialog.

Exit 0: settled. Exit 1: blocked, gone, unreadable, or not settled by
--timeout. Exit 2: a usage or setup error.

  forgectl surface wait fix-login
  forgectl surface wait fix-login --timeout 2h --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			return runSurfaceWait(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", defaultWaitTimeout, "how long to wait for the turn to settle")
	cmd.Flags().DurationVar(&opts.Interval, "interval", defaultWaitInterval, "how often to read the pane")
	cmd.Flags().DurationVar(&opts.Settle, "settle", defaultWaitSettle, "how long the worker must stay at its prompt")
	cmd.Flags().DurationVar(&opts.Quiet, "quiet", defaultWaitQuiet, "settle after this long at the prompt even with no turn or report seen")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","harness","state","blocking","reason","turn_seen","report","waited_ms"} as JSON`)
	return cmd
}

func runSurfaceWait(cmd *cobra.Command, deps module.Deps, opts waitOptions) error {
	if opts.Timeout <= 0 || opts.Interval <= 0 || opts.Settle < 0 || opts.Quiet < opts.Settle {
		return WithExitCode(errors.New("--timeout and --interval must be positive, and --quiet at least --settle"), 2)
	}
	w, err := openWorker(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo, opts.Name)
	if err != nil {
		return err
	}
	marker := ""
	if w.row.Brief != nil {
		marker = w.row.Brief.Marker
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.Timeout+opts.Interval)
	defer cancel()
	res := waitSettled(ctx, waitLoop{
		read:     w.read,
		evaluate: w.evaluate,
		marker:   marker,
		now:      time.Now,
		sleep:    resume.SleepContext,
		timeout:  opts.Timeout,
		interval: opts.Interval,
		settle:   opts.Settle,
		quiet:    opts.Quiet,
	})
	res.Name, res.Harness = w.row.Name, w.row.Harness
	return reportWait(cmd, res, opts.JSON)
}

// waitLoop is the wait, with its I/O and clock injected.
type waitLoop struct {
	read     func(context.Context) (ready.Screen, error)
	evaluate func(ready.Screen) ready.Verdict
	marker   string
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	timeout  time.Duration
	interval time.Duration
	settle   time.Duration
	quiet    time.Duration
}

// waitSettled reads until the worker settles, a blocking screen shows, the
// workspace is gone, or the timeout passes. An unreadable pane is retried,
// as in waitReady, and breaks the settle run.
func waitSettled(ctx context.Context, l waitLoop) waitResult {
	start := l.now()
	var last waitResult
	var readySince time.Time
	for {
		s, err := l.read(ctx)
		switch {
		case errors.Is(err, herdradapter.ErrWorkerGone):
			last.State, last.Reason = readyStateGone, "the worker's herdr workspace is gone"
			return finishWait(last, start, l.now())
		case err != nil && ctx.Err() != nil && last.State != "":
			last.Reason = fmt.Sprintf("not settled after %s: %s", l.timeout, last.Reason)
			return finishWait(last, start, l.now())
		case err != nil:
			last.State, last.Blocking = readyStateUnreadable, ""
			last.Reason = termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
			readySince = time.Time{}
		default:
			if s.Status == "working" {
				last.TurnSeen = true
			}
			if l.marker != "" {
				_, last.Report = worker.FindReport(s.Text, l.marker)
			}
			v := l.evaluate(s)
			last.State, last.Blocking, last.Reason = v.State, v.Blocking, v.Reason
			if v.State == ready.StateBlocked {
				return finishWait(last, start, l.now())
			}
			if v.Ready() && v.Input != "" {
				// Text left in the input box (a refused brief) is not a
				// settled turn: no turn will run until someone sends or
				// clears it.
				last.State = ready.StateNotReady
				last.Reason = "the input box holds unsent text; send or clear it in the worker's pane"
				readySince = time.Time{}
			} else if v.Ready() {
				now := l.now()
				if readySince.IsZero() {
					readySince = now
				}
				held := now.Sub(readySince)
				if held >= l.settle && (last.TurnSeen || last.Report || held >= l.quiet) {
					last.State, last.Reason = readyStateSettled, ""
					return finishWait(last, start, now)
				}
				last.Reason = "at the prompt for " + held.Truncate(time.Second).String()
			} else {
				readySince = time.Time{}
			}
		}
		if l.now().Sub(start)+l.interval > l.timeout {
			if last.State == ready.StateReady {
				// At the prompt but not settled: never report "ready" on a
				// failed wait.
				last.State = ready.StateNotReady
			}
			last.Reason = fmt.Sprintf("not settled after %s: %s", l.timeout, last.Reason)
			return finishWait(last, start, l.now())
		}
		if err := l.sleep(ctx, l.interval); err != nil {
			last.Reason = "wait canceled: " + err.Error()
			return finishWait(last, start, l.now())
		}
	}
}

func finishWait(r waitResult, start, end time.Time) waitResult {
	r.WaitedMS = end.Sub(start).Milliseconds()
	return r
}

// reportWait prints the result and returns exit 1 for anything but settled.
func reportWait(cmd *cobra.Command, r waitResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, r); err != nil {
			return err
		}
		if r.State == readyStateSettled {
			return nil
		}
		return newSilentCodedError(1)
	}
	if r.State == readyStateSettled {
		_, err := fmt.Fprintf(out, "%s: settled\n", r.Name)
		return err
	}
	return WithExitCode(fmt.Errorf("worker %s is %s: %s", r.Name, r.State, termsafe.SafeLineMax(r.Reason, 300)), 1)
}
