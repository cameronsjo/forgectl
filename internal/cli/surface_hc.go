package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// herdr-core (hc) workers.
//
// hc is its own backend: `--surface hc` names it, and the worker verbs that
// support it take `--surface herdr|hc`, defaulting to herdr so every existing
// command keeps its meaning. See openspec/changes/add-hc-surface/design.md.

// Worker surfaces, as the --surface flag spells them.
const (
	workerSurfaceHerdr = "herdr"
	workerSurfaceHc    = "hc"
)

// errUnknownWorkerSurface refuses a --surface the worker verbs do not drive.
var errUnknownWorkerSurface = errors.New("--surface must be herdr or hc")

// addWorkerSurfaceFlag adds the worker verbs' --surface flag.
func addWorkerSurfaceFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "surface", workerSurfaceHerdr,
		"terminal manager the worker was launched in: herdr or hc (herdr-core)")
}

// checkWorkerSurface refuses a --surface value outside herdr and hc.
func checkWorkerSurface(name string) error {
	if name != workerSurfaceHerdr && name != workerSurfaceHc {
		return WithExitCode(errUnknownWorkerSurface, exitUsage)
	}
	return nil
}

// newHcAdapter resolves hc on PATH and builds the adapter, for the reason the
// other backends resolve theirs here: the sensitive runner needs an absolute
// path chosen before any command runs.
func newHcAdapter() (backend.Adapter, error) {
	path, err := osexec.LookPath("hc")
	if err != nil {
		return nil, fmt.Errorf("%w: hc (herdr-core) not found on PATH: %w", errBackendUnavailable, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve hc path: %w", errBackendUnavailable, termsafe.Error(err))
	}
	a, err := hcadapter.New(exec.NewOSSensitiveRunner(), abs, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBackendUnavailable, err)
	}
	return a, nil
}

// checkHcName refuses a display name hc would refuse, before anything is
// created. The adapter cannot check it: the name is sealed by then.
func checkHcName(name string) error {
	if err := hcadapter.CheckName(name); err != nil {
		return WithExitCode(fmt.Errorf("--name %q: %w", name, err), exitUsage)
	}
	return nil
}

// workerIsGone reports a worker whose workspace or terminal is provably gone,
// under either backend.
func workerIsGone(err error) bool {
	return errors.Is(err, herdradapter.ErrWorkerGone) || errors.Is(err, hcadapter.ErrWorkerGone)
}

// goneReason names what is gone, for the backend that said so.
func goneReason(err error) string {
	if errors.Is(err, hcadapter.ErrWorkerGone) {
		return "the worker's hc terminal is gone"
	}
	return "the worker's herdr workspace is gone"
}

// surfaceNoun names what a reference points at, for messages: an hc
// terminal, or another backend's workspace.
func surfaceNoun(ref backend.Ref) string {
	if ref.Kind() == backend.KindHc {
		return "terminal"
	}
	return "workspace"
}

// hcScreenReader reads an hc worker as a ready.Screen. Status and agent come
// from hc's OSC 7501 report, text from `hc read`. With live set, a worker
// whose program has exited reads as gone, which ends ready, wait and brief at
// once; read itself still shows a finished worker's last screen.
func hcScreenReader(hc *hcadapter.Adapter, ref backend.Ref, live bool) func(context.Context) (ready.Screen, error) {
	return func(ctx context.Context) (ready.Screen, error) {
		w, err := hc.WorkerScreen(ctx, ref)
		if err != nil {
			return ready.Screen{}, err
		}
		if live && !w.Live {
			return ready.Screen{}, fmt.Errorf("%w: its program has exited", hcadapter.ErrWorkerGone)
		}
		return ready.Screen{Text: w.Text, Agent: w.App, Status: w.Status}, nil
	}
}

// hcBlocking is the Blocking name an hc worker's blocked status reports.
const hcBlocking = "hc blocked status"

