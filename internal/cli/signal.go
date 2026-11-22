package cli

import (
	"context"
	"time"

	"github.com/cameronsjo/forgectl/internal/notify"
)

// The operator signals the desk and the surface drain share. Each caller
// picks its own pane: the desk marks the pane of the session that queued an
// item, the drain the worker's own pane from its ledger ref. The pane-state
// argv itself is herdr.ReportBlockedArgs and herdr.ReleaseAgentArgs, which
// take the pane as an explicit operand.

// signalTimeout bounds each macOS notification. A signal is a courtesy: a
// hung sender must not hold up the queue operation or the drain tick that
// raised it.
const signalTimeout = time.Second

// titledNotifier adapts notify.Client to the title-and-body notifier the pr
// package takes, so pr need not import notify. Its banner has no group and no
// click target: a click only brings herdr-app forward.
type titledNotifier struct{ c *notify.Client }

func (n titledNotifier) Notify(ctx context.Context, title, body string) error {
	_, err := n.c.Notify(ctx, notify.Message{Title: title, Body: body})
	return err
}

// sendMacSignal posts one macOS notification through post, bounded by
// signalTimeout. post is internal/notify's Notify or a test seam over it:
// the title and body reach the sender as argv, capped and rendered inert
// there, never spliced into a script or a shell string.
func sendMacSignal(ctx context.Context, post func(context.Context, notify.Message) (notify.Sent, error), m notify.Message) (notify.Sent, error) {
	c, cancel := context.WithTimeout(ctx, signalTimeout)
	defer cancel()
	return post(c, m)
}
