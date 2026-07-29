package docs

import "sync"

// subscriberBuffer is how many reload notifications a single subscriber may
// fall behind by before Publish starts dropping them for that subscriber.
// Dropping is correct here rather than blocking: every notification carries
// the same instruction ("re-read the page"), so a subscriber with one pending
// message already knows everything a second would tell it. Blocking instead
// would let one wedged browser tab stall the watcher goroutine.
const subscriberBuffer = 1

// Broker fans reload notifications out to every connected SSE subscriber.
//
// The reason this is a named type rather than a bare channel is Close: an SSE
// response never completes on its own, so http.Server.Shutdown — which waits
// for "in-flight requests to finish" — will block the FULL shutdown grace
// period on every open stream. Long-lived streams are simply not the kind of
// request Shutdown was designed to drain. Close breaks that deadlock by
// closing every subscriber channel, which makes each /events handler's read
// return immediately so the handler can return and Shutdown can see the
// connection go idle. Callers MUST Close the broker BEFORE calling Shutdown,
// not after; the ordering is the whole point, and getting it backwards turns
// Ctrl-C into a guaranteed multi-second hang with no error to explain it.
type Broker struct {
	mu     sync.Mutex
	subs   map[chan string]struct{}
	closed bool
}

// NewBroker returns a Broker with no subscribers.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan string]struct{})}
}

// Subscribe registers a new subscriber and returns its channel plus a
// cancel func that unregisters it. The handler MUST call cancel (defer it) so
// a disconnected browser doesn't leak a channel the watcher keeps sending to.
//
// Subscribing to an already-Closed Broker returns a closed channel rather than
// an error: the caller's job either way is to stop reading and return, and a
// closed channel already says exactly that through the normal receive path.
// This also removes a shutdown race — a request that arrives between Close and
// the listener closing cannot register a subscriber that nothing will ever
// close.
func (b *Broker) Subscribe() (<-chan string, func()) {
	ch := make(chan string, subscriberBuffer)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		close(ch)
		return ch, func() {}
	}

	b.subs[ch] = struct{}{}
	return ch, func() { b.unsubscribe(ch) }
}

// unsubscribe removes ch and closes it. Safe to call more than once — the map
// membership check makes the second call a no-op, so a handler that both defers
// cancel and returns via a Close-triggered path cannot double-close.
func (b *Broker) unsubscribe(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subs[ch]; !ok {
		return
	}
	delete(b.subs, ch)
	close(ch)
}

// Publish delivers msg to every current subscriber, skipping any whose buffer
// is already full (see subscriberBuffer). Publish never blocks and is safe to
// call after Close, where it does nothing.
func (b *Broker) Publish(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	for ch := range b.subs {
		select {
		case ch <- msg:
		default: // subscriber already has a pending reload; one is enough
		}
	}
}

// Close closes every subscriber channel and refuses future subscriptions,
// releasing all open SSE handlers so the HTTP server can shut down promptly.
// Idempotent.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

// Subscribers reports the current subscriber count. Exists for tests and
// diagnostics; request paths have no reason to consult it.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
