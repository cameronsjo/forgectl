//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/notify"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// TestNeedsYouShowsAHerdrNotification: the needs-you signal also goes through
// herdr notification show, on the pinned session, and a notification herdr
// declined is a failed attempt naming herdr's reason (forgectl#1189).
func TestNeedsYouShowsAHerdrNotification(t *testing.T) {
	t.Run("shown", func(t *testing.T) {
		r := newNotifyRig(t)
		attempts := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt")
		if got := channels(attempts); got != "macos herdr herdr-pane" {
			t.Fatalf("channels %q, want macos herdr herdr-pane", got)
		}
		if err := failures(attempts); err != nil {
			t.Fatalf("failures: %v", err)
		}
		if len(r.panes.toasts) != 1 || r.panes.toasts[0] != "forgectl: fix-flake needs you|blocked: prompt" {
			t.Fatalf("herdr notifications %q", r.panes.toasts)
		}
	})
	t.Run("not shown", func(t *testing.T) {
		r := newNotifyRig(t)
		r.panes.toast = herdr.NotificationResult{Reason: "no_foreground_client"}
		err := failures(r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt"))
		if err == nil || !strings.Contains(err.Error(), "herdr: herdr did not show the notification: no_foreground_client") {
			t.Fatalf("err = %v, want herdr's reason", err)
		}
		if len(r.panes.reported) != 1 || len(r.run.Calls) != 1 {
			t.Fatalf("reported %q, osascript %d; the other channels still go out", r.panes.reported, len(r.run.Calls))
		}
	})
	t.Run("herdr fails", func(t *testing.T) {
		r := newNotifyRig(t)
		r.panes.toastErr = errors.New("herdr notification show: exit; herdr said: unknown option: x")
		if err := failures(r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt")); err == nil || !strings.Contains(err.Error(), "unknown option") {
			t.Fatalf("err = %v, want herdr's own words", err)
		}
	})
	t.Run("adapter pinned elsewhere shows nothing", func(t *testing.T) {
		r := newNotifyRig(t)
		r.panes.session = "default"
		if err := failures(r.n.Alert(t.Context(), "t", "b")); err == nil {
			t.Fatal("an adapter on another session was used")
		}
		if len(r.panes.toasts) != 0 {
			t.Fatalf("notified %q on an unpinned adapter", r.panes.toasts)
		}
	})
}

func channels(attempts []notifyAttempt) string {
	var out []string
	for _, a := range attempts {
		out = append(out, a.channel)
	}
	return strings.Join(out, " ")
}

// TestDrainRecordsEachNotifyAttempt: every channel's attempt is one notify
// event naming the channel, and the failures are one error event.
func TestDrainRecordsEachNotifyAttempt(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedLive(t, q, "w", "/repo/a", worker.QueueLaunched, worker.StageLaunched)
	f.notifyErr = errors.New("refused")
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
		return drain.Probe{State: drain.ProbeRead, Verdict: ready.Verdict{State: ready.StateBlocked, Blocking: "permission prompt"}}
	}
	d.tick(t.Context())
	notes := eventsOfKind(f, drain.EventNotify)
	if len(notes) != 2 || notes[0].State != channelHerdr || notes[0].Error != "" || notes[1].State != channelPane || notes[1].Error != "refused" || notes[1].Name != "w" {
		t.Fatalf("notify events %+v; want herdr ok and herdr-pane refused, for w", notes)
	}
	if n := errorEvents(f, "needs-you notification: herdr-pane: refused"); n != 1 {
		t.Fatalf("%d error events naming the failed channel, want 1", n)
	}
}

// TestDrainSlotsHoldIsVisible: a hold is in drain.json from its first check,
// keeps its start while it lasts, and goes when the plan empties
// (forgectl#1232).
func TestDrainSlotsHoldIsVisible(t *testing.T) {
	f, d, q, _ := slotsDrain(t, drain.SlotsCheck{Exit: 2, Reason: "broken"})
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	d.tick(t.Context())
	st := d.status(drain.Status{}, nil)
	if st.SlotsHold == nil || st.SlotsHold.Cond != "exit 2" || !st.SlotsHold.Since.Equal(drainT0) || !strings.Contains(st.SlotsHold.Reason, "could not be checked") {
		t.Fatalf("slots_hold %+v; want exit 2 since the first check", st.SlotsHold)
	}
	f.now = drainT0.Add(5 * time.Minute)
	d.tick(t.Context())
	if st := d.status(drain.Status{}, nil); st.SlotsHold == nil || !st.SlotsHold.Since.Equal(drainT0) {
		t.Fatalf("slots_hold %+v; want the same start while it lasts", st.SlotsHold)
	}
	if _, err := q.Dequeue("a"); err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context())
	if st := d.status(drain.Status{}, nil); st.SlotsHold != nil {
		t.Fatalf("slots_hold %+v with nothing to claim; want none", st.SlotsHold)
	}
}

