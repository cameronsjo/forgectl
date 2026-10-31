package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// launchResult is what a real `surface launch --json` prints: what the
// launch created, so a caller does not re-read `surface list` to learn it
// (forgectl#1239). Additive changes only (ADR-0008 rule 2). It names nothing
// the manager does not already know: no harness path, argument, environment
// or brief.
type launchResult struct {
	Surface string `json:"surface"`
	Name    string `json:"name"`
	Target  string `json:"target"`
	Harness string `json:"harness"`
	// Ref is the line a launch without --json prints: the backend and the
	// recovery tag.
	Ref string `json:"ref"`
	// The herdr ids, for a herdr launch. TerminalID is read after the launch
	// and is absent when that read failed; a pane id renumbers when the pane
	// moves, a terminal id does not.
	WorkspaceID string `json:"workspace_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	TerminalID  string `json:"terminal_id,omitempty"`
	// Worker is set for a --worktree launch.
	Worker *launchedWorkerResult `json:"worker,omitempty"`
}

// launchedWorkerResult is the worker half of launchResult, from the ledger
// row the launch wrote.
type launchedWorkerResult struct {
	Repo       string `json:"repo"`
	Worktree   string `json:"worktree"`
	Branch     string `json:"branch"`
	SessionID  string `json:"session_id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	// Marker is the first brief's REPORT marker, when --brief was given.
	Marker string `json:"marker,omitempty"`
}

// terminalReader is implemented by the herdr adapter.
type terminalReader interface {
	TerminalID(ctx context.Context, ref backend.Ref) (string, error)
}

// terminalReadTimeout bounds the post-launch terminal_id read. The read is
// a nicety: the launch already succeeded.
const terminalReadTimeout = 10 * time.Second

// newLaunchResult fills the ids a reference carries and, when adapter can
// read it, the pane's terminal_id. A failed read leaves terminal_id out.
func newLaunchResult(ctx context.Context, adapter any, ref backend.Ref, surfaceName, name, target, harness string) launchResult {
	res := launchResult{Surface: surfaceName, Name: name, Target: target, Harness: harness, Ref: ref.String()}
	// An hc reference already holds the terminal's address, which is its
	// terminal_id: nothing more to read.
	if hc, err := ref.HcIdentity(); err == nil {
		res.TerminalID = hc.Terminal()
		return res
	}
	id, err := ref.HerdrIdentity()
	if err != nil {
		return res
	}
	res.WorkspaceID, res.TabID, res.PaneID = id.Workspace(), id.Tab(), id.Pane()
	if tr, ok := adapter.(terminalReader); ok {
		ctx, cancel := context.WithTimeout(ctx, terminalReadTimeout)
		defer cancel()
		if t, err := tr.TerminalID(ctx, ref); err == nil {
			res.TerminalID = t
		}
	}
	return res
}

// workerLaunchResult is a worker launch's result, from the row it wrote.
// name is the worker's name as launched. When that row could not be read
// back, the result still carries what the launch itself returned (the ids,
// worktree and name) and the error says the rest is missing.
func workerLaunchResult(ctx context.Context, adapter any, attempt workerAttempt, name, top string) (launchResult, error) {
	ref := attempt.launched.ref
	w := &launchedWorkerResult{Repo: top, Worktree: attempt.launched.worktree}
	var harness string
	var err error
	switch r := attempt.row; {
	case attempt.rowErr != nil:
		err = fmt.Errorf("the worker started, but its ledger row could not be read back for --json: %w", attempt.rowErr)
	case r == nil:
		err = errors.New("the worker started, but this launch's ledger row was not found for --json")
	default:
		harness = r.Harness
		w.Branch, w.SessionID, w.Transcript = r.Branch, r.SessionID, r.Transcript
		if r.Brief != nil {
			w.Marker = r.Brief.Marker
		}
	}
	// The surface is the backend the reference names (herdr or hc).
	res := newLaunchResult(ctx, adapter, ref, ref.Kind().String(), name, top, harness)
	res.Worker = w
	return res, err
}

// launchStarted reports whether a worker attempt got as far as a running
// surface: its reference names a backend.
func launchStarted(a workerAttempt) bool {
	return a.launched.ref.Kind() != backend.KindUnspecified
}

// runningWorkerError adds where a running worker is to the error of a
// launch whose bookkeeping failed after the start, so a caller can still
// find it (and close it).
func runningWorkerError(err error, ref backend.Ref) error {
	where := ref.String()
	if id, ierr := ref.HerdrIdentity(); ierr == nil {
		where = fmt.Sprintf("%s, herdr workspace %s, pane %s", where, id.Workspace(), id.Pane())
	}
	return fmt.Errorf("%w (the worker is running: %s)", err, termsafe.SafeLineMax(where, 300))
}
