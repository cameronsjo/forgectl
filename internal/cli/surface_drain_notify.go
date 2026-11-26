package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The drain's needs-you signal goes out on three channels, each one attempt
// the drain records as a notify event: a macOS notification naming the worker
// and the reason, a herdr notification with the same text, and the worker's
// own herdr pane put in herdr's needs-you state.
//
// The pane comes from the worker's ledger ref on the drain's pinned herdr
// session, never from the drain's own HERDR_PANE_ID: the drain runs detached,
// and any pane it inherited is the operator's, not the worker's.
//
// Only herdr says whether a notification was seen. osascript exits 0 whether
// or not macOS displays it (Focus, or a notification permission turned off),
// and terminal-notifier exits 0 once macOS accepts it, so a macOS attempt
// that succeeded says only that the sender ran; herdr's reply says whether it
// showed its own (forgectl#1189).
//
// The macOS banner carries the worker's group, so a repeat replaces it and
// Cleared removes it. Its click brings herdr-app forward; it does not show
// the worker's tab, because the drain's refs are herdr 0.9 panes, not the
// herdr-app terminals a click can show.

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

// drainPanes reports that the legacy herdr adapter moved to a plugin (#1518).
func drainPanes() (workerPaneState, error) {
	return nil, errLegacyBackend
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
// went out (for macOS: when the sender exited 0).
type notifyAttempt struct {
	channel string
	err     error
	// sender is the macOS sender that posted it (notify.Sender*).
	sender string
	// note is a remark on an attempt that did not fail (herdr rate-limited
	// it), recorded on its notify event.
	note string
}

// NeedsYou sends the macOS and herdr notifications, then marks the worker's
// pane. A pane that cannot be verified as the worker's is left alone; the
// notifications go out either way. It returns one attempt per channel tried.
func (n needsYouNotifier) NeedsYou(ctx context.Context, row worker.QueueRow, led worker.Row, reason string) []notifyAttempt {
	title, body, message := needsYouText(row.Name, reason)
	attempts := n.alert(ctx, notify.Message{Title: title, Body: body, Group: drainBannerGroup(row.Name)})
	err := n.onPane(ctx, row, led, func(c context.Context, p workerPaneState, ref backend.Ref) error {
		return p.ReportBlocked(c, ref, message)
	})
	return append(attempts, notifyAttempt{channel: channelPane, err: err})
}

// Alert sends one notification, not tied to a worker's pane: macOS when this
// platform posts it, and herdr on the drain's pinned session.
func (n needsYouNotifier) Alert(ctx context.Context, title, body string) []notifyAttempt {
	return n.alert(ctx, notify.Message{Title: title, Body: body})
}

func (n needsYouNotifier) alert(ctx context.Context, m notify.Message) []notifyAttempt {
	var attempts []notifyAttempt
	if n.mac != nil && n.mac.Supported() {
		sent, err := sendMacSignal(ctx, n.mac.Notify, m)
		a := notifyAttempt{channel: channelMacOS, err: err, sender: sent.Sender}
		if sent.Fallback != nil {
			a.note = "terminal-notifier failed (" + sent.Fallback.Error() + "), so osascript posted it and a click opens Script Editor"
		}
		attempts = append(attempts, a)
	}
	note, err := n.toast(ctx, m.Title, m.Body)
	return append(attempts, notifyAttempt{channel: channelHerdr, err: err, note: note})
}

// drainBannerGroup is the macOS banner group of one worker.
func drainBannerGroup(name string) string { return notify.GroupID("drain", name) }

// toast shows a herdr notification through an adapter pinned to the drain's
// session. herdr's reply is read through NotificationResult.Err, as the
// desk's signal reads it: a notification herdr did not show is a failure
// naming its reason, except rate_limited, which comes back as a note.
func (n needsYouNotifier) toast(ctx context.Context, title, body string) (string, error) {
	p, err := n.panes()
	if err != nil {
		return "", err
	}
	if p.Session() != n.session {
		return "", fmt.Errorf("the herdr adapter resolved session %q, expected the pinned %q", p.Session(), n.session)
	}
	c, cancel := context.WithTimeout(ctx, drainPaneTimeout)
	defer cancel()
	r, err := p.Notify(c, herdr.Notification{Title: title, Body: body, Sound: herdr.SoundRequest})
	if err != nil {
		return "", err
	}
	return r.Note(), r.Err()
}

// Cleared releases the pane state NeedsYou set, when the row leaves
// needs-you. A worker whose workspace is gone, or a ledger row with no ref,
// has nothing to clear.
func (n needsYouNotifier) Cleared(ctx context.Context, row worker.QueueRow, led worker.Row) error {
	// The banner goes whatever the pane's fate; a failure to remove it is
	// dropped, as the desk drops its own: it is a courtesy.
	if n.mac != nil && n.mac.Supported() {
		_ = n.mac.Remove(ctx, drainBannerGroup(row.Name))
	}
	if len(led.Ref) == 0 {
		return nil
	}
	err := n.onPane(ctx, row, led, func(c context.Context, p workerPaneState, ref backend.Ref) error {
		return p.ReleaseBlocked(c, ref)
	})
	if err == nil || workerIsGone(err) {
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
	Note    string `json:"note,omitempty"`
	// Sender is the macOS sender that posted it: terminal-notifier or
	// osascript. Empty for herdr and for a failed macOS attempt.
	Sender string `json:"sender,omitempty"`
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

  macos  on macOS only: terminal-notifier when it is on PATH, else osascript
         (or osascript when terminal-notifier fails, which the note says).
         Neither reports whether macOS displayed the notification (Focus, or
         a notification permission turned off), so "ok" here means only that
         the sender ran: check that you saw it.
  herdr  herdr notification show, on the herdr session the drain would pin
         (resolved as drain start resolves it). herdr's reply says whether
         it showed the notification and, when not, why: disabled,
         no_foreground_client, or busy. rate_limited (herdr showed another
         notification moments ago) is ok, with a note, as in the drain.

It runs whatever [surface.drain] notify says, and needs no running drain.
The worker pane's needs-you state has no worker to mark here and is not
tested.

--json prints {"herdr_session","attempts":[{"channel","ok","error","note","sender"}]}.

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
	cmd.Flags().BoolVar(&asJSON, "json", false, `print {"herdr_session","attempts":[{"channel","ok","error","note","sender"}]} as JSON`)
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
		r := notifyTestAttempt{Channel: a.channel, OK: a.err == nil, Note: termsafe.SafeLineMax(a.note, maxDrainErrorLen), Sender: a.sender}
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
			case a.Note != "":
				line += "ok, but " + a.Note
			case a.Channel == channelMacOS:
				line += a.Sender + " exited 0; macOS does not report whether it displayed the notification, so check that you saw it"
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
