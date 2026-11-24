// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/progstatus"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// An item queued by `desk add` raises a signal the operator sees without the
// desk on screen: a herdr notification, the queuing session's pane in herdr's
// needs-you state, and a macOS notification. [desk] notify_herdr and
// notify_macos turn each off. The pane state is cleared when the item runs or
// is skipped (attach), and stays while other items from that pane still wait.

// deskSignalTimeout bounds each signal call. A signal is a courtesy: a hung
// herdr or osascript must not hold up a queue operation.
const deskSignalTimeout = signalTimeout

// deskSignalBodyMax caps the notification body, matching herdr.NotificationMaxRunes.
const deskSignalBodyMax = 200

// deskSignalErrMax caps the cause shown in a failed-signal warning. It fits
// the seam's exit summary and herdr's own one-line reason after it.
const deskSignalErrMax = 400

// deskMacNotify posts the macOS notification; a seam so tests record the call
// instead of running a sender.
var deskMacNotify = func(ctx context.Context, deps module.Deps, m notify.Message) (notify.Sent, error) {
	return notify.New(deps.Runner).Notify(ctx, m)
}

// deskMacRemove clears an item's banner once it has run or been skipped; a
// seam like deskMacNotify.
var deskMacRemove = func(ctx context.Context, deps module.Deps, group string) error {
	return notify.New(deps.Runner).Remove(ctx, group)
}

// deskBannerGroup is the banner group of an item, keyed by the hash it was
// queued at: desk add knows it, and every meta that leaves pending/ carries
// it, so the banner is cleared without the hooks needing the item's name. An
// item re-queued with other bytes is a new banner. A hash that does not
// validate (meta is untrusted on read) has no group.
func deskBannerGroup(sha string) string {
	if !desk.ValidSHA256(sha) {
		return ""
	}
	return notify.GroupID("desk", sha[:16])
}

// deskClickTerminal is the herdr-app terminal a click on the banner shows:
// this process's own, when it runs in one. In a herdr 0.9 pane, or no host
// at all, the click only brings herdr-app forward.
func deskClickTerminal() string {
	if herdr.DetectHost(deskLookupEnv) != herdr.HostApp {
		return ""
	}
	v, _ := deskLookupEnv("HC_TERMINAL_ID")
	return v
}

// deskMacSupported reports whether the macOS notification can post here: the
// same predicate Notify checks, so a signal that Notify would drop silently is
// not counted as one that can go out. A seam so tests simulate either platform.
var deskMacSupported = func(deps module.Deps) bool {
	return notify.New(deps.Runner).Supported()
}

type deskSignal struct {
	cfg  config.DeskConfig
	deps module.Deps
}

func newDeskSignal(deps module.Deps) deskSignal {
	return deskSignal{cfg: deps.Cfg.Desk, deps: deps}
}

// enabled reports whether any operator signal can actually go out: the macOS
// notification is on and this platform posts it (Notify is a no-op off
// darwin), or the herdr signal is on, herdr is found, and this process is in a
// herdr session. With none, queueing an item tells nobody.
func (s deskSignal) enabled() bool {
	if s.cfg.MacOSSignal() && deskMacSupported(s.deps) {
		return true
	}
	_, ok := s.herdrPath()
	return ok && herdr.CheckSession(deskLookupEnv) == nil
}

// herdrPath is where herdr is when the herdr signal is on and can run.
func (s deskSignal) herdrPath() (string, bool) {
	if !s.cfg.HerdrSignal() || s.deps.SensitiveRunner == nil {
		return "", false
	}
	p, err := deskHerdrPath()
	if err != nil {
		return "", false
	}
	return p, true
}

// pane is the herdr pane of this process, when it runs inside one and the
// herdr signal is on. desk add stamps it into the item so the signal can be
// cleared from any process later.
func (s deskSignal) pane() (string, bool) {
	if _, ok := s.herdrPath(); !ok || herdr.CheckSession(deskLookupEnv) != nil {
		return "", false
	}
	v, ok := deskLookupEnv("HERDR_PANE_ID")
	return v, ok && v != ""
}