// evaluateHcStatus is the readiness verdict for an hc worker, from its status
// alone. hc's manual says not to guess status from screen text, so the herdr
// prompt predicates are not consulted. The input box's contents are unknown,
// so Input is always empty.
func evaluateHcStatus(s ready.Screen) ready.Verdict {
	switch s.Status {
	case hcadapter.StatusIdle, hcadapter.StatusDone:
		return ready.Verdict{State: ready.StateReady}
	case hcadapter.StatusBlocked:
		return ready.Verdict{State: ready.StateBlocked, Blocking: hcBlocking,
			Reason: "hc reports the worker blocked: it needs a person (a permission prompt or a question) in its terminal"}
	case "":
		return ready.Verdict{State: ready.StateNotReady, Reason: "the worker has reported no status to hc yet"}
	}
	return ready.Verdict{State: ready.StateNotReady, Reason: fmt.Sprintf("hc reports status %q, want idle or done", s.Status)}
}

// evaluateHcWorker is ready and wait's verdict for an hc worker: hc's status
// first, then the screen, which must show the harness's input prompt and no
// dialog, the same two checks brief runs before it sends. Both come from the
// worker's own terminal output, so they catch an honest dialog; anything that
// can write to that terminal can forge both, and a verdict here is never a
// guarantee that no dialog is open (sendBriefHC's Escape covers that).
func evaluateHcWorker(table *ready.Table, harness string, s ready.Screen) ready.Verdict {
	if v := evaluateHcStatus(s); !v.Ready() {
		return v
	}
	return table.EvaluateText(harness, s.Text)
}

// vimModeLines are the mode indicators a vim-mode input box draws.
var vimModeLines = []string{"-- INSERT --", "-- NORMAL --", "-- VISUAL --"}

