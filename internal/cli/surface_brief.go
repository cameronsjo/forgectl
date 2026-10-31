package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface brief <name> <text|@file>` types a follow-up brief into a worker
// that is at its input prompt, and confirms the turn started. A worker's
// first brief does not come through here: `surface launch --brief` passes it
// as the harness's prompt argument, so it is never typed.

const (
	// defaultBriefReadback is how long brief waits for the typed text to
	// show in the input box.
	defaultBriefReadback = 3 * time.Second
	// defaultBriefStart is how long brief waits for herdr to report the
	// turn working after Enter. Measured at under 0.5 s on herdr 0.9.1.
	defaultBriefStart = 5 * time.Second
	briefPollInterval = 200 * time.Millisecond
)

// Brief outcomes. Additive changes only (ADR-0008 rule 2).
const (
	// briefSent: Enter was sent and herdr reported the turn working.
	briefSent = "sent"
	// briefRefused: nothing was submitted. Text may sit unsent in the input
	// box when Step is "readback".
	briefRefused = "refused"
	// briefUnconfirmed: Enter was sent, but no working turn was seen.
	briefUnconfirmed = "unconfirmed"
)

// briefResult is what `surface brief --json` prints.
type briefResult struct {
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	Outcome  string `json:"outcome"`
	Step     string `json:"step,omitempty"`
	Marker   string `json:"marker,omitempty"`
	Count    int    `json:"count,omitempty"`
	Blocking string `json:"blocking,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type briefOptions struct {
	Repo string
	// Surface is the worker surface: herdr (the default) or hc.
	Surface  string
	Name     string
	Text     string
	Readback time.Duration
	Start    time.Duration
	JSON     bool
}

