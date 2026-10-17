package mail

import (
	"context"
	"testing"
	"time"
)

func setTestMemberRun(test *testing.T, service *Service, member, runID string) {
	test.Helper()
	if err := service.Roster.Update(member, func(worker *Worker) error {
		worker.RunID = runID
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestQueuedMessageDoesNotMoveToReplacementRun(test *testing.T) {
	adapter := &fakeAdapter{errs: []error{NotReady("busy")}}
	service, clock := newTestService(test, adapter)
	setTestMemberRun(test, service, "coord", "coordinator-one")
	setTestMemberRun(test, service, "pi-1", "worker-one")
	result, err := service.Send(context.Background(), "coord", "pi-1", "bounded task", PriorityNext)
	if err != nil || result.Status != StatusQueued {
		test.Fatalf("send=%+v err=%v", result, err)
	}
	setTestMemberRun(test, service, "pi-1", "worker-two")
	clock.add(6 * time.Second)
	report, err := service.Flush(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if report.Failed != 1 || report.Sent != 0 || adapter.calls != 1 {
		test.Fatalf("replacement received the queued message: report=%+v deliveries=%d", report, adapter.calls)
	}
	entries, err := service.Messages(nil)
	if err != nil || len(entries) != 1 || entries[0].Msg.FromRunID != "coordinator-one" || entries[0].Msg.ToRunID != "worker-one" || entries[0].Status != StatusFailed || entries[0].Attempts != 1 {
		test.Fatalf("message lost run provenance or counted a nonexistent attempt: entries=%+v err=%v", entries, err)
	}
}

func TestQueuedMessageAllowsSameRunSessionDiscovery(test *testing.T) {
	adapter := &fakeAdapter{errs: []error{NotReady("session not known")}}
	service, clock := newTestService(test, adapter)
	setTestMemberRun(test, service, "coord", "coordinator-one")
	setTestMemberRun(test, service, "pi-1", "worker-one")
	result, err := service.Send(context.Background(), "coord", "pi-1", "bounded task", PriorityNext)
	if err != nil || result.Status != StatusQueued {
		test.Fatalf("send=%+v err=%v", result, err)
	}
	if err := service.Roster.Update("pi-1", func(worker *Worker) error {
		worker.ThreadID = "native-discovered"
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	clock.add(6 * time.Second)
	report, err := service.Flush(context.Background())
	if err != nil || report.Sent != 1 || adapter.calls != 2 {
		test.Fatalf("same-run discovery broke delivery: report=%+v deliveries=%d err=%v", report, adapter.calls, err)
	}
}

func TestUnconfirmedMessageDoesNotQueryReplacementReceipt(test *testing.T) {
	adapter := &receiptTestAdapter{fakeAdapter: fakeAdapter{errs: []error{Unconfirmed("lost acknowledgement")}}, accepted: true}
	service, _ := newTestService(test, adapter)
	setTestMemberRun(test, service, "coord", "coordinator-one")
	setTestMemberRun(test, service, "pi-1", "worker-one")
	result, err := service.Send(context.Background(), "coord", "pi-1", "bounded task", PriorityNext)
	if err != nil || result.Status != StatusUnconfirmed {
		test.Fatalf("send=%+v err=%v", result, err)
	}
	setTestMemberRun(test, service, "pi-1", "worker-two")
	report, err := service.Flush(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if report.Unconfirmed != 1 || report.Sent != 0 || adapter.receipts != 0 || adapter.calls != 1 {
		test.Fatalf("replacement confirmed another run: report=%+v receipts=%d deliveries=%d", report, adapter.receipts, adapter.calls)
	}
}
