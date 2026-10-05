package herdradapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
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
	ws, _, state, cause := a.locate(ctx, ref)
	switch state {
	case locateFound:
	case locateAbsent:
		return ready.Screen{}, ErrWorkerGone
	case locateMismatch, locateUnreadable, locateInvalid:
		return ready.Screen{}, fmt.Errorf("%w: %v", ErrScreenUnreadable, cause)
	}
	identity, err := ref.HerdrIdentity()
	if err != nil {
		return ready.Screen{}, fmt.Errorf("%w: %w", ErrScreenUnreadable, err)
	}
	pane := identity.Pane()
	if pane == "" || !strings.HasPrefix(pane, ws+":") {
		return ready.Screen{}, fmt.Errorf("%w: the reference names pane %q, which is not in workspace %q",
			ErrScreenUnreadable, pane, ws)
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

type paneGetReply struct {
	Result *struct {
		Pane *struct {
			PaneID      string  `json:"pane_id"`
			WorkspaceID string  `json:"workspace_id"`
			Agent       *string `json:"agent"`
			AgentStatus string  `json:"agent_status"`
		} `json:"pane"`
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
	s := ready.Screen{Status: p.AgentStatus}
	if p.Agent != nil {
		s.Agent = *p.Agent
	}
	return s, nil
}
