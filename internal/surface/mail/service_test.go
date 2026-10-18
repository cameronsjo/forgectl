package mail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

type writeAheadAdapter struct {
	fakeAdapter
	box      Mailbox
	observed bool
}

func (adapter *writeAheadAdapter) Deliver(context context.Context, worker Worker, message Message, rendered string) (string, error) {
	data, err := os.ReadFile(adapter.box.logPath())
	if err != nil {
		return "", err
	}
	adapter.observed = bytes.Contains(data, []byte(`"status":"unconfirmed"`))
	return "ok", nil
}

type receiptTestAdapter struct {
	fakeAdapter
	detail   string
	accepted bool
	err      error
	receipts int
}

func (adapter *receiptTestAdapter) Receipt(context context.Context, worker Worker, message Message) (string, bool, error) {
	adapter.receipts++
	return adapter.detail, adapter.accepted, adapter.err
}

func TestSendWritesUnconfirmedBeforeDeliverAndCountsOneAttempt(t *testing.T) {
	adapter := &writeAheadAdapter{}
	s, _ := newTestService(t, adapter)
	adapter.box = s.Box
	result, err := s.Send(context.Background(), "coord", "pi-1", "hello", PriorityNext)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusSent || !adapter.observed {
		t.Fatalf("result = %+v, write-ahead observed = %t", result, adapter.observed)
	}
	entries, err := s.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusSent || entries[0].Attempts != 1 {
		t.Fatalf("entries = %+v, want one sent attempt", entries)
	}
}

func TestUnconfirmedFlushUsesReceiptWithoutRedelivery(t *testing.T) {
	adapter := &receiptTestAdapter{
		fakeAdapter: fakeAdapter{errs: []error{Unconfirmed("ack lost")}},
		detail:      "receipt accepted",
		accepted:    true,
	}
	s, _ := newTestService(t, adapter)
	result, err := s.Send(context.Background(), "coord", "pi-1", "hello", PriorityNext)
	if err != nil || result.Status != StatusUnconfirmed {
		t.Fatalf("send result = %+v, err = %v", result, err)
	}
	report, err := s.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Sent != 1 || report.Unconfirmed != 0 || adapter.calls != 1 || adapter.receipts != 1 {
		t.Fatalf("report = %+v, deliver calls = %d, receipt calls = %d", report, adapter.calls, adapter.receipts)
	}
	entries, err := s.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusSent || entries[0].Attempts != 1 {
		t.Fatalf("entries = %+v, want one sent attempt", entries)
	}
}

