package cli

import (
	"context"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
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
func workerLaunchResult(ctx context.Context, adapter any, attempt workerAttempt, top string) launchResult {
	ref := attempt.launched.ref
	w := &launchedWorkerResult{Repo: top, Worktree: attempt.launched.worktree}
	var name, harness string
	if r := attempt.row; r != nil {
		name, harness = r.Name, r.Harness
		w.Branch, w.SessionID, w.Transcript = r.Branch, r.SessionID, r.Transcript
		if r.Brief != nil {
			w.Marker = r.Brief.Marker
		}
	}
	res := newLaunchResult(ctx, adapter, ref, "herdr", name, top, harness)
	res.Worker = w
	return res
}
