package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// The drain's needs-you signal: one macOS notification naming the worker and
// the reason, and the worker's own herdr pane put in herdr's needs-you
// state. The pane comes from the worker's ledger ref on the drain's pinned
// herdr session, never from the drain's own HERDR_PANE_ID: the drain runs
// detached, and any pane it inherited is the operator's, not the worker's.

// drainPaneTimeout bounds the herdr half: the ownership checks and the
// pane-state change, together.
const drainPaneTimeout = 5 * time.Second

// workerPaneState is the herdr adapter's pane-state surface for one worker,
// reached by its ref after the adapter's ownership checks.
type workerPaneState interface {
	Session() string
	ReportBlocked(ctx context.Context, ref backend.Ref, message string) error
	ReleaseBlocked(ctx context.Context, ref backend.Ref) error
}

// needsYouNotifier is the drain's drainNotifier.
type needsYouNotifier struct {
	// session is the drain's pinned herdr session.
	session string
	// mac posts the macOS notification; nil sends none.
	mac *notify.Client
	// panes builds the herdr adapter for one call.
	panes func() (workerPaneState, error)
}

func newNeedsYouNotifier(run exec.Runner, session string) needsYouNotifier {
	return needsYouNotifier{session: session, mac: notify.New(run), panes: drainPanes}
}

// drainPanes is the real herdr adapter, resolved as the drain's probe
// resolves it.
func drainPanes() (workerPaneState, error) {
	adapter, err := newHerdrAdapter(io.Discard)
	if err != nil {
		return nil, err
	}
	herdr, ok := adapter.(*herdradapter.Adapter)
	if !ok {
		return nil, errors.New("forgectl: the herdr adapter has an unexpected type")
	}
	return herdr, nil
}

// needsYouText is the notification's title and body, and the message shown
// on the worker's pane.
func needsYouText(name, reason string) (title, body, pane string) {
	title = "forgectl: " + name + " needs you"
	body = reason
	if body == "" {
		body = "the worker is waiting"
	}
	return title, body, title + " — " + body
}

// NeedsYou sends the macOS notification, then marks the worker's pane. A
// pane that cannot be verified as the worker's is left alone; the macOS
// notification goes out either way. It returns what failed, for one event.
func (n needsYouNotifier) NeedsYou(ctx context.Context, row worker.QueueRow, led worker.Row, reason string) error {
	title, body, message := needsYouText(row.Name, reason)
	var errs []error
	if n.mac != nil && n.mac.Supported() {
		if err := sendMacSignal(ctx, n.mac.Notify, title, body); err != nil {
			errs = append(errs, fmt.Errorf("macOS notification: %w", err))
		}
	}
	if err := n.onPane(ctx, row, led, func(c context.Context, p workerPaneState, ref backend.Ref) error {
		return p.ReportBlocked(c, ref, message)
	}); err != nil {
		errs = append(errs, fmt.Errorf("herdr pane state: %w", err))
	}
	return errors.Join(errs...)
}

// Cleared releases the pane state NeedsYou set, when the row leaves
// needs-you. A worker whose workspace is gone, or a ledger row with no ref,
// has nothing to clear.
func (n needsYouNotifier) Cleared(ctx context.Context, row worker.QueueRow, led worker.Row) error {
	if len(led.Ref) == 0 {
		return nil
	}
	err := n.onPane(ctx, row, led, func(c context.Context, p workerPaneState, ref backend.Ref) error {
		return p.ReleaseBlocked(c, ref)
	})
	if err == nil || errors.Is(err, herdradapter.ErrWorkerGone) {
		return nil
	}
	return fmt.Errorf("herdr pane state: %w", err)
}

// onPane runs fn on the worker's pane: the ref from the worker's ledger row,
// on the drain's pinned session, through an adapter pinned to the same one.
func (n needsYouNotifier) onPane(ctx context.Context, row worker.QueueRow, led worker.Row, fn func(context.Context, workerPaneState, backend.Ref) error) error {
	if row.Session != "" && row.Session != n.session {
		return fmt.Errorf("the worker is in herdr session %q, the drain is pinned to %q", row.Session, n.session)
	}
	ref, err := backend.DecodeRef(led.Ref)
	if err != nil {
		return fmt.Errorf("the ledger reference does not decode: %w", err)
	}
	p, err := n.panes()
	if err != nil {
		return err
	}
	if p.Session() != n.session {
		return fmt.Errorf("the herdr adapter resolved session %q, expected the pinned %q", p.Session(), n.session)
	}
	c, cancel := context.WithTimeout(ctx, drainPaneTimeout)
	defer cancel()
	return fn(c, p, ref)
}