func newSurfaceBriefCmd(deps module.Deps) *cobra.Command {
	opts := briefOptions{}
	cmd := &cobra.Command{
		Use:   "brief <name> <text|@file>",
		Short: "Type a follow-up brief into a worker and confirm its turn started",
		Long: `brief types a follow-up brief into a worker that is at its input prompt.

A worker's first brief belongs on its launch (surface launch --brief), which
passes it as the harness's prompt argument so nothing is typed. brief is for
the briefs after that.

It sends in two steps and never answers a dialog: it types the text without
Enter, reads the screen back until the text sits in the input box with no
dialog showing, records the brief in the ledger, and only then sends Enter as
its own call. It then waits for herdr to report the turn working. A dialog
can still appear between the read-back and Enter; the readiness checks read
signals the worker itself can set, so they guard against accidents, not a
worker trying to look ready. A brief must never carry an approval.

With --surface hc there is no read-back: hc send pastes and submits in one
call, and hc cannot type without submitting. brief refuses unless hc reports
the worker idle or done and the screen shows the harness's input prompt,
empty, with no dialog. Both checks read the worker's own terminal output,
so they catch an honest dialog but not a forged screen. So, right before it
pastes, brief sends one Escape, which closes or declines any dialog open at
that moment (Claude Code, Codex and pi all treat Escape as cancel), and a
worker's prompt is never in vim mode (a vim mode line under the input box is
refused). Escape also interrupts a running turn, so brief re-reads the
worker first and sends no key if its status or turn moved. The limit: a
dialog drawn after that Escape and before the paste is not covered. It
records the brief, then sends
(with --if idle when it was idle, so a turn that started in between refuses
the send). It never passes --force, so hc itself refuses a blocked worker. hc's
refusal reports refused; any other send failure reports unconfirmed.
--readback-timeout has no step under hc.

Each brief gets a new random marker and asks the worker to end its final
message with a REPORT line naming it; read it with surface read --report.

A typed brief is one line of at most 600 characters with the report
instruction: Claude Code turns longer typed text into a paste placeholder,
and a newline would be Enter. Put a long brief in a file in the worktree and
brief the worker to read it. @file reads the brief from a file. A typed brief
may not start with - / ! # ? @ or &, which herdr reads as an option or the
harness reads as a mode or menu.

brief refuses inside a drain worker (FORGECTL_DRAIN_WORKER set), so one
worker does not type into another by accident.

Exit 0: sent and working. Exit 1: refused (not at its prompt, a dialog, text
already in the input box, a read-back that did not match) or unconfirmed.
--json's outcome tells the two apart, and only refused is safe to retry:
refused submitted nothing, while unconfirmed already sent Enter, so a retry
could send the brief twice. After unconfirmed, check the worker with
surface ready or surface read before sending again.
Exit 2: a usage or setup error.

  forgectl surface brief fix-login "Now run the full test suite and fix failures."
  forgectl surface brief fix-login @next.txt --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name, opts.Text = args[0], args[1]
			return runSurfaceBrief(cmd, deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Repo, "repo", ".", "repository the worker was launched from (project name or path)")
	addWorkerSurfaceFlag(cmd, &opts.Surface)
	cmd.Flags().DurationVar(&opts.Readback, "readback-timeout", defaultBriefReadback, "how long to wait for the typed text to show")
	cmd.Flags().DurationVar(&opts.Start, "start-timeout", defaultBriefStart, "how long to wait for the turn to start after Enter")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, `print {"name","harness","outcome","step","marker","count","blocking","reason"} as JSON`)
	return cmd
}

func runSurfaceBrief(cmd *cobra.Command, deps module.Deps, opts briefOptions) error {
	// A worker must not type into a sibling worker's prompt.
	if err := refuseInDrainWorker(os.Getenv, "surface brief"); err != nil {
		return err
	}
	if opts.Readback <= 0 || opts.Start <= 0 {
		return WithExitCode(errors.New("--readback-timeout and --start-timeout must be positive"), exitUsage)
	}
	text, err := readBriefArg(opts.Text)
	if err != nil {
		return WithExitCode(err, exitUsage)
	}
	if err := worker.CheckBrief(text, worker.ViaTyped); err != nil {
		return WithExitCode(err, exitUsage)
	}
	w, err := openWorker(cmd.Context(), cmd.ErrOrStderr(), deps, opts.Repo, opts.Name, opts.Surface)
	if err != nil {
		return err
	}
	marker, err := worker.NewMarker()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.Readback+opts.Start+5*time.Second)
	defer cancel()
	if w.hc != nil {
		res := hcBriefLocked(ctx, w.led.LockBrief, w.row, hcBriefSteps{
			status: func(ctx context.Context) (hcadapter.Worker, error) { return w.hc.WorkerStatus(ctx, w.ref) },
			screen: func(ctx context.Context) (string, error) {
				s, err := w.hc.WorkerScreen(ctx, w.ref)
				return s.Text, err
			},
			promptShown: func(text string) ready.Verdict { return w.table.EvaluateText(w.row.Harness, text) },
			escape: func(ctx context.Context, checked hcadapter.Worker) error {
				return w.hc.PressEscape(ctx, w.ref, checked)
			},
			send: func(ctx context.Context, s string, checked hcadapter.Worker) (hcadapter.SendResult, error) {
				return w.hc.SendBrief(ctx, w.ref, s, checked)
			},
			record: w.recordBrief,
			now:    time.Now,
			sleep:  resume.SleepContext,
			start:  opts.Start,
			// --readback-timeout has no step under hc: hc's send pastes and
			// submits in one call.
			interval: briefPollInterval,
		}, text, marker)
		res.Name, res.Harness = w.row.Name, w.row.Harness
		return reportBrief(cmd, res, opts.JSON)
	}
	res := sendBrief(ctx, briefSteps{
		read:     w.read,
		evaluate: w.evaluate,
		typeText: func(ctx context.Context, s string) error { return w.herdr.TypeText(ctx, w.ref, s) },
		enter:    func(ctx context.Context) error { return w.herdr.PressEnter(ctx, w.ref) },
		record:   w.recordBrief,
		now:      time.Now,
		sleep:    resume.SleepContext,
		readback: opts.Readback,
		start:    opts.Start,
		interval: briefPollInterval,
	}, text, marker)
	res.Name, res.Harness = w.row.Name, w.row.Harness
	return reportBrief(cmd, res, opts.JSON)
}

// briefSteps is the send, with its I/O and clock injected so each refusal
// can be tested without herdr.
type briefSteps struct {
	read     func(context.Context) (ready.Screen, error)
	evaluate func(ready.Screen) ready.Verdict
	typeText func(context.Context, string) error
	enter    func(context.Context) error
	// record writes the brief to the ledger and returns its count.
	record   func(worker.Brief) (int, error)
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	readback time.Duration
	start    time.Duration
	interval time.Duration
}

// sendBrief types text with its report instruction, confirms it on screen,
// records it, presses Enter, and confirms the turn started.
//
// Enter is sent only after a read-back shows the whole brief in the input box
// and the predicates find the harness at its prompt with no dialog. Any other
// outcome before that point refuses with Enter unsent.
func sendBrief(ctx context.Context, s briefSteps, text, marker string) briefResult {
	refuse := func(step, reason string, v ready.Verdict) briefResult {
		return briefResult{Outcome: briefRefused, Step: step, Blocking: v.Blocking, Reason: reason}
	}

	screen, err := s.read(ctx)
	if err != nil {
		return refuse("check", "the worker's pane could not be read: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}
	v := s.evaluate(screen)
	if !v.Ready() {
		return refuse("check", "the worker is not at its input prompt: "+v.Reason, v)
	}
	if v.Input != "" {
		return refuse("check", "the worker's input box already holds text; clear it in the worker's pane first", v)
	}

	composed := worker.Compose(text, marker, worker.ViaTyped)
	if err := s.typeText(ctx, composed); err != nil {
		return refuse("type", "typing the brief failed, so part of it may sit unsent in the input box: "+
			termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}

	want := strings.Join(strings.Fields(composed), " ")
	start := s.now()
	for {
		screen, err := s.read(ctx)
		if err == nil {
			v = s.evaluate(screen)
			if v.State == ready.StateBlocked {
				return refuse("readback", "a dialog appeared after the brief was typed; Enter was not sent, and the brief sits unsent in the input box", v)
			}
			if v.Ready() && v.Input == want {
				break
			}
		}
		if s.now().Sub(start)+s.interval > s.readback {
			reason := "the typed brief did not show in the input box as typed; Enter was not sent, and the text sits unsent in the input box"
			if err != nil {
				reason += " (last read failed: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen) + ")"
			}
			return refuse("readback", reason, v)
		}
		if err := s.sleep(ctx, s.interval); err != nil {
			return refuse("readback", "canceled before Enter: "+err.Error(), v)
		}
	}

	// The marker is recorded before Enter, so a worker whose turn started is
	// never missing the marker its report will carry.
	count, err := s.record(worker.Brief{Marker: marker, SentAt: s.now().UTC(), Via: worker.ViaTyped})
	if err != nil {
		return refuse("record", "the brief could not be recorded in the worker ledger, so Enter was not sent: "+
			termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}
	// One more read right before Enter: the read-back and the ledger write
	// leave a window in which a dialog could draw, and Enter would answer it.
	if screen, err := s.read(ctx); err != nil {
		return refuse("enter-check", "the last read before Enter failed, so Enter was not sent; the brief sits unsent in the input box and is recorded in the ledger: "+
			termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	} else if v = s.evaluate(screen); !v.Ready() || v.Input != want {
		return refuse("enter-check", "the screen changed after the read-back, so Enter was not sent; the brief sits unsent in the input box and is recorded in the ledger", v)
	}
	if err := s.enter(ctx); err != nil {
		return briefResult{Outcome: briefUnconfirmed, Step: "enter", Marker: marker, Count: count,
			Reason: "sending Enter failed, and whether it reached the pane is unknown: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
	}

	start = s.now()
	var last string
	for {
		screen, err := s.read(ctx)
		switch {
		case err != nil:
			last = "read failed: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
		case screen.Status == "working":
			return briefResult{Outcome: briefSent, Marker: marker, Count: count}
		default:
			v = s.evaluate(screen)
			if v.State == ready.StateBlocked {
				return briefResult{Outcome: briefUnconfirmed, Step: "start", Marker: marker, Count: count, Blocking: v.Blocking,
					Reason: "Enter was sent, then the worker showed a dialog instead of starting a turn"}
			}
			last = fmt.Sprintf("herdr reports agent status %q", screen.Status)
		}
		if s.now().Sub(start)+s.interval > s.start {
			return briefResult{Outcome: briefUnconfirmed, Step: "start", Marker: marker, Count: count,
				Reason: fmt.Sprintf("Enter was sent, but no working turn was seen within %s: %s", s.start, last)}
		}
		if err := s.sleep(ctx, s.interval); err != nil {
			return briefResult{Outcome: briefUnconfirmed, Step: "start", Marker: marker, Count: count,
				Reason: "canceled after Enter: " + err.Error()}
		}
	}
}

// reportBrief prints the result and returns exit 1 for anything but sent.
func reportBrief(cmd *cobra.Command, r briefResult, asJSON bool) error {
	out := cmd.OutOrStdout()
	if asJSON {
		if err := writeJSON(out, r); err != nil {
			return err
		}
		if r.Outcome == briefSent {
			return nil
		}
		return newSilentCodedError(1)
	}
	if r.Outcome == briefSent {
		_, err := fmt.Fprintf(out, "%s: brief %d sent, marker %s\n", r.Name, r.Count, r.Marker)
		return err
	}
	return WithExitCode(fmt.Errorf("brief to %s %s at %s: %s", r.Name, r.Outcome, r.Step, termsafe.SafeLineMax(r.Reason, 300)), exitFailed)
}

// readBriefArg returns arg, or the contents of the file it names after '@'.
// The file must be a regular file of at most worker.MaxLaunchBrief bytes;
// one trailing newline run is dropped.
func readBriefArg(arg string) (string, error) {
	path, ok := strings.CutPrefix(arg, "@")
	if !ok {
		return arg, nil
	}
	// Stat before open: opening a FIFO blocks until a writer appears.
	before, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("brief file: %w", err)
	}
	if !before.Mode().IsRegular() {
		return "", fmt.Errorf("brief file %s is not a regular file", termsafe.QuotePath(path))
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the brief file
	if err != nil {
		return "", fmt.Errorf("brief file: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error loses nothing
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("brief file: %w", err)
	}
	if !os.SameFile(before, info) {
		return "", fmt.Errorf("brief file %s changed while it was opened", termsafe.QuotePath(path))
	}
	data, err := io.ReadAll(io.LimitReader(f, worker.MaxLaunchBrief+1))
	if err != nil {
		return "", fmt.Errorf("brief file: %w", err)
	}
	if len(data) > worker.MaxLaunchBrief {
		return "", fmt.Errorf("brief file %s is over %d bytes", termsafe.QuotePath(path), worker.MaxLaunchBrief)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// hcBriefLocked runs one hc brief under the worker's brief lock
// (worker.Ledger.LockBrief), so two briefs to one worker cannot both read
// the last brief's time before either records it and send their Escapes
// inside the harness's double-press window. The spacing reads the row as it
// is under the lock, and a row that changed since the worker was opened (a
// close or a relaunch under the name) refuses.
func hcBriefLocked(ctx context.Context, lock func(context.Context, string) (func(), worker.Row, error), opened worker.Row, s hcBriefSteps, text, marker string) briefResult {
	release, row, err := lock(ctx, opened.Name)
	if err != nil {
		return briefResult{Outcome: briefRefused, Step: "check",
			Reason: "the worker's brief lock was not taken, so nothing was sent: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
	}
	defer release()
	if !worker.SameRow(opened)(row) {
		return briefResult{Outcome: briefRefused, Step: "check", Reason: "the worker's ledger row changed since it was read, so nothing was sent"}
	}
	s.lastTyped = lastTypedBrief(row)
	return sendBriefHC(ctx, s, text, marker)
}

// lastTypedBrief is when row's last brief was recorded, when it was typed
// into the worker (a launch brief sent no Escape), or the zero time.
func lastTypedBrief(row worker.Row) time.Time {
	if row.Brief == nil || row.Brief.Via != worker.ViaTyped {
		return time.Time{}
	}
	return row.Brief.SentAt
}

// recordBrief writes b as the worker's latest brief and returns its count.
func (w *openedWorker) recordBrief(b worker.Brief) (int, error) {
	err := w.led.Update(w.row.Name, func(r *worker.Row) {
		b.Count = 1
		if r.Brief != nil {
			b.Count = r.Brief.Count + 1
		}
		r.Brief = &b
	})
	return b.Count, err
}