// TestDrainSlotsStuckAlertsOnce: a check that cannot answer for
// drain.SlotsStuckAfter is one slots-stuck event and one alert; a full cap
// (exit 1) never alerts; notify off records the event only.
func TestDrainSlotsStuckAlertsOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		check  drain.SlotsCheck
		notify bool
		events int
		alerts int
	}{
		{"broken, notify on", drain.SlotsCheck{Exit: 2, Reason: "broken"}, true, 1, 1},
		{"timeout, notify off", drain.SlotsCheck{Exit: -1, TimedOut: true}, false, 1, 0},
		{"cap full", drain.SlotsCheck{Exit: 1, Reason: "full"}, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d, q, _ := slotsDrain(t, tc.check)
			f.cfg.Surface.Drain.Notify = &tc.notify
			enqueueAt(t, q, "a", "/repo/a", drainT0)
			d.tick(t.Context())
			f.now = drainT0.Add(drain.SlotsStuckAfter - time.Second)
			d.tick(t.Context())
			if n := len(eventsOfKind(f, drain.EventSlotsStuck)); n != 0 {
				t.Fatalf("%d slots-stuck events before %s", n, drain.SlotsStuckAfter)
			}
			for i := range 3 {
				f.now = drainT0.Add(drain.SlotsStuckAfter + time.Duration(i)*time.Minute)
				d.tick(t.Context())
			}
			stuck := eventsOfKind(f, drain.EventSlotsStuck)
			if len(stuck) != tc.events || len(f.alerted) != tc.alerts {
				t.Fatalf("%d slots-stuck events, alerts %q; want %d and %d", len(stuck), f.alerted, tc.events, tc.alerts)
			}
			if tc.events == 1 && !strings.Contains(stuck[0].Error, "claude launches held for 15m0s") {
				t.Fatalf("slots-stuck event %+v; want how long", stuck[0])
			}
		})
	}
}

// TestDrainStatusShowsTheSlotsHold: drain status names the hold, how long
// it has lasted and why, and says when the cap cannot be checked.
func TestDrainStatusShowsTheSlotsHold(t *testing.T) {
	files := drainCmdEnv(t)
	lock, err := files.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test
	since := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Second)
	data, err := drain.EncodeStatus(drain.Status{Status: drain.StatusRunning, PID: 4242, StartedAt: since, LastTick: time.Now(), IntervalSeconds: 30,
		SlotsHold: &drain.SlotsHold{Cond: "exit 2", Reason: "held: the session cap could not be checked: claude-slots check 1 exited 2", Since: since}})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteStatus(lock, data); err != nil {
		t.Fatal(err)
	}
	out, err := runQueueCmd(t, newSurfaceDrainStatusCmd(), "--json")
	var v drainStatusView
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v.SlotsHold == nil || v.SlotsHold.Cond != "exit 2" || v.HeldFor < 20*60 {
		t.Fatalf("status --json: %s, %v; want slots_hold exit 2 held 20 minutes", out, err)
	}
	out, err = runQueueCmd(t, newSurfaceDrainStatusCmd())
	if err != nil || !strings.Contains(out, "claude launches held for 20m") || !strings.Contains(out, "could not be checked") {
		t.Fatalf("status text: %q, %v", out, err)
	}
}

// TestDrainNotifyTest: notify-test sends through each channel and reports
// each, exit 1 when herdr did not show it.
func TestDrainNotifyTest(t *testing.T) {
	r := newNotifyRig(t)
	var out strings.Builder
	if err := runDrainNotifyTest(t.Context(), &out, r.n, false); err != nil {
		t.Fatalf("notify-test: %v", err)
	}
	if !strings.Contains(out.String(), "macos: osascript exited 0; macOS does not report") || !strings.Contains(out.String(), "herdr: shown") {
		t.Fatalf("output %q", out.String())
	}
	if len(r.panes.reported) != 0 {
		t.Fatalf("notify-test marked a pane: %q", r.panes.reported)
	}

	r = newNotifyRig(t)
	r.n.mac = notify.New(r.run, notify.WithGOOS("linux"))
	r.panes.toast = herdr.NotificationResult{Reason: "disabled"}
	out.Reset()
	err := runDrainNotifyTest(t.Context(), &out, r.n, true)
	if ExitCode(err) != 1 {
		t.Fatalf("exit %d (%v), want 1", ExitCode(err), err)
	}
	var res notifyTestResult
	if json.Unmarshal([]byte(out.String()), &res) != nil || res.HerdrSession != drainTestSession || len(res.Attempts) != 1 ||
		res.Attempts[0].Channel != "herdr" || res.Attempts[0].OK || !strings.Contains(res.Attempts[0].Error, "disabled") {
		t.Fatalf("--json %s", out.String())
	}
}