func TestUnconfirmedWithoutReceiptStaysVisibleAfterReload(t *testing.T) {
	first := &fakeAdapter{errs: []error{Unconfirmed("ack lost")}}
	s, _ := newTestService(t, first)
	if result, err := s.Send(context.Background(), "coord", "pi-1", "hello", PriorityNext); err != nil || result.Status != StatusUnconfirmed {
		t.Fatalf("send result = %+v, err = %v", result, err)
	}
	second := &fakeAdapter{}
	reloaded := &Service{
		Box:      s.Box,
		Roster:   s.Roster,
		Adapters: map[Harness]Adapter{HarnessClaude: second, HarnessPi: second, HarnessCodex: second},
		Policy:   s.Policy,
		Now:      s.Now,
	}
	report, err := reloaded.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Unconfirmed != 1 || second.calls != 0 {
		t.Fatalf("report = %+v, redelivery calls = %d", report, second.calls)
	}
	before, err := os.ReadFile(reloaded.Box.logPath())
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := reloaded.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(reloaded.Box.logPath())
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Unconfirmed != 1 || !bytes.Equal(before, after) {
		t.Fatalf("repeated flush = %+v, mailbox changed without new receipt evidence", repeated)
	}
	entries, err := reloaded.Messages(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusUnconfirmed || entries[0].Attempts != 1 {
		t.Fatalf("entries = %+v, want one visible unconfirmed attempt", entries)
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

func TestQueuePersistsWithoutAttemptingDelivery(test *testing.T) {
	adapter := &fakeAdapter{}
	service, _ := newTestService(test, adapter)

	result, err := service.Queue(context.Background(), "coord", "pi-1", "hello", PriorityNext)
	if err != nil {
		test.Fatal(err)
	}
	if result.Status != StatusQueued || result.ID == "" {
		test.Fatalf("queue result = %+v, want a queued message with an id", result)
	}
	if adapter.calls != 0 {
		test.Fatalf("queue attempted delivery %d times", adapter.calls)
	}
	entries, err := service.Messages(nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusQueued || entries[0].Attempts != 0 {
		test.Fatalf("entries = %+v, want one unattempted queued message", entries)
	}
}

func TestQueueFlushDeliversExactlyOnce(test *testing.T) {
	adapter := &fakeAdapter{}
	service, _ := newTestService(test, adapter)
	ctx := context.Background()

	result, err := service.Queue(ctx, "coord", "pi-1", "hello", PriorityNext)
	if err != nil {
		test.Fatal(err)
	}
	report, err := service.Flush(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if report.Sent != 1 || adapter.calls != 1 {
		test.Fatalf("flush report = %+v, delivery calls = %d", report, adapter.calls)
	}
	report, err = service.Flush(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if report.Sent != 0 || adapter.calls != 1 {
		test.Fatalf("second flush report = %+v, delivery calls = %d", report, adapter.calls)
	}
	entries, err := service.Messages(nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Msg.ID != result.ID || entries[0].Status != StatusSent || entries[0].Attempts != 1 {
		test.Fatalf("entries = %+v, want one sent attempt", entries)
	}
}

func TestQueuePersistsRunIDsAndRefusesReplacementRecipient(test *testing.T) {
	adapter := &fakeAdapter{}
	service, _ := newTestService(test, adapter)
	setTestMemberRun(test, service, "coord", "coordinator-one")
	setTestMemberRun(test, service, "pi-1", "worker-one")

	result, err := service.Queue(context.Background(), "coord", "pi-1", "bounded task", PriorityNext)
	if err != nil {
		test.Fatal(err)
	}
	entries, err := service.Messages(nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Msg.ID != result.ID || entries[0].Msg.FromRunID != "coordinator-one" || entries[0].Msg.ToRunID != "worker-one" {
		test.Fatalf("queued message = %+v, want both run identities persisted", entries)
	}
	setTestMemberRun(test, service, "pi-1", "worker-two")

	report, err := service.Flush(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if report.Failed != 1 || report.Sent != 0 || adapter.calls != 0 {
		test.Fatalf("replacement received queued message: report = %+v, delivery calls = %d", report, adapter.calls)
	}
	entries, err = service.Messages(nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != StatusFailed || entries[0].Attempts != 0 {
		test.Fatalf("entries = %+v, want failed refusal without an attempt", entries)
	}
}

func TestQueuePreservesPolicyRejections(test *testing.T) {
	tests := []struct {
		name  string
		from  string
		to    string
		body  string
		want  error
		setup func(*Service) error
	}{
		{name: "duplicate", from: "coord", to: "pi-1", body: "same", want: ErrDuplicate, setup: func(service *Service) error {
			_, err := service.Queue(context.Background(), "coord", "pi-1", "same", PriorityNext)
			return err
		}},
		{name: "unknown recipient", from: "coord", to: "nobody", body: "hello", want: ErrUnknownWorker},
		{name: "malformed recipient", from: "coord", to: "../pi-1", body: "hello", want: ErrBadName},
		{name: "empty body", from: "coord", to: "pi-1", body: "\x00", want: ErrEmptyBody},
		{name: "oversized body", from: "coord", to: "pi-1", body: strings.Repeat("x", DefaultPolicy().MaxBody+1), want: ErrTooLarge},
		{name: "peer to peer", from: "pi-1", to: "codex-1", body: "hello", want: ErrPeerBlocked},
		{name: "rate limit", from: "coord", to: "pi-1", body: "second", want: ErrRateLimited, setup: func(service *Service) error {
			service.Policy.PerPairPerMinute = 1
			service.Policy.DedupeWindow = 0
			_, err := service.Queue(context.Background(), "coord", "pi-1", "first", PriorityNext)
			return err
		}},
	}
	for _, testCase := range tests {
		test.Run(testCase.name, func(test *testing.T) {
			service, _ := newTestService(test, &fakeAdapter{})
			if testCase.setup != nil {
				if err := testCase.setup(service); err != nil {
					test.Fatal(err)
				}
			}
			_, err := service.Queue(context.Background(), testCase.from, testCase.to, testCase.body, PriorityNext)
			if !errors.Is(err, testCase.want) {
				test.Fatalf("Queue(%q, %q) error = %v, want %v", testCase.from, testCase.to, err, testCase.want)
			}
		})
	}
}

func TestQueuedFlushUsesReplyBinaryPersistedAtEnqueue(t *testing.T) {
	adapter := &fakeAdapter{errs: []error{NotReady("not started")}}
	service, clock := newTestService(t, adapter)
	first := "/tmp/forge one"
	second := "/tmp/forge two"
	service.ReplyBinary = first
	if result, err := service.Send(context.Background(), "coord", "pi-1", "hello", PriorityNext); err != nil || result.Status != StatusQueued {
		t.Fatalf("send result = %+v, err = %v", result, err)
	}
	service.ReplyBinary = second
	clock.add(6 * time.Second)
	if report, err := service.Flush(context.Background()); err != nil || report.Sent != 1 {
		t.Fatalf("flush report = %+v, err = %v", report, err)
	}
	if len(adapter.texts) != 2 {
		t.Fatalf("adapter received %d renders, want 2", len(adapter.texts))
	}
	if !strings.Contains(adapter.texts[1], "'/tmp/forge one' surface send") || strings.Contains(adapter.texts[1], second) {
		t.Fatalf("flushed render did not retain its original reply binary: %q", adapter.texts[1])
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

func TestApplyEventRejectsMissingOrStaleRunIDBeforeMutation(t *testing.T) {
	s, _ := newTestService(t, &fakeAdapter{})
	if err := s.Roster.Update("codex-1", func(worker *Worker) error {
		worker.RunID = "run-1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"", "run-old"} {
		if _, err := s.ApplyEvent(context.Background(), Event{Worker: "codex-1", State: StateIdle, ThreadID: "th_stale", RunID: runID}); !errors.Is(err, ErrRunIDMismatch) {
			t.Fatalf("run id %q: err = %v, want ErrRunIDMismatch", runID, err)
		}
		worker, err := s.Roster.Get("codex-1")
		if err != nil {
			t.Fatal(err)
		}
		if worker.State != "" || worker.ThreadID != "" {
			t.Fatalf("run id %q mutated worker = %+v", runID, worker)
		}
	}
	if _, err := s.ApplyEvent(context.Background(), Event{Worker: "codex-1", State: StateIdle, ThreadID: "th_current", RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	worker, err := s.Roster.Get("codex-1")
	if err != nil {
		t.Fatal(err)
	}
	if worker.State != StateIdle || worker.ThreadID != "th_current" {
		t.Fatalf("matching run did not apply: %+v", worker)
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

// Once a Codex worker's thread is known, a notify from another thread leaves
// the worker alone: no idle, no new thread id, no watcher notice.
func TestCodexNotifyFromAnotherThreadIsIgnored(t *testing.T) {
	s, _ := newTestService(t, &fakeAdapter{})
	ctx := context.Background()
	if _, err := s.ApplyEvent(ctx, Event{Worker: "codex-1", State: StateIdle, ThreadID: "th_worker"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Roster.Update("codex-1", func(w *Worker) error { w.State = StateBusy; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Watch("codex-1", "coord"); err != nil {
		t.Fatal(err)
	}
	_, err := s.ApplyEvent(ctx, Event{Worker: "codex-1", State: StateIdle, ThreadID: "th_other"})
	if !errors.Is(err, ErrNotTurnEvent) {
		t.Fatalf("err %v, want ErrNotTurnEvent", err)
	}
	w, err := s.Roster.Get("codex-1")
	if err != nil {
		t.Fatal(err)
	}
	if w.ThreadID != "th_worker" || w.State != StateBusy || len(w.Watchers) != 1 {
		t.Fatalf("worker %+v, want it untouched", w)
	}
}
