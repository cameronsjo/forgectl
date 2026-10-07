package cli

import (
	"context"
	"time"
)

// The operator signals the desk and the surface drain share. Each caller
// picks its own pane: the desk marks the pane of the session that queued an
// item, the drain the worker's own pane from its ledger ref. The pane-state
// argv itself is herdr.ReportBlockedArgs and herdr.ReleaseAgentArgs, which
// take the pane as an explicit operand.

// signalTimeout bounds each macOS notification. A signal is a courtesy: a
// hung osascript must not hold up the queue operation or the drain tick that
// raised it.
const signalTimeout = time.Second

// sendMacSignal posts one macOS notification through post, bounded by
// signalTimeout. post is internal/notify's Notify or a test seam over it:
// the title and body reach osascript as argv, capped and rendered inert
// there, never spliced into a script.
func sendMacSignal(ctx context.Context, post func(context.Context, string, string) error, title, body string) error {
	c, cancel := context.WithTimeout(ctx, signalTimeout)
	defer cancel()
	return post(c, title, body)
}
