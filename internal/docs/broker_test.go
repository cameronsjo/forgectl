package docs

// Test plan for broker.go
//
// Broker (Classification: pure concurrency primitive — run under -race)
//   [x] Happy: a subscriber receives a published message
//   [x] Happy: every subscriber receives the same published message (fan-out)
//   [x] Happy: the cancel func unsubscribes, and later Publishes are not delivered
//   [x] Happy: Publish does not block when a subscriber's buffer is already full
//   [x] Happy: Close closes every subscriber channel (this is what unblocks
//              http.Server.Shutdown — see Broker's doc comment)
//   [x] Happy: Close is idempotent
//   [x] Happy: Subscribe after Close returns an already-closed channel
//   [x] Happy: Publish after Close is a no-op rather than a panic on a closed channel
//   [x] Happy: concurrent Subscribe/Publish/cancel is race-free

import (
	"sync"
	"testing"
	"time"
)

// recvTimeout is how long a test waits for a message that should already be
// in flight. Generous because CI machines stall, but bounded so a genuine
// missing-delivery bug fails rather than hanging the suite.
const recvTimeout = 2 * time.Second

func recvOne(t *testing.T, ch <-chan string) (string, bool) {
	t.Helper()
	select {
	case msg, ok := <-ch:
		return msg, ok
	case <-time.After(recvTimeout):
		t.Fatalf("timed out after %s waiting for a broker message", recvTimeout)
		return "", false
	}
}

func TestBroker_Publish_SubscriberReceives(t *testing.T) {
	b := NewBroker()
	sub, cancel := b.Subscribe()
	defer cancel()

	b.Publish("reload")

	msg, ok := recvOne(t, sub)
	if !ok {
		t.Fatal("channel closed, want a delivered message")
	}
	if msg != "reload" {
		t.Errorf("msg = %q, want %q", msg, "reload")
	}
}

func TestBroker_Publish_AllSubscribersReceive(t *testing.T) {
	b := NewBroker()
	subA, cancelA := b.Subscribe()
	defer cancelA()
	subB, cancelB := b.Subscribe()
	defer cancelB()

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("Subscribers() = %d, want 2", got)
	}
	b.Publish("reload")

	for name, sub := range map[string]<-chan string{"A": subA, "B": subB} {
		msg, ok := recvOne(t, sub)
		if !ok || msg != "reload" {
			t.Errorf("subscriber %s got (%q, ok=%v), want (%q, ok=true)", name, msg, ok, "reload")
		}
	}
}

func TestBroker_Cancel_StopsDelivery(t *testing.T) {
	b := NewBroker()
	sub, cancel := b.Subscribe()

	cancel()

	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d after cancel, want 0", got)
	}
	// cancel closes the channel, so the receive returns immediately with ok=false.
	if msg, ok := recvOne(t, sub); ok {
		t.Errorf("received (%q, ok=true) after cancel, want a closed channel", msg)
	}

	b.Publish("reload") // must not panic on the closed channel
}

func TestBroker_PublishWithFullBuffer_DoesNotBlock(t *testing.T) {
	b := NewBroker()
	sub, cancel := b.Subscribe()
	defer cancel()

	// subscriberBuffer is 1, so the first Publish fills it and the rest must be
	// dropped rather than blocking the caller (the watcher goroutine).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish("reload")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(recvTimeout):
		t.Fatalf("Publish blocked with a full subscriber buffer (waited %s); a wedged browser tab would stall the watcher", recvTimeout)
	}

	if msg, ok := recvOne(t, sub); !ok || msg != "reload" {
		t.Errorf("buffered message = (%q, ok=%v), want (%q, ok=true)", msg, ok, "reload")
	}
}

func TestBroker_Close_ClosesSubscriberChannels(t *testing.T) {
	b := NewBroker()
	subA, cancelA := b.Subscribe()
	defer cancelA()
	subB, cancelB := b.Subscribe()
	defer cancelB()

	b.Close()

	for name, sub := range map[string]<-chan string{"A": subA, "B": subB} {
		if msg, ok := recvOne(t, sub); ok {
			t.Errorf("subscriber %s received (%q, ok=true) after Close, want a closed channel; an open SSE stream would block Shutdown for the full grace period", name, msg)
		}
	}
	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d after Close, want 0", got)
	}
}

func TestBroker_Close_IsIdempotent(t *testing.T) {
	b := NewBroker()
	_, cancel := b.Subscribe()
	defer cancel()

	b.Close()
	b.Close() // must not panic double-closing a subscriber channel
}

func TestBroker_SubscribeAfterClose_ReturnsClosedChannel(t *testing.T) {
	b := NewBroker()
	b.Close()

	sub, cancel := b.Subscribe()
	defer cancel()

	if msg, ok := recvOne(t, sub); ok {
		t.Errorf("received (%q, ok=true) from a post-Close subscription, want a closed channel", msg)
	}
	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d, want 0 — a post-Close subscription must not be registered", got)
	}
}

func TestBroker_ConcurrentUse_IsRaceFree(t *testing.T) {
	b := NewBroker()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, cancel := b.Subscribe()
			select {
			case <-sub:
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish("reload")
		}()
	}

	wg.Wait()
	b.Close()
}
