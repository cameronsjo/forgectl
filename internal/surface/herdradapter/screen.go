package herdradapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// screenLines is how many rows `surface ready` reads. herdr's visible source
// is the viewport, so this only bounds a very tall pane.
const screenLines = "200"

var (
	// ErrWorkerGone reports a worker whose workspace no longer exists on the
	// server its reference was bound to.
	ErrWorkerGone = errors.New("herdr: the worker's workspace is gone")
	// ErrScreenUnreadable reports a read that cannot say what the worker's
	// pane shows: herdr failed, answered for another object, or truncated
	// the reply. It is never a verdict about the worker.
	ErrScreenUnreadable = errors.New("herdr: the worker's pane could not be read")
)

// WorkerScreen reads a worker's root pane: its visible text and herdr's
// agent status, for the readiness predicates.
//
// It reads only a pane forgectl owns. The workspace is located through the
// same ownership check Close and Probe use, the pane must be the root pane
// the create response named, and herdr's own record of the pane must place
// it in that workspace. Anything else is ErrScreenUnreadable, or
// ErrWorkerGone for a workspace that is provably absent.
func (a *Adapter) WorkerScreen(ctx context.Context, ref backend.Ref) (ready.Screen, error) {
	pane, ws, err := a.ownedPane(ctx, ref)
	if err != nil {
		return ready.Screen{}, err
	}
	status, err := a.paneStatus(ctx, pane, ws)
	if err != nil {
		return ready.Screen{}, err
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrScreenRead,
		exec.MustFixed("pane"),
		exec.MustFixed("read"),
		exec.Opaque(pane),
		exec.MustFixed("--source"),
		exec.MustFixed("visible"),
		exec.MustFixed("--format"),
		exec.MustFixed("text"),
		exec.MustFixed("--lines"),
		exec.MustFixed(screenLines),
	))
	if runErr != nil {
		c := a.classifyRunError(runErr, res)
		return ready.Screen{}, fmt.Errorf("%w: pane read: %v", ErrScreenUnreadable, c)
	}
	text, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return ready.Screen{}, fmt.Errorf("%w: pane read output was truncated", ErrScreenUnreadable)
	}
	status.Text = strings.TrimRight(string(text), "\n")
	return status, nil
}

// ownedPane locates the worker's workspace through the ownership check and
// returns its root pane, which must sit in that workspace.
func (a *Adapter) ownedPane(ctx context.Context, ref backend.Ref) (pane, ws string, err error) {
	ws, _, state, cause := a.locate(ctx, ref)
	switch state {
	case locateFound:
	case locateAbsent:
		return "", "", ErrWorkerGone
	case locateMismatch, locateUnreadable, locateInvalid:
		return "", "", fmt.Errorf("%w: %v", ErrScreenUnreadable, cause)
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrScreenUnreadable, err)
	}
	pane = identity.Pane()
	if pane == "" || !strings.HasPrefix(pane, ws+":") {
		return "", "", fmt.Errorf("%w: the reference names pane %q, which is not in workspace %q",
			ErrScreenUnreadable, pane, ws)
	}
	return pane, ws, nil
}

// ErrSendFailed reports a write to a worker's pane that herdr did not
// confirm. Whether any of it reached the pane is unknown.
var ErrSendFailed = errors.New("herdr: the send to the worker's pane failed")

// TypeText types text into a worker's root pane without pressing Enter.
//
// It re-runs the ownership check first, so it types only into the root pane
// of a workspace forgectl owns. herdr's send-text takes the text as its last
// operand and does not honour `--` (it types the `--`), so text that starts
// with '-' cannot be sent; the sensitive runner refuses it as an option-shaped
// operand, and worker.CheckBrief refuses it earlier with a clearer message.
// The text travels sealed: no log or error renders it.
func (a *Adapter) TypeText(ctx context.Context, ref backend.Ref, text string) error {
	pane, _, err := a.ownedPane(ctx, ref)
	if err != nil {
		return err
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrSendText,
		exec.MustFixed("pane"),
		exec.MustFixed("send-text"),
		exec.Opaque(pane),
		exec.Opaque(text),
	))
	if runErr != nil {
		return fmt.Errorf("%w: send-text: %v", ErrSendFailed, a.classifyRunError(runErr, res))
	}
	return nil
}

// PressEnter sends one Enter key to a worker's root pane, after the same
// ownership check as TypeText.
func (a *Adapter) PressEnter(ctx context.Context, ref backend.Ref) error {
	pane, _, err := a.ownedPane(ctx, ref)
	if err != nil {
		return err
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrSendKeys,
		exec.MustFixed("pane"),
		exec.MustFixed("send-keys"),
		exec.Opaque(pane),
		exec.MustFixed("Enter"),
	))
	if runErr != nil {
		return fmt.Errorf("%w: send-keys: %v", ErrSendFailed, a.classifyRunError(runErr, res))
	}
	return nil
}

// paneGetReply is `pane get`'s envelope around the shared herdr pane type, so
// the adapter and internal/herdr decode one wire shape.
type paneGetReply struct {
	Result *struct {
		Pane *herdr.Pane `json:"pane"`
	} `json:"result"`
}

// paneStatus reads herdr's record of pane and checks it is the pane asked
// for, in workspace ws.
func (a *Adapter) paneStatus(ctx context.Context, pane, ws string) (ready.Screen, error) {
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrPaneStatus,
		exec.MustFixed("pane"),
		exec.MustFixed("get"),
		exec.Opaque(pane),
	))
	if runErr != nil {
		c := a.classifyRunError(runErr, res)
		return ready.Screen{}, fmt.Errorf("%w: pane get: %v", ErrScreenUnreadable, c)
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		return ready.Screen{}, fmt.Errorf("%w: pane get output was truncated", ErrScreenUnreadable)
	}
	var r paneGetReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return ready.Screen{}, fmt.Errorf("%w: pane get: %w", ErrScreenUnreadable, err)
	}
	if r.Result == nil || r.Result.Pane == nil {
		return ready.Screen{}, fmt.Errorf("%w: pane get reply has no pane", ErrScreenUnreadable)
	}
	p := r.Result.Pane
	if p.PaneID != pane || p.WorkspaceID != ws {
		return ready.Screen{}, fmt.Errorf("%w: asked for pane %q in %q, herdr answered for %q in %q",
			ErrScreenUnreadable, pane, ws, p.PaneID, p.WorkspaceID)
	}
	return ready.Screen{Agent: p.Agent, Status: p.AgentStatus}, nil
}
