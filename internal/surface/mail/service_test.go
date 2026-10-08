package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSendDelivers(t *testing.T) {
	ad := &fakeAdapter{}
	s, _ := newTestService(t, ad)
	res, err := s.Send(context.Background(), "coord", "pi-1", "hello", PriorityNext)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSent {
		t.Fatalf("status %s, want sent (%s)", res.Status, res.Detail)
	}
	if len(ad.texts) != 1 || !strings.HasPrefix(ad.texts[0], Marker+" from=coord to=pi-1 ") {
		t.Fatalf("rendered %q", ad.texts)
	}
}

func TestSendQueuesThenFlushDelivers(t *testing.T) {
	ad := &fakeAdapter{errs: []error{NotReady("not started")}}
	s, clock := newTestService(t, ad)
	ctx := context.Background()

	res, err := s.Send(ctx, "coord", "pi-1", "hello", PriorityNext)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusQueued {
		t.Fatalf("status %s, want queued", res.Status)
	}

	rep, err := s.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sent != 0 || ad.calls != 1 {
		t.Fatalf("flushed inside the backoff: %+v, %d calls", rep, ad.calls)
	}

	clock.add(6 * time.Second)
	rep, err = s.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sent != 1 {
		t.Fatalf("flush after backoff: %+v", rep)
	}
	entries, err := s.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusSent || entries[0].Attempts != 2 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestPermanentErrorFails(t *testing.T) {
	ad := &fakeAdapter{errs: []error{errors.New("thread id is malformed")}}
	s, _ := newTestService(t, ad)
	res, err := s.Send(context.Background(), "coord", "codex-1", "hello", PriorityNext)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || !strings.Contains(res.Detail, "malformed") {
		t.Fatalf("result = %+v", res)
	}
}

func TestAttemptCapFails(t *testing.T) {
	ad := &fakeAdapter{errs: []error{NotReady("a"), NotReady("b")}}
	s, clock := newTestService(t, ad)
	s.Policy.MaxAttempts = 2
	ctx := context.Background()
	if _, err := s.Send(ctx, "coord", "pi-1", "hello", PriorityNext); err != nil {
		t.Fatal(err)
	}
	clock.add(time.Minute)
	rep, err := s.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Fatalf("report %+v, want one failure at the attempt cap", rep)
	}
}

func TestExpiryNotifiesSender(t *testing.T) {
	ad := &fakeAdapter{errs: []error{NotReady("pi not started"), NotReady("coord busy")}}
	s, clock := newTestService(t, ad)
	ctx := context.Background()
	if _, err := s.Send(ctx, "coord", "pi-1", "hello", PriorityNext); err != nil {
		t.Fatal(err)
	}
	clock.add(31 * time.Minute)
	rep, err := s.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Expired != 1 {
		t.Fatalf("report %+v, want one expiry", rep)
	}
	entries, err := s.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d entries, want the message and its expiry notice", len(entries))
	}
	notice := entries[1]
	if notice.Msg.From != SystemSender || notice.Msg.To != "coord" || !strings.Contains(notice.Msg.Body, "expired undelivered") {
		t.Fatalf("notice = %+v", notice.Msg)
	}
}

func TestPeerMessagesBlockedByDefault(t *testing.T) {
	s, _ := newTestService(t, &fakeAdapter{})
	_, err := s.Send(context.Background(), "pi-1", "codex-1", "hi", PriorityNext)
	if !errors.Is(err, ErrPeerBlocked) {
		t.Fatalf("err = %v, want ErrPeerBlocked", err)
	}
}

func TestSendRejectsBadAddresses(t *testing.T) {
	s, _ := newTestService(t, &fakeAdapter{})
	ctx := context.Background()
	for _, tc := range []struct{ from, to string }{
		{"coord", "coord"},
		{"coord", SystemSender},
		{"coord", "nobody"},
		{"-rf", "pi-1"},
	} {
		if _, err := s.Send(ctx, tc.from, tc.to, "hi", PriorityNext); err == nil {
			t.Errorf("Send(%q, %q) = nil error", tc.from, tc.to)
		}
	}
}

