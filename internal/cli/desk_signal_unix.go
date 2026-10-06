// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// An item queued by `desk add` raises a signal the operator sees without the
// desk on screen: a herdr notification, the queuing session's pane in herdr's
// needs-you state, and a macOS notification. [desk] notify_herdr and
// notify_macos turn each off. The pane state is cleared when the item runs or
// is skipped (attach), and stays while other items from that pane still wait.

// deskSignalTimeout bounds each signal call. A signal is a courtesy: a hung
// herdr or osascript must not hold up a queue operation.
const deskSignalTimeout = time.Second

// deskSignalBodyMax caps the notification body, matching herdr.NotificationMaxRunes.
const deskSignalBodyMax = 200

// deskSignalErrMax caps the cause shown in a failed-signal warning.
const deskSignalErrMax = 200

// deskMacNotify posts the macOS notification; a seam so tests record the call
// instead of running osascript.
var deskMacNotify = func(ctx context.Context, deps module.Deps, title, body string) error {
	return notify.New(deps.Runner).Notify(ctx, title, body)
}

type deskSignal struct {
	cfg  config.DeskConfig
	deps module.Deps
}

func newDeskSignal(deps module.Deps) deskSignal {
	return deskSignal{cfg: deps.Cfg.Desk, deps: deps}
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

	if s.cfg.MacOSSignal() {
		c, cancel := context.WithTimeout(ctx, deskSignalTimeout)
		if err := deskMacNotify(c, s.deps, title, body); err != nil {
			failed = append(failed, signalFailure("macOS notification", "notify_macos", err))
		}
		cancel()
	}
	if path, ok := s.herdrPath(); ok && herdr.CheckSession(deskLookupEnv) == nil {
		c, cancel := context.WithTimeout(ctx, deskSignalTimeout)
		err := herdr.NotificationShow(c, s.deps.SensitiveRunner, path, herdr.Notification{Title: title, Body: body, Sound: herdr.SoundRequest})
		cancel()
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

// failureHint says what to try. The sensitive runner withholds the tool's own
// stderr, and a runner error is only an exit status, so this names the likely
// fix instead of echoing text the operator cannot act on.
func failureHint(setting string, err error) string {
	switch setting {
	case "notify_macos":
		return "osascript did not post it; check System Settings > Notifications"
	default:
		var he *herdr.Error
		if errors.As(err, &he) {
			return termsafe.SafeLineMax(he.Error(), deskSignalErrMax)
		}
		return "herdr exited with an error; run herdr by hand to see why"
	}
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
	// a failed call this desk handle stops calling it. The hooks run on the
	// claim path, so a "run all" must not wait on a dead herdr N times.
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
		if err != nil {
			gaveUp.Store(true)
		}
	}
	d.OnLeavePending(func(m desk.Meta) { refresh(m, true) })
	d.OnReturnPending(func(m desk.Meta) { refresh(m, false) })
}