// vimModeShown reports whether the screen shows a vim mode indicator.
func vimModeShown(text string) bool {
	for _, m := range vimModeLines {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// hcBriefSteps is sendBriefHC's I/O, injected so each outcome is testable
// without hc.
type hcBriefSteps struct {
	status func(context.Context) (hcadapter.Worker, error)
	// screen reads the worker's screen text, and promptShown judges it with
	// the harness's prompt and dialog predicates (ready.Table.EvaluateText).
	screen      func(context.Context) (string, error)
	promptShown func(text string) ready.Verdict
	// escape sends one Escape to the worker, right before send, and refuses
	// when the worker's status or turn is no longer the checked one.
	escape func(ctx context.Context, checked hcadapter.Worker) error
	// send sends text; ifIdle asks hc to type only while still idle.
	send   func(ctx context.Context, text string, ifIdle bool) (hcadapter.SendResult, error)
	record func(worker.Brief) (int, error)
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	// start bounds the wait for the turn to begin after the send.
	start    time.Duration
	interval time.Duration
}

// sendBriefHC sends a typed brief to an hc worker.
//
// hc's send pastes and submits in one call, so there is no read-back step,
// and hc cannot type without submitting. Two checks run first: the status
// must be idle or done, and the screen must show the harness's input prompt,
// empty, with no dialog. Both come from the worker's own terminal output,
// so anything that can write to that terminal can forge both: three printed
// lines make an empty prompt appear under a real dialog. The checks stop an
// honest worker's dialog; they do not stop a forger.
//
// What keeps Enter off a dialog is the key sent right before the paste: one
// Escape (escape). Claude Code's Esc closes an open dialog and declines a
// permission prompt, Codex maps Esc to cancel on its approval prompts, and
// pi's Escape cancels a selection, so the first key any dialog open at that
// moment sees is a refusal. A worker's prompt is never in vim mode, where
// Esc would switch to NORMAL (worker settings pin editorMode "normal"), and a
// screen that shows a vim mode line is refused as a backstop. Escape also
// interrupts a running turn and hc keys has no idle guard, so the Escape
// re-reads the worker and refuses unless its status and turn are the checked
// ones. The limits: a dialog drawn after that Escape and before hc pastes is
// not covered, nor a turn that starts in the one round trip between that
// re-read and the key. hc also refuses a blocked terminal itself, and --force
// is never passed. The marker is recorded after the Escape and before the
// send, so a turn that started always has its marker in the ledger and a
// refused Escape leaves none.
func sendBriefHC(ctx context.Context, s hcBriefSteps, text, marker string) briefResult {
	refuse := func(step, reason string, v ready.Verdict) briefResult {
		return briefResult{Outcome: briefRefused, Step: step, Blocking: v.Blocking, Reason: reason}
	}
	before, err := s.status(ctx)
	if err != nil {
		return refuse("check", "the worker's terminal could not be read: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}
	if !before.Live {
		return refuse("check", "the worker's program has exited", ready.Verdict{})
	}
	if v := evaluateHcStatus(ready.Screen{Status: before.Status}); !v.Ready() {
		return refuse("check", "the worker is not waiting for input: "+v.Reason, v)
	}
	shown, err := s.screen(ctx)
	if err != nil {
		return refuse("check", "the worker's screen could not be read: "+termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}
	switch v := s.promptShown(shown); {
	case v.State == ready.StateBlocked:
		return refuse("check", "hc reports the worker "+before.Status+", but its screen shows a dialog: "+v.Reason, v)
	case !v.Ready():
		return refuse("check", "hc reports the worker "+before.Status+", but its screen does not show the input prompt: "+v.Reason, v)
	case v.Input != "":
		return refuse("check", "the worker's input box already holds text; clear it in the worker's terminal first", v)
	case vimModeShown(shown):
		return refuse("check", "the worker's screen shows a vim mode line, where the Escape sent before the paste would switch modes instead of closing a dialog; set editorMode to normal for this worker", v)
	}

	if err := s.escape(ctx, before); err != nil {
		return refuse("escape", "the Escape that closes any open dialog before the paste was not sent or failed, so nothing was typed or recorded: "+
			termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}
	count, err := s.record(worker.Brief{Marker: marker, SentAt: s.now().UTC(), Via: worker.ViaTyped})
	if err != nil {
		return refuse("record", "the brief could not be recorded in the worker ledger, so nothing was sent: "+
			termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	}
	sent, err := s.send(ctx, worker.Compose(text, marker, worker.ViaTyped), before.Status == hcadapter.StatusIdle)
	switch {
	case errors.Is(err, hcadapter.ErrSendRefused):
		return refuse("send", "hc refused the send and typed nothing; the brief is recorded in the ledger: "+
			termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen), ready.Verdict{})
	case err != nil:
		return briefResult{Outcome: briefUnconfirmed, Step: "send", Marker: marker, Count: count,
			Reason: "the send failed, and whether it reached the worker is unknown: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)}
	}
	if sent.Status == hcadapter.StatusWorking || sent.Turn > before.Turn {
		return briefResult{Outcome: briefSent, Marker: marker, Count: count}
	}

	start := s.now()
	var last string
	for {
		w, err := s.status(ctx)
		switch {
		case err != nil:
			last = "read failed: " + termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
		case w.Status == hcadapter.StatusWorking || w.Turn > before.Turn:
			return briefResult{Outcome: briefSent, Marker: marker, Count: count}
		case w.Status == hcadapter.StatusBlocked:
			return briefResult{Outcome: briefUnconfirmed, Step: "start", Marker: marker, Count: count, Blocking: hcBlocking,
				Reason: "the brief was sent, then hc reported the worker blocked instead of starting a turn"}
		default:
			last = fmt.Sprintf("hc reports status %q", w.Status)
		}
		if s.now().Sub(start)+s.interval > s.start {
			return briefResult{Outcome: briefUnconfirmed, Step: "start", Marker: marker, Count: count,
				Reason: fmt.Sprintf("the brief was sent, but no working turn was seen within %s: %s", s.start, last)}
		}
		if err := s.sleep(ctx, s.interval); err != nil {
			return briefResult{Outcome: briefUnconfirmed, Step: "start", Marker: marker, Count: count,
				Reason: "canceled after the send: " + err.Error()}
		}
	}
}