// waitingFrom counts pending items, and those queued from pane.
func waitingFrom(d *desk.Desk, pane string) (total, fromPane int, err error) {
	snap, err := d.Scan()
	if err != nil {
		return 0, 0, err
	}
	for _, it := range snap.Pending {
		total++
		if it.Meta.SignalPane == pane {
			fromPane++
		}
	}
	return total, fromPane, nil
}

// queued raises the signals for a just-queued item and returns what failed,
// one line each; the caller prints them as warnings. Nothing here fails the
// add: the item is queued either way.
func (s deskSignal) queued(ctx context.Context, d *desk.Desk, a desk.Added, what string) []string {
	var failed []string
	herdrFailed := false
	total, _, _ := waitingFrom(d, "")
	total = max(total, 1)
	title := fmt.Sprintf("forgectl desk: %d waiting", total)
	body := a.Name
	if what != "" {
		body += ": " + what
	}
	body = termsafe.SafeLineMax(body, deskSignalBodyMax)

	if s.cfg.MacOSSignal() && deskMacSupported(s.deps) {
		post := func(c context.Context, m notify.Message) (notify.Sent, error) { return deskMacNotify(c, s.deps, m) }
		m := notify.Message{Title: title, Body: body, Group: deskBannerGroup(a.SHA256), Terminal: deskClickTerminal()}
		sent, err := sendMacSignal(ctx, post, m)
		switch {
		case err != nil:
			failed = append(failed, signalFailure("macOS notification", "notify_macos", err))
		case sent.Fallback != nil:
			failed = append(failed, fallbackNotice(sent.Fallback))
		}
	}
	if path, ok := s.herdrPath(); ok && herdr.CheckSession(deskLookupEnv) == nil {
		c, cancel := context.WithTimeout(ctx, deskSignalTimeout)
		res, err := herdr.NotificationShow(c, s.deps.SensitiveRunner, path, herdr.Notification{Title: title, Body: body, Sound: herdr.SoundRequest})
		cancel()
		if err == nil {
			err = res.Err()
		}
		if err != nil {
			herdrFailed = true
			failed = append(failed, signalFailure("herdr signal", "notify_herdr", err))
		}
		if pane, ok := deskLookupEnv("HERDR_PANE_ID"); ok && pane != "" {
			c, cancel := context.WithTimeout(ctx, deskSignalTimeout)
			defer cancel()
			if err := herdr.PaneReportBlocked(c, s.deps.SensitiveRunner, path, pane, title+": "+body); err != nil {
				if !herdrFailed {
					failed = append(failed, signalFailure("herdr signal", "notify_herdr", err))
				}
			}
		}
	}
	return failed
}

// signalFailure words one failed signal: what failed, that the item is queued
// anyway, and which setting turns it off. The cause is the runner's own text.
func signalFailure(what, setting string, err error) string {
	return fmt.Sprintf("%s failed (%s); the item is queued. Turn it off with [desk] %s = false", what, failureHint(setting, err), setting)
}

// fallbackNotice words a banner osascript posted because terminal-notifier
// failed: the operator was told, but a click opens Script Editor. Its usual
// cause is terminal-notifier's notification permission being off (exit 3).
func fallbackNotice(cause error) string {
	return "terminal-notifier did not post the macOS notification (" + termsafe.SafeLineMax(cause.Error(), deskSignalErrMax) +
		"); osascript posted it instead, so a click opens Script Editor. Allow terminal-notifier in System Settings > Notifications"
}

// failureHint says what to try. For macOS the runner's error is only an exit
// status, so it names the likely fix. For herdr it is the error itself, which
// carries herdr's refusal or its own stderr (forgectl#1181).
func failureHint(setting string, err error) string {
	if setting == "notify_macos" {
		return "no sender posted it; check System Settings > Notifications for terminal-notifier and Script Editor"
	}
	return termsafe.SafeLineMax(err.Error(), deskSignalErrMax)
}

// cleanupPath is where herdr is for clearing a signal already raised. It does
// not look at the settings: turning notify_herdr off must not strand a pane
// that is blocked from before.
func (s deskSignal) cleanupPath() (string, bool) {
	if s.deps.SensitiveRunner == nil {
		return "", false
	}
	p, err := deskHerdrPath()
	if err != nil {
		return "", false
	}
	return p, true
}