func TestWatchNotifiesOnceOnIdle(t *testing.T) {
	ad := &fakeAdapter{}
	s, _ := newTestService(t, ad)
	ctx := context.Background()
	if err := s.Watch("pi-1", "coord"); err != nil {
		t.Fatal(err)
	}
	if err := s.Watch("pi-1", "coord"); err != nil {
		t.Fatal(err)
	}
	notified, err := s.ApplyEvent(ctx, Event{Worker: "pi-1", State: StateIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 1 || notified[0] != "coord" {
		t.Fatalf("notified %v, want [coord]", notified)
	}
	if last := ad.texts[len(ad.texts)-1]; !strings.Contains(last, "pi-1 finished its turn") {
		t.Fatalf("notice text %q", last)
	}
	notified, err = s.ApplyEvent(ctx, Event{Worker: "pi-1", State: StateIdle})
	if err != nil {
		t.Fatal(err)
	}
	if len(notified) != 0 {
		t.Fatalf("second idle notified %v, want nobody", notified)
	}
}

func TestApplyEventRecordsCodexThread(t *testing.T) {
	s, _ := newTestService(t, &fakeAdapter{})
	ctx := context.Background()
	if _, err := s.ApplyEvent(ctx, Event{Worker: "codex-1", State: StateIdle, ThreadID: "th_123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyEvent(ctx, Event{Worker: "pi-1", State: StateBusy, ThreadID: "th_999"}); err != nil {
		t.Fatal(err)
	}
	codex, err := s.Roster.Get("codex-1")
	if err != nil {
		t.Fatal(err)
	}
	if codex.ThreadID != "th_123" || codex.State != StateIdle {
		t.Fatalf("codex-1 = %+v", codex)
	}
	pi, err := s.Roster.Get("pi-1")
	if err != nil {
		t.Fatal(err)
	}
	if pi.ThreadID != "" || pi.State != StateBusy {
		t.Fatalf("pi-1 = %+v", pi)
	}
	if _, err := s.ApplyEvent(ctx, Event{Worker: "codex-1", State: StateIdle, ThreadID: "--exec=x"}); err == nil {
		t.Fatal("accepted a thread id that starts with a dash")
	}
}

// brokenRoster fails every read after the send, the way a roster.json caught
// mid-write by another tool or made unreadable would.
type brokenRoster struct {
	Roster
	broken bool
}

func (b *brokenRoster) Get(name string) (Worker, error) {
	if b.broken {
		return Worker{}, errors.New("roster.json: unexpected end of JSON input")
	}
	return b.Roster.Get(name)
}

func TestFlushKeepsMessageQueuedWhenRosterUnreadable(t *testing.T) {
	ad := &fakeAdapter{errs: []error{NotReady("not started")}}
	s, clock := newTestService(t, ad)
	br := &brokenRoster{Roster: s.Roster}
	s.Roster = br
	ctx := context.Background()
	if _, err := s.Send(ctx, "coord", "pi-1", "hello", PriorityNext); err != nil {
		t.Fatal(err)
	}
	br.broken = true
	clock.add(time.Minute)
	if _, err := s.Flush(ctx); err == nil {
		t.Fatal("flush over an unreadable roster succeeded")
	}
	entries, err := s.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusQueued {
		t.Fatalf("entries = %+v, want the message still queued", entries)
	}
}

func TestFlushFailsMessageToRemovedWorker(t *testing.T) {
	ad := &fakeAdapter{errs: []error{NotReady("not started")}}
	s, clock := newTestService(t, ad)
	ctx := context.Background()
	if _, err := s.Send(ctx, "coord", "pi-1", "hello", PriorityNext); err != nil {
		t.Fatal(err)
	}
	s.Roster = FileRoster{Dir: t.TempDir()}
	clock.add(time.Minute)
	rep, err := s.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Fatalf("report %+v, want the message to a removed worker failed", rep)
	}
}

func TestSendRefusesTheSystemSender(t *testing.T) {
	s, _ := newTestService(t, &fakeAdapter{})
	for _, tc := range []struct{ from, to string }{{SystemSender, "pi-1"}, {"coord", SystemSender}} {
		if _, err := s.Send(context.Background(), tc.from, tc.to, "hi", PriorityNext); !errors.Is(err, ErrBadName) {
			t.Errorf("Send(%q, %q): err = %v, want ErrBadName", tc.from, tc.to, err)
		}
	}
}

// Idle notices are forgectl's own, so the sender policy cannot drop them: two
// watched turns inside the dedupe window, and more watchers than the per-pair
// rate limit, each still get theirs.
func TestIdleNoticesSkipThePolicy(t *testing.T) {
	ad := &fakeAdapter{}
	s, _ := newTestService(t, ad)
	ctx := context.Background()
	for turn := 0; turn < 2; turn++ {
		if err := s.Watch("pi-1", "coord"); err != nil {
			t.Fatal(err)
		}
		notified, err := s.ApplyEvent(ctx, Event{Worker: "pi-1", State: StateIdle})
		if err != nil || len(notified) != 1 {
			t.Fatalf("turn %d: notified %v, %v", turn, notified, err)
		}
	}
	for i := 0; i < s.Policy.PerPairPerMinute+2; i++ {
		name := fmt.Sprintf("w%d", i)
		if err := s.Roster.Put(Worker{Name: name, Harness: HarnessPi}); err != nil {
			t.Fatal(err)
		}
		if err := s.Watch(name, "coord"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyEvent(ctx, Event{Worker: name, State: StateIdle}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Messages(func(e Entry) bool { return e.Msg.From == SystemSender })
	if err != nil {
		t.Fatal(err)
	}
	want := 2 + s.Policy.PerPairPerMinute + 2
	sent := 0
	for _, e := range entries {
		if e.Status == StatusSent {
			sent++
		}
	}
	if len(entries) != want || sent != want {
		t.Fatalf("%d notices, %d sent; want %d of each", len(entries), sent, want)
	}
}

// A notice that fails part way gives back only its own watcher, so the next
// idle does not notify the others twice.
func TestIdleNoticeFailureRestoresOnlyTheFailedWatcher(t *testing.T) {
	ad := &fakeAdapter{}
	s, _ := newTestService(t, ad)
	ctx := context.Background()
	if err := s.Roster.Update("pi-1", func(w *Worker) error {
		w.Watchers = []string{"coord", "bad name"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < 2; turn++ {
		if _, err := s.ApplyEvent(ctx, Event{Worker: "pi-1", State: StateIdle}); err == nil {
			t.Fatalf("turn %d: want the malformed watcher's error", turn)
		}
		w, err := s.Roster.Get("pi-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(w.Watchers) != 1 || w.Watchers[0] != "bad name" {
			t.Fatalf("turn %d: watchers %q, want only the failed one back", turn, w.Watchers)
		}
	}
	entries, err := s.Messages(func(e Entry) bool { return e.Msg.From == SystemSender })
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Msg.To != "coord" {
		t.Fatalf("notices %+v, want one to coord", entries)
	}
}
