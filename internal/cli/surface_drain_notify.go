package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The drain's needs-you signal: one macOS notification naming the worker and
// the reason, one herdr notification with the same text, and the worker's
// own herdr pane put in herdr's needs-you state. Each is one attempt the
// drain records as an event. osascript exits 0 whether or not macOS displays
// the notification (Focus, or a notification permission turned off), so a
// macOS attempt that succeeded says only that osascript ran; herdr's reply
// says whether it showed its own (forgectl#1189). The pane comes from the worker's ledger ref on the drain's pinned
// herdr session, never from the drain's own HERDR_PANE_ID: the drain runs
// detached, and any pane it inherited is the operator's, not the worker's.

// drainPaneTimeout bounds the herdr half: the ownership checks and the
// pane-state change, together.
const drainPaneTimeout = 5 * time.Second

// workerPaneState is the herdr adapter's pane-state surface for one worker,
// reached by its ref after the adapter's ownership checks.
type workerPaneState interface {
	Session() string
	Notify(ctx context.Context, n herdr.Notification) (herdr.NotificationResult, error)
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

// Notification channels, as a notify event's state names them.
const (
	channelMacOS = "macos"
	channelHerdr = "herdr"
	channelPane  = "herdr-pane"
)

// notifyAttempt is one channel's try at a notification: err is nil when it
// went out (for macOS: when osascript exited 0).
type notifyAttempt struct {
	channel string
	err     error
}

// NeedsYou sends the macOS and herdr notifications, then marks the worker's
// pane. A pane that cannot be verified as the worker's is left alone; the
// notifications go out either way. It returns one attempt per channel tried.
func (n needsYouNotifier) NeedsYou(ctx context.Context, row worker.QueueRow, led worker.Row, reason string) []notifyAttempt {
	title, body, message := needsYouText(row.Name, reason)
	attempts := n.Alert(ctx, title, body)
	err := n.onPane(ctx, row, led, func(c context.Context, p workerPaneState, ref backend.Ref) error {
		return p.ReportBlocked(c, ref, message)
	})
	return append(attempts, notifyAttempt{channel: channelPane, err: err})
}

// Alert sends one notification, not tied to a worker's pane: macOS when this
// platform posts it, and herdr on the drain's pinned session.
func (n needsYouNotifier) Alert(ctx context.Context, title, body string) []notifyAttempt {
	var attempts []notifyAttempt
	if n.mac != nil && n.mac.Supported() {
		attempts = append(attempts, notifyAttempt{channel: channelMacOS, err: sendMacSignal(ctx, n.mac.Notify, title, body)})
	}
	return append(attempts, notifyAttempt{channel: channelHerdr, err: n.toast(ctx, title, body)})
}

// toast shows a herdr notification through an adapter pinned to the drain's
// session. A notification herdr declined to show is a failure: its reply
// says why (disabled, rate_limited, no_foreground_client, busy).
func (n needsYouNotifier) toast(ctx context.Context, title, body string) error {
	p, err := n.panes()
	if err != nil {
		return err
	}
	if p.Session() != n.session {
		return fmt.Errorf("the herdr adapter resolved session %q, expected the pinned %q", p.Session(), n.session)
	}
	c, cancel := context.WithTimeout(ctx, drainPaneTimeout)
	defer cancel()
	r, err := p.Notify(c, herdr.Notification{Title: title, Body: body, Sound: herdr.SoundRequest})
	if err != nil {
		return err
	}
	if !r.Shown {
		return fmt.Errorf("herdr did not show it: %s", r.Reason)
	}
	return nil
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

// drainNotifyTestTitle is notify-test's notification title.
const drainNotifyTestTitle = "forgectl: drain notify-test"

// notifyTestAttempt is one channel's result in `drain notify-test --json`.
type notifyTestAttempt struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// notifyTestResult is what `drain notify-test --json` prints. Additive
// changes only (ADR-0008 rule 2).
type notifyTestResult struct {
	HerdrSession string              `json:"herdr_session,omitempty"`
	Attempts     []notifyTestAttempt `json:"attempts"`
}

func newSurfaceDrainNotifyTestCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "notify-test",
		Short: "Send one test notification through the drain's channels",
		Long: `notify-test sends one notification through each channel the drain's needs-you
signal uses, the way the drain sends it, and prints each channel's result:

  macos  osascript, on macOS only. osascript exits 0 whether or not macOS
         displays the notification (Focus, or the notification permission of
         Script Editor or your terminal app), so "ok" here means only that
         osascript ran: check that you saw it.
  herdr  herdr notification show, on the herdr session the drain would pin
         (resolved as drain start resolves it). herdr's reply says whether
         it showed the notification and, when not, why: disabled,
         rate_limited, no_foreground_client, or busy.

It runs whatever [surface.drain] notify says, and needs no running drain.
The worker pane's needs-you state has no worker to mark here and is not
tested.

--json prints {"herdr_session","attempts":[{"channel","ok","error"}]}.

Exit 0: every channel went out. Exit 1: a channel failed or herdr did not
show the notification.

  forgectl surface drain notify-test
  forgectl surface drain notify-test --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			n := needsYouNotifier{mac: notify.New(deps.Runner), panes: drainPanes}
			if p, err := drainPanes(); err == nil {
				n.session = p.Session()
			}
			return runDrainNotifyTest(cmd.Context(), cmd.OutOrStdout(), n, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"herdr_session","attempts":[{"channel","ok","error"}]} as JSON`)
	return cmd
}

// runDrainNotifyTest sends the test notification through n and reports each
// channel. A herdr session that did not resolve fails the herdr channel with
// the adapter's own error.
func runDrainNotifyTest(ctx context.Context, out io.Writer, n needsYouNotifier, asJSON bool) error {
	body := "a test of the drain's needs-you notification, sent " + time.Now().Format("15:04:05")
	res := notifyTestResult{HerdrSession: n.session, Attempts: []notifyTestAttempt{}}
	failed := false
	for _, a := range n.Alert(ctx, drainNotifyTestTitle, body) {
		r := notifyTestAttempt{Channel: a.channel, OK: a.err == nil}
		if a.err != nil {
			failed = true
			r.Error = termsafe.SafeLineMax(a.err.Error(), maxDrainErrorLen)
		}
		res.Attempts = append(res.Attempts, r)
	}
	if asJSON {
		if err := writeJSON(out, res); err != nil {
			return err
		}
	} else {
		if res.HerdrSession != "" {
			if _, err := fmt.Fprintf(out, "herdr session %s\n", termsafe.SafeLineMax(res.HerdrSession, 64)); err != nil {
				return err
			}
		}
		for _, a := range res.Attempts {
			line := a.Channel + ": "
			switch {
			case !a.OK:
				line += "failed: " + a.Error
			case a.Channel == channelMacOS:
				line += "osascript exited 0; macOS does not report whether it displayed the notification, so check that you saw it"
			default:
				line += "shown"
			}
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	if failed {
		return newSilentCodedError(1)
	}
	return nil
}
