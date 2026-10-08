package herdradapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// ErrPaneStateFailed reports a pane-state change herdr did not confirm.
var ErrPaneStateFailed = errors.New("herdr: the worker's pane state was not changed")

// ReportBlocked puts a worker's root pane in herdr's needs-you state under
// the drain's source, with message shown on it.
//
// The pane comes only from ref, and only after the checks a write makes
// (writablePane): the workspace carries forgectl's ownership marker, the
// pane is the root pane the create response named, and herdr places it in
// that workspace. The command carries this adapter's session pin. Nothing
// here reads the calling process's own HERDR_PANE_ID.
func (a *Adapter) ReportBlocked(ctx context.Context, ref backend.Ref, message string) error {
	pane, err := a.writablePane(ctx, ref)
	if err != nil {
		return err
	}
	args, err := herdr.ReportBlockedArgs(herdr.SourceDrain, pane, message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPaneStateFailed, err)
	}
	return a.paneAgent(ctx, "report-agent", args)
}

// ReleaseBlocked clears what ReportBlocked set on a worker's root pane,
// after the same checks.
func (a *Adapter) ReleaseBlocked(ctx context.Context, ref backend.Ref) error {
	pane, err := a.writablePane(ctx, ref)
	if err != nil {
		return err
	}
	args, err := herdr.ReleaseAgentArgs(herdr.SourceDrain, pane)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPaneStateFailed, err)
	}
	return a.paneAgent(ctx, "release-agent", args)
}

func (a *Adapter) paneAgent(ctx context.Context, verb string, args []exec.Arg) error {
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrPaneAgent, args...))
	if runErr != nil {
		return fmt.Errorf("%w: %s: %v", ErrPaneStateFailed, verb, a.classifyRunError(runErr, res))
	}
	return nil
}