// TestDrainStatusDropsTheHoldOfADeadDrain: a stale (crashed or killed) or
// stopped drain's last hold is not shown; its length would only grow.
func TestDrainStatusDropsTheHoldOfADeadDrain(t *testing.T) {
	files := drainCmdEnv(t)
	lock, err := files.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test
	since := time.Now().Add(-3 * time.Hour).UTC()
	data, err := drain.EncodeStatus(drain.Status{Status: drain.StatusRunning, PID: 4242, StartedAt: since, LastTick: since, IntervalSeconds: 30,
		SlotsHold: &drain.SlotsHold{Cond: "exit 2", Reason: "held: the session cap could not be checked", Since: since}})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteStatus(lock, data); err != nil {
		t.Fatal(err)
	}
	out, err := runQueueCmd(t, newSurfaceDrainStatusCmd(), "--json")
	var v drainStatusView
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v.Status != drain.StatusStale || v.SlotsHold != nil || v.HeldFor != 0 {
		t.Fatalf("status --json: %s, %v; want stale with no slots_hold", out, err)
	}
	out, err = runQueueCmd(t, newSurfaceDrainStatusCmd())
	if err != nil || strings.Contains(out, "claude launches held") {
		t.Fatalf("status text: %q, %v; want no hold for a stale drain", out, err)
	}
}

// TestDrainRateLimitedIsANoteNotAnError: herdr rate-limiting the needs-you
// notification is a notify event with a note and no error event, as the
// desk treats it.
func TestDrainRateLimitedIsANoteNotAnError(t *testing.T) {
	r := newNotifyRig(t)
	r.panes.toast = herdr.NotificationResult{Reason: herdr.ReasonRateLimited}
	attempts := r.n.NeedsYou(t.Context(), r.row, r.led, "blocked: prompt")
	if err := failures(attempts); err != nil {
		t.Fatalf("failures: %v; rate_limited is not one", err)
	}
	if attempts[1].channel != channelHerdr || !strings.Contains(attempts[1].note, "rate_limited") {
		t.Fatalf("herdr attempt %+v, want a rate_limited note", attempts[1])
	}

	f, d, q := newFakeDrain(t)
	d.io.notify = rateLimitedNotifier{f}
	seedLive(t, q, "w", "/repo/a", worker.QueueLaunched, worker.StageLaunched)
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
		return drain.Probe{State: drain.ProbeRead, Verdict: ready.Verdict{State: ready.StateBlocked, Blocking: "permission prompt"}}
	}
	d.tick(t.Context())
	notes := eventsOfKind(f, drain.EventNotify)
	if len(notes) != 1 || notes[0].Error != "" || !strings.Contains(notes[0].Note, "rate_limited") {
		t.Fatalf("notify events %+v; want one with a rate_limited note and no error", notes)
	}
	if n := len(eventsOfKind(f, drain.EventError)); n != 0 {
		t.Fatalf("%d error events for a rate-limited notification", n)
	}
	if line := drainEventLine(notes[0]); !strings.Contains(line, "(herdr did not show it: rate_limited") {
		t.Fatalf("event line %q, want the note", line)
	}

	r = newNotifyRig(t)
	r.n.mac = notify.New(r.run, notify.WithGOOS("linux"))
	r.panes.toast = herdr.NotificationResult{Reason: herdr.ReasonRateLimited}
	var out strings.Builder
	if err := runDrainNotifyTest(t.Context(), &out, r.n, false); err != nil {
		t.Fatalf("notify-test: %v; rate_limited is ok", err)
	}
	if !strings.Contains(out.String(), "herdr: ok, but herdr did not show it: rate_limited") {
		t.Fatalf("notify-test output %q", out.String())
	}
}

// rateLimitedNotifier answers NeedsYou with one herdr attempt herdr
// rate-limited.
type rateLimitedNotifier struct{ *fakeDrain }

func (rateLimitedNotifier) NeedsYou(context.Context, worker.QueueRow, worker.Row, string) []notifyAttempt {
	return []notifyAttempt{{channel: channelHerdr, note: herdr.NotificationResult{Reason: herdr.ReasonRateLimited}.Note()}}
}

// TestDrainSlotsAlternatingFaultsStillAlert: a check that alternates between
// a timeout and exit 2 is one fault hold, so it still alerts once.
func TestDrainSlotsAlternatingFaultsStillAlert(t *testing.T) {
	f, d, q, _ := slotsDrain(t, drain.SlotsCheck{Exit: -1, TimedOut: true}, drain.SlotsCheck{Exit: 2, Reason: "x"},
		drain.SlotsCheck{Exit: -1, TimedOut: true}, drain.SlotsCheck{Exit: 2, Reason: "x"}, drain.SlotsCheck{Exit: -1, TimedOut: true})
	on := true
	f.cfg.Surface.Drain.Notify = &on
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	for i := range 5 {
		f.now = drainT0.Add(time.Duration(i) * (drain.SlotsStuckAfter / 3))
		d.tick(t.Context())
	}
	if n := len(eventsOfKind(f, drain.EventSlotsStuck)); n != 1 || len(f.alerted) != 1 {
		t.Fatalf("%d slots-stuck events, alerts %q; want one each", n, f.alerted)
	}
	if st := d.status(drain.Status{}, nil); st.SlotsHold == nil || !st.SlotsHold.Since.Equal(drainT0) {
		t.Fatalf("slots_hold %+v; want the hold since the first failure", st.SlotsHold)
	}
}
