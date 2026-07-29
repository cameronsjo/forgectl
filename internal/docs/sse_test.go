package docs

// Test plan for server.go's live-reload endpoint (handleEvents)
//
// These use httptest.NewServer rather than httptest.NewRecorder: a recorder
// cannot exercise flushing or streaming at all — it buffers the whole response
// and only "completes" when the handler returns, which an SSE handler never
// does on its own. A recorder-based test would deadlock, or pass while the real
// endpoint buffered forever.
//
// handleEvents (Classification: API handler — streaming)
//   [x] Happy: the endpoint responds 200 with Content-Type: text/event-stream
//   [x] Happy: a connected client receives a published reload as an SSE data frame
//   [x] Happy: the handler returns when the Broker is closed (this is what lets
//              http.Server.Shutdown complete promptly instead of blocking the
//              full grace period on an open stream)
//   [x] Happy: a disconnecting client unregisters itself from the Broker
//   [x] Happy: the endpoint 404s when live reload is not enabled (nil Broker)
//   [x] Happy: the response carries no-store, so a reload stream is never cached
//   [x] Happy: the shell template links the reload client (the browser half is
//              wired, not just embedded)

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer starts a real HTTP server over a docs handler and returns it plus
// the broker feeding its /events endpoint.
func sseServer(t *testing.T) (*httptest.Server, *Broker) {
	t.Helper()

	idx, _ := testIndex(t)
	broker := NewBroker()
	srv := httptest.NewServer(NewHandler(NewStore(idx), broker))
	t.Cleanup(func() {
		broker.Close()
		srv.Close()
	})
	return srv, broker
}

// openStream connects to /events and returns the response plus a reader
// positioned after the server's initial ": connected" comment frame, so the
// stream is proven live before the test publishes anything.
func openStream(t *testing.T, srv *httptest.Server) (*http.Response, *bufio.Reader) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+eventsPath, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := srv.Client().Do(req) //nolint:bodyclose // closed via t.Cleanup below
	if err != nil {
		t.Fatalf("GET %s: %v", eventsPath, err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(resp.Body)
	consumeFrame(t, reader, ": connected")
	return resp, reader
}

// consumeFrame reads one whole SSE frame and asserts its payload line starts
// with wantPrefix. An SSE frame is terminated by a BLANK line, so a frame costs
// two reads, not one — consuming only the payload leaves the reader parked on
// the terminator, and the next read then returns "\n", which is
// indistinguishable from the server having sent an empty frame.
func consumeFrame(t *testing.T, reader *bufio.Reader, wantPrefix string) {
	t.Helper()

	payload, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading SSE frame payload: %v", err)
	}
	if !strings.HasPrefix(payload, wantPrefix) {
		t.Fatalf("SSE frame = %q, want a frame starting %q", payload, wantPrefix)
	}
	terminator, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading SSE frame terminator: %v", err)
	}
	if strings.TrimRight(terminator, "\r\n") != "" {
		t.Fatalf("SSE frame terminator = %q, want a blank line", terminator)
	}
}

func TestEvents_Connect_RespondsWithEventStreamContentType(t *testing.T) {
	srv, _ := sseServer(t)
	resp, _ := openStream(t, srv)

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", got, "text/event-stream")
	}
}

func TestEvents_ResponseIsNotCacheable(t *testing.T) {
	srv, _ := sseServer(t)
	resp, _ := openStream(t, srv)

	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
}

func TestEvents_PublishedReload_ArrivesAsDataFrame(t *testing.T) {
	srv, broker := sseServer(t)
	_, reader := openStream(t, srv)

	// The stream is already open (the comment frame proved it), so this Publish
	// cannot race ahead of the subscription.
	broker.Publish(reloadMessage)

	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		got <- result{line, err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("reading the data frame: %v", r.err)
		}
		want := "data: " + reloadMessage
		if strings.TrimRight(r.line, "\r\n") != want {
			t.Errorf("frame = %q, want %q", r.line, want)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("no SSE data frame within %s of Publish", recvTimeout)
	}
}

func TestEvents_BrokerClose_ReleasesTheHandler(t *testing.T) {
	srv, broker := sseServer(t)
	_, reader := openStream(t, srv)

	if got := broker.Subscribers(); got != 1 {
		t.Fatalf("Subscribers() = %d, want 1 — the stream did not register", got)
	}

	// Closing the broker must end the handler, which ends the response body.
	// Without this, http.Server.Shutdown blocks for the whole grace period on
	// every open reader tab and Ctrl-C appears to hang.
	broker.Close()

	done := make(chan error, 1)
	go func() {
		_, err := reader.ReadString('\n')
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("read succeeded after Broker.Close, want EOF — the handler did not return")
		}
	case <-time.After(recvTimeout):
		t.Fatalf("stream still open %s after Broker.Close; Shutdown would block on it", recvTimeout)
	}
}

func TestEvents_ClientDisconnect_UnregistersSubscriber(t *testing.T) {
	srv, broker := sseServer(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+eventsPath, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", eventsPath, err)
	}
	reader := bufio.NewReader(resp.Body)
	consumeFrame(t, reader, ": connected")
	if got := broker.Subscribers(); got != 1 {
		t.Fatalf("Subscribers() = %d, want 1", got)
	}

	resp.Body.Close()

	// The handler notices via request-context cancellation, which is inherently
	// asynchronous — poll rather than assert instantly.
	deadline := time.Now().Add(recvTimeout)
	for time.Now().Before(deadline) {
		if broker.Subscribers() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Subscribers() = %d after client disconnect, want 0 — a closed tab leaks a channel the watcher keeps sending to", broker.Subscribers())
}

func TestEvents_NilBroker_404s(t *testing.T) {
	idx, _ := testIndex(t)
	h := NewHandler(NewStore(idx), nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, eventsPath, nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d when live reload is not enabled", rec.Code, http.StatusNotFound)
	}
}

func TestShell_LinksTheReloadClient(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !strings.Contains(rec.Body.String(), `src="/assets/reload.js"`) {
		t.Errorf("shell does not link the reload client; embedding it is not enough to make live reload work. Body: %s", rec.Body.String())
	}
}