// attach keeps the pane state in step with the queue. When an item leaves
// pending/ by a run, a skip, or a change, the pane is released if no other
// item from it still waits, else its message is refreshed. An Unskip puts the
// state back. Failures are dropped; the state is a courtesy and herdr may be
// gone. A failed scan changes nothing: a signal is never cleared on a guess.
func (s deskSignal) attach(d *desk.Desk) {
	// A herdr that hangs costs one timeout per process, not one per item: after
	// a call times out this desk handle stops calling it. The hooks run on the
	// claim path, so a "run all" must not wait on a dead herdr N times. Any
	// other error (a pane that closed since it queued) says nothing about the
	// next pane, so it does not trip.
	var gaveUp atomic.Bool
	refresh := func(m desk.Meta, releaseWhenNone bool) {
		if m.SignalPane == "" || gaveUp.Load() {
			return
		}
		path, ok := s.cleanupPath()
		if !ok {
			return
		}
		total, left, err := waitingFrom(d, m.SignalPane)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), deskSignalTimeout)
		defer cancel()
		if releaseWhenNone && left == 0 {
			err = herdr.PaneReleaseDesk(ctx, s.deps.SensitiveRunner, path, m.SignalPane)
		} else {
			err = herdr.PaneReportBlocked(ctx, s.deps.SensitiveRunner, path, m.SignalPane, fmt.Sprintf("forgectl desk: %d waiting", total))
		}
		if err != nil && ctx.Err() != nil {
			gaveUp.Store(true)
		}
	}
	d.OnLeavePending(func(m desk.Meta) {
		refresh(m, true)
		s.clearBanner(m)
	})
	// Re-raising is a new signal, so the setting gates it; clearing is not.
	d.OnReturnPending(func(m desk.Meta) {
		if s.cfg.HerdrSignal() {
			refresh(m, false)
		}
	})
}

// clearBanner removes the macOS banner raised for an item that has left
// pending/. Like the pane release it ignores the settings (turning
// notify_macos off must not strand a banner from before) and drops its
// failure: the banner is a courtesy, and the sender may be gone.
func (s deskSignal) clearBanner(m desk.Meta) {
	group := deskBannerGroup(m.SHA256)
	if group == "" || !deskMacSupported(s.deps) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), deskSignalTimeout)
	defer cancel()
	_ = deskMacRemove(ctx, s.deps, group)
}

// waitingRow reports the waiting count over OSC 7501 for a host that has no
// herdr 0.9 CLI (cmux, plain herdr without the fork, anything that reads the
// Program Status Protocol). It returns the hook for tui.DeskOptions.OnWaiting
// and a release to defer.
//
// Row lifetime: the dashboard process holds the row while it is open. The
// signals above are raised by short-lived processes (desk add, and the verbs
// that move an item out of pending), and a host drops a blocked row when the
// reporting process exits, so a row set there would vanish at once, and one
// left behind by a crashed process would stay stale. The dashboard is the one
// long-lived process that watches the queue, so it sets the row when items are
// waiting, clears it when none are, and release clears it on
// every return from the dashboard, a panic included. A killed dashboard leaves
// its row only where the host does not drop it (docs/commands/workflow.md).
//
// Inside a herdr session (herdr.CheckSession) it reports nothing: that path
// already has the herdr notification and the pane's blocked state, and a
// second report would double it. Opting out (FORGECTL_NO_PROGRAM_STATUS) and
// the worker-pane guard live in the Reporter.
func (s deskSignal) waitingRow(r *progstatus.Reporter) (onWaiting func(int), release func()) {
	if !r.Enabled() || herdr.CheckSession(deskLookupEnv) == nil {
		return nil, func() {}
	}
	raised := false
	onWaiting = func(n int) {
		switch {
		case n > 0:
			r.Blocked(progstatus.KindPermission, fmt.Sprintf("%d waiting", n))
			raised = true
		case raised:
			r.Clear()
			raised = false
		}
	}
	release = func() {
		if raised {
			r.Clear()
			raised = false
		}
	}
	return onWaiting, release
}
