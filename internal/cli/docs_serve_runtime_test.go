package cli

// Test plan for runDocsServeWithRuntime (Classification: server lifecycle —
// decides whether a record naming this server ever becomes visible).
//
// Every row drives the real helper with a complete injected runtime. No mutable
// package global, no filesystem permission games, no wall-clock sleeps standing
// in for ordering.
//
//   [x] an invalid advertised address serves with NO identity route and makes
//       exactly zero newInfo/probe/publish/lease calls
//   [x] a serversDir failure takes the same zero-call branch
//   [x] an initial newInfo failure happens BEFORE Serve and publishes nothing
//   [x] a successful probe always precedes the first publish
//   [x] a probe failure is a startup failure: closeServer once, no publish
//   [x] cancellation at the readiness barrier publishes nothing and returns nil
//   [x] a collision retries with a NEW generation and publishes only the second
//   [x] eight collisions exhaust the budget: exactly 8 publishes, 8 probes,
//       8 newInfo calls, one warning, no ninth newInfo, serving continues
//   [x] collision-retry newInfo failure after k=1 and k=7: exactly k publishes,
//       k probes, k+1 newInfo calls, one warning, no lease, serving continues,
//       and the later Shutdown result is what is returned
//   [x] cancellation observed when publish returns suppresses retry AND the
//       continuation warning
//   [x] an ordinary publish error warns once and keeps serving
//   [x] a nil lease with a nil error is a fixed internal-contract failure
//   [x] Publication.Warning is surfaced exactly once and the lease still closes
//   [x] steady-state cancellation calls Shutdown once and closes the lease once
//   [x] a lease-close failure warns and never replaces the primary result
//   [x] a mixed runtime with a REAL listener, Serve, identity middleware and
//       probe proves the atomic generation source actually changed after a
//       collision — not merely that a fake probe saw a new argument

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// stubListener stands in for a bound socket. Serve is faked too, so Accept is
// never called; the only thing the lifecycle reads is Addr.
type stubListener struct {
	addr   net.Addr
	closed bool
	mu     sync.Mutex
}

func (l *stubListener) Accept() (net.Conn, error) { return nil, errors.New("stub listener") }
func (l *stubListener) Addr() net.Addr            { return l.addr }
func (l *stubListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func loopbackListenerAddr(port int) net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
}

type fakePublication struct {
	publication docspkg.Publication
	err         error
}

type fakeInfoResult struct {
	info docspkg.ServerInfo
	err  error
}

// fakeServeRuntime records every lifecycle call and hands back scripted results.
//
// The scripts are consumed IN ORDER and the counters are exact, because most of
// what this test file asserts is "how many times", not "with what": the state
// machine's contract is written in call counts.
type fakeServeRuntime struct {
	mu     sync.Mutex
	events []string

	listener      net.Listener
	serversDirErr error
	serversDir    string

	infoScript    []fakeInfoResult
	probeScript   []error
	publishScript []fakePublication

	shutdownErr error
	leaseErr    error

	// serveGate blocks the fake Serve until the test releases it, so a test can
	// keep the server "running" for as long as it needs.
	serveGate chan error
	// publishHook runs inside publish, before its result is returned, so a test
	// can make a higher-priority event ready mid-call.
	publishHook func(attempt int)

	newInfoCalls     int
	probeCalls       int
	publishCalls     int
	closeServerCalls int
	shutdownCalls    int
	leaseCloseCalls  int

	probedGenerations []string
}

func (f *fakeServeRuntime) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, args...))
}

func (f *fakeServeRuntime) eventLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeServeRuntime) counts() (newInfo, probe, publish, closeServer, shutdown, leaseClose int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newInfoCalls, f.probeCalls, f.publishCalls, f.closeServerCalls, f.shutdownCalls, f.leaseCloseCalls
}

func (f *fakeServeRuntime) runtime() docsServeRuntime {
	return docsServeRuntime{
		listen: func(string) (net.Listener, error) { return f.listener, nil },
		serversDir: func() (string, error) {
			if f.serversDirErr != nil {
				return "", f.serversDirErr
			}
			return f.serversDir, nil
		},
		newInfo: func(addr, token string) (docspkg.ServerInfo, error) {
			f.mu.Lock()
			index := f.newInfoCalls
			f.newInfoCalls++
			f.mu.Unlock()
			f.record("newInfo")
			if index >= len(f.infoScript) {
				return docspkg.ServerInfo{}, fmt.Errorf("unscripted newInfo call #%d", index+1)
			}
			result := f.infoScript[index]
			if result.err != nil {
				return docspkg.ServerInfo{}, result.err
			}
			result.info.Addr = addr
			result.info.Token = token
			return result.info, nil
		},
		probe: func(ctx context.Context, addr, generation string) error {
			f.mu.Lock()
			index := f.probeCalls
			f.probeCalls++
			f.probedGenerations = append(f.probedGenerations, generation)
			f.mu.Unlock()
			f.record("probe %s", generation)
			if index >= len(f.probeScript) {
				// An unscripted probe blocks until its child context is
				// cancelled, which is how a test parks the readiness barrier.
				<-ctx.Done()
				return ctx.Err()
			}
			return f.probeScript[index]
		},
		publish: func(_ string, info docspkg.ServerInfo) (docspkg.Publication, error) {
			f.mu.Lock()
			index := f.publishCalls
			f.publishCalls++
			f.mu.Unlock()
			f.record("publish %s", info.Generation)
			if f.publishHook != nil {
				f.publishHook(index + 1)
			}
			if index >= len(f.publishScript) {
				return docspkg.Publication{}, fmt.Errorf("unscripted publish call #%d", index+1)
			}
			result := f.publishScript[index]
			return result.publication, result.err
		},
		serve: func(*http.Server, net.Listener) error {
			f.record("serve")
			return <-f.serveGate
		},
		closeServer: func(*http.Server) error {
			f.mu.Lock()
			f.closeServerCalls++
			f.mu.Unlock()
			f.record("closeServer")
			// A real Close makes Serve return; the fake does the same so the
			// lifecycle's background wait can complete.
			select {
			case f.serveGate <- http.ErrServerClosed:
			default:
			}
			return nil
		},
		shutdown: func(*http.Server, context.Context) error {
			f.mu.Lock()
			f.shutdownCalls++
			f.mu.Unlock()
			f.record("shutdown")
			select {
			case f.serveGate <- http.ErrServerClosed:
			default:
			}
			return f.shutdownErr
		},
		closeLease: func(*docspkg.ServerLease) error {
			f.mu.Lock()
			f.leaseCloseCalls++
			f.mu.Unlock()
			f.record("closeLease")
			return f.leaseErr
		},
	}
}

func newFakeServeRuntime(port int) *fakeServeRuntime {
	return &fakeServeRuntime{
		listener:   &stubListener{addr: loopbackListenerAddr(port)},
		serversDir: "/nonexistent/docs-servers",
		serveGate:  make(chan error, 1),
	}
}

func testGenerationInfo(seed byte) docspkg.ServerInfo {
	raw := make([]byte, 16)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return docspkg.ServerInfo{
		SchemaVersion: 1,
		Generation:    fmt.Sprintf("%x", raw),
		StartedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
}

// leasePlaceholder is a non-nil lease the fake closeLease consumes. Its zero
// value is never really closed, because closeLease is injected.
func leasePlaceholder() *docspkg.ServerLease { return &docspkg.ServerLease{} }

// serveHarness runs runDocsServeWithRuntime in the background and gives the
// test the handles it needs to drive and observe it.
type serveHarness struct {
	fake   *fakeServeRuntime
	cancel context.CancelFunc
	done   chan error
	stdout *lockedBuffer
	stderr *lockedBuffer
}

func startServeHarness(t *testing.T, fake *fakeServeRuntime) *serveHarness {
	t.Helper()
	idx := testDocsIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetContext(ctx)

	h := &serveHarness{fake: fake, cancel: cancel, done: make(chan error, 1), stdout: stdout, stderr: stderr}
	go func() {
		h.done <- runDocsServeWithRuntime(cmd, module.Deps{Runner: &exec.FakeRunner{}}, idx, "127.0.0.1:0", false, "", fake.runtime())
	}()
	return h
}

// wait returns the lifecycle's result, failing the test if it never returns.
func (h *serveHarness) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(shutdownWaitBudget):
		t.Fatalf("runDocsServeWithRuntime did not return within 10s; events: %v", h.fake.eventLog())
		return nil
	}
}

// waitForBanner blocks until the server has printed its address line, which it
// does only after publication has resolved one way or another.
func (h *serveHarness) waitForBanner(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.stdout.String(), "Ctrl-C to stop") {
			return
		}
		select {
		case err := <-h.done:
			h.done <- err
			t.Fatalf("the server returned %v before printing its banner; events: %v", err, h.fake.eventLog())
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the server never printed its banner; events: %v", h.fake.eventLog())
}

// ---------------------------------------------------------------------------
// zero-discovery-call branches
// ---------------------------------------------------------------------------

func TestRunDocsServeWithRuntime_IneligibleBranches_MakeZeroDiscoveryCalls(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeServeRuntime)
		wantWarn  string
	}{
		{
			name: "invalid advertised address",
			// A Unix socket address cannot be normalized into a locally
			// connectable host:port, so no record could ever name this server.
			configure: func(f *fakeServeRuntime) {
				f.listener = &stubListener{addr: &net.UnixAddr{Name: "/tmp/docs.sock", Net: "unix"}}
			},
			wantWarn: "cannot be published for discovery",
		},
		{
			name:      "discovery directory unavailable",
			configure: func(f *fakeServeRuntime) { f.serversDirErr = errors.New("no config dir") },
			wantWarn:  "discovery directory cannot be located",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeServeRuntime(3590)
			tc.configure(fake)

			h := startServeHarness(t, fake)
			h.waitForBanner(t)
			h.cancel()

			if err := h.wait(t); err != nil {
				t.Fatalf("runDocsServeWithRuntime = %v, want nil", err)
			}

			newInfo, probe, publish, _, shutdown, leaseClose := fake.counts()
			if newInfo != 0 || probe != 0 || publish != 0 || leaseClose != 0 {
				t.Errorf("discovery calls = (newInfo %d, probe %d, publish %d, closeLease %d), want all zero — an ineligible server must never touch discovery",
					newInfo, probe, publish, leaseClose)
			}
			if shutdown != 1 {
				t.Errorf("shutdown calls = %d, want 1", shutdown)
			}
			if !strings.Contains(h.stderr.String(), tc.wantWarn) {
				t.Errorf("stderr = %q, want it to name %q exactly once", h.stderr.String(), tc.wantWarn)
			}
			if got := strings.Count(h.stderr.String(), "forgectl docs open` will not find this server"); got != 1 {
				t.Errorf("discovery warning printed %d times, want exactly 1", got)
			}
		})
	}
}

func TestRunDocsServeWithRuntime_IneligibleBranch_ServesWithoutAnIdentityRoute(t *testing.T) {
	// A server that can never publish must not answer the freshness endpoint:
	// a 204 there would imply a discoverability it does not have.
	idx := testDocsIndex(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	rt := productionDocsServeRuntime()
	rt.listen = func(string) (net.Listener, error) { return ln, nil }
	rt.serversDir = func() (string, error) { return "", errors.New("no config dir") }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetContext(ctx)

	done := make(chan error, 1)
	go func() {
		done <- runDocsServeWithRuntime(cmd, module.Deps{Runner: &exec.FakeRunner{}}, idx, "127.0.0.1:0", false, "", rt)
	}()

	addr := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && addr == "" {
		addr = parseServedAddr(stdout.String())
		time.Sleep(2 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("the server never printed its address; stderr=%q", stderr.String())
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/.well-known/forgectl-docs")
	if err != nil {
		t.Fatalf("GET the identity path: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // only the status is under test
	if resp.StatusCode == http.StatusNoContent {
		t.Errorf("the identity endpoint answered 204 on a server that can never publish a record")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown = %v, want nil", err)
		}
	case <-time.After(shutdownWaitBudget):
		t.Fatal("the server did not shut down")
	}
}

// ---------------------------------------------------------------------------
// startup failures
// ---------------------------------------------------------------------------

func TestRunDocsServeWithRuntime_InitialNewInfoError_HappensBeforeServe(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{err: errors.New("entropy source failed")}}

	h := startServeHarness(t, fake)
	err := h.wait(t)
	if !errors.Is(err, errDocsServeGeneration) {
		t.Fatalf("runDocsServeWithRuntime = %v, want the fixed generation error", err)
	}
	if strings.Contains(err.Error(), "entropy source failed") {
		t.Errorf("the returned error leaks the underlying cause: %v", err)
	}

	newInfo, probe, publish, closeServer, shutdown, leaseClose := fake.counts()
	if newInfo != 1 || probe != 0 || publish != 0 || closeServer != 0 || shutdown != 0 || leaseClose != 0 {
		t.Errorf("calls = (newInfo %d, probe %d, publish %d, closeServer %d, shutdown %d, closeLease %d), want only one newInfo",
			newInfo, probe, publish, closeServer, shutdown, leaseClose)
	}
	for _, event := range fake.eventLog() {
		if event == "serve" {
			t.Error("Serve was started despite the initial generation failing — the failure must precede it")
		}
	}
}

func TestRunDocsServeWithRuntime_ProbeFailure_IsAStartupFailure(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0x10)}}
	fake.probeScript = []error{errors.New("no answer at 127.0.0.1:3590")}

	h := startServeHarness(t, fake)
	err := h.wait(t)
	if !errors.Is(err, errDocsServeProbe) {
		t.Fatalf("runDocsServeWithRuntime = %v, want the fixed probe error", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1:3590") {
		t.Errorf("the returned error leaks the probed address: %v", err)
	}

	_, probe, publish, closeServer, shutdown, leaseClose := fake.counts()
	if probe != 1 || publish != 0 || closeServer != 1 || shutdown != 0 || leaseClose != 0 {
		t.Errorf("calls = (probe %d, publish %d, closeServer %d, shutdown %d, closeLease %d), want one probe, one closeServer, and nothing else",
			probe, publish, closeServer, shutdown, leaseClose)
	}
}

func TestRunDocsServeWithRuntime_CancelAtReadinessBarrier_PublishesNothing(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0x11)}}
	// No probe script: the probe parks until its context is cancelled.

	h := startServeHarness(t, fake)
	// Wait for the probe to be in flight, then cancel. Cancellation outranks
	// the parked probe, so nothing may be published.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, probe, _, _, _, _ := fake.counts(); probe == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.cancel()

	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil — cancelling during startup is a normal stop", err)
	}
	_, _, publish, closeServer, shutdown, leaseClose := fake.counts()
	if publish != 0 || leaseClose != 0 {
		t.Errorf("publish = %d, closeLease = %d, want both zero", publish, leaseClose)
	}
	if closeServer != 1 || shutdown != 0 {
		t.Errorf("closeServer = %d, shutdown = %d, want one Close and no Shutdown before steady state", closeServer, shutdown)
	}
}

// ---------------------------------------------------------------------------
// probe-before-publish ordering
// ---------------------------------------------------------------------------

func TestRunDocsServeWithRuntime_ProbePrecedesPublish(t *testing.T) {
	info := testGenerationInfo(0x20)
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: info}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{publication: docspkg.Publication{Lease: leasePlaceholder()}}}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil", err)
	}

	events := fake.eventLog()
	probeAt, publishAt := -1, -1
	for i, event := range events {
		if probeAt < 0 && strings.HasPrefix(event, "probe ") {
			probeAt = i
		}
		if publishAt < 0 && strings.HasPrefix(event, "publish ") {
			publishAt = i
		}
	}
	if probeAt < 0 || publishAt < 0 || probeAt > publishAt {
		t.Fatalf("events = %v, want a successful probe before the first publish — a record must never name a listener that has not answered for it", events)
	}
	if events[probeAt] != "probe "+info.Generation || events[publishAt] != "publish "+info.Generation {
		t.Errorf("events = %v, want the probe and the publish to name the same generation", events)
	}
}

// ---------------------------------------------------------------------------
// collision retry
// ---------------------------------------------------------------------------

func TestRunDocsServeWithRuntime_Collision_RetriesWithANewGeneration(t *testing.T) {
	first, second := testGenerationInfo(0x30), testGenerationInfo(0x40)
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: first}, {info: second}}
	fake.probeScript = []error{nil, nil}
	fake.publishScript = []fakePublication{
		{err: docspkg.ErrGenerationCollision},
		{publication: docspkg.Publication{Lease: leasePlaceholder()}},
	}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil", err)
	}

	newInfo, probe, publish, _, _, leaseClose := fake.counts()
	if newInfo != 2 || probe != 2 || publish != 2 {
		t.Errorf("calls = (newInfo %d, probe %d, publish %d), want 2 of each", newInfo, probe, publish)
	}
	if leaseClose != 1 {
		t.Errorf("closeLease = %d, want exactly 1", leaseClose)
	}
	fake.mu.Lock()
	probed := append([]string(nil), fake.probedGenerations...)
	fake.mu.Unlock()
	if len(probed) != 2 || probed[0] != first.Generation || probed[1] != second.Generation {
		t.Fatalf("probed generations = %v, want %q then %q — each attempt must probe the generation it is about to publish", probed, first.Generation, second.Generation)
	}
	if got := strings.Count(h.stderr.String(), "will not find this server"); got != 0 {
		t.Errorf("a successful retry printed %d discovery warnings, want 0", got)
	}
}

func TestRunDocsServeWithRuntime_CollisionBudgetExhausted(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	for i := 0; i < docsServePublishAttempts; i++ {
		fake.infoScript = append(fake.infoScript, fakeInfoResult{info: testGenerationInfo(byte(0x50 + i))})
		fake.probeScript = append(fake.probeScript, nil)
		fake.publishScript = append(fake.publishScript, fakePublication{err: docspkg.ErrGenerationCollision})
	}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)

	newInfo, probe, publish, _, _, _ := fake.counts()
	if publish != docsServePublishAttempts {
		t.Errorf("publish calls = %d, want exactly %d", publish, docsServePublishAttempts)
	}
	if newInfo != docsServePublishAttempts {
		t.Errorf("newInfo calls = %d, want exactly %d — there must be no ninth mint after the last collision", newInfo, docsServePublishAttempts)
	}
	if probe != docsServePublishAttempts {
		t.Errorf("probe calls = %d, want exactly %d", probe, docsServePublishAttempts)
	}
	if got := strings.Count(h.stderr.String(), docsServeDiscoveryUnavailable); got != 1 {
		t.Errorf("the discovery-unavailable warning appeared %d times, want exactly 1", got)
	}

	// Serving continues until an explicit stop.
	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil — exhausting the budget costs discovery, not serving", err)
	}
	_, _, _, _, shutdown, leaseClose := fake.counts()
	if shutdown != 1 || leaseClose != 0 {
		t.Errorf("shutdown = %d, closeLease = %d, want one Shutdown and no lease to close", shutdown, leaseClose)
	}
}

func TestRunDocsServeWithRuntime_CollisionRetryGenerationFailure(t *testing.T) {
	for _, k := range []int{1, 7} {
		t.Run(fmt.Sprintf("after %d collisions", k), func(t *testing.T) {
			fake := newFakeServeRuntime(3590)
			for i := 0; i < k; i++ {
				fake.infoScript = append(fake.infoScript, fakeInfoResult{info: testGenerationInfo(byte(0x60 + i))})
				fake.probeScript = append(fake.probeScript, nil)
				fake.publishScript = append(fake.publishScript, fakePublication{err: docspkg.ErrGenerationCollision})
			}
			// The k+1'th mint fails.
			fake.infoScript = append(fake.infoScript, fakeInfoResult{err: errors.New("entropy source failed")})
			fake.shutdownErr = errors.New("shutdown deadline exceeded")

			h := startServeHarness(t, fake)
			h.waitForBanner(t)

			newInfo, probe, publish, closeServer, _, leaseClose := fake.counts()
			if publish != k || probe != k {
				t.Errorf("publish = %d, probe = %d, want %d of each", publish, probe, k)
			}
			if newInfo != k+1 {
				t.Errorf("newInfo = %d, want %d", newInfo, k+1)
			}
			if closeServer != 0 || leaseClose != 0 {
				t.Errorf("closeServer = %d, closeLease = %d, want zero — a failed retry mint is not a shutdown", closeServer, leaseClose)
			}
			if got := strings.Count(h.stderr.String(), docsServeDiscoveryUnavailable); got != 1 {
				t.Errorf("the discovery-unavailable warning appeared %d times, want exactly 1", got)
			}

			// The eventual return is the LATER primary result, never the mint
			// error — that error is diagnostic only.
			h.cancel()
			err := h.wait(t)
			if err == nil || !strings.Contains(err.Error(), "shutdown deadline exceeded") {
				t.Fatalf("runDocsServeWithRuntime = %v, want the injected Shutdown error", err)
			}
			if strings.Contains(err.Error(), "entropy source failed") {
				t.Errorf("the returned error is the retry-mint failure: %v", err)
			}
		})
	}
}

func TestRunDocsServeWithRuntime_CancelDuringPublish_SuppressesRetryAndWarning(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0x70)}, {info: testGenerationInfo(0x80)}}
	fake.probeScript = []error{nil, nil}
	fake.publishScript = []fakePublication{
		{err: docspkg.ErrGenerationCollision},
		{publication: docspkg.Publication{Lease: leasePlaceholder()}},
	}

	// Cancel from INSIDE the first publish call. Cancellation is observable the
	// instant cancel() returns, with no goroutine handoff, so the checkpoint
	// that runs when publish returns is guaranteed to see it.
	//
	// The cancel function arrives over a channel rather than through a captured
	// variable, because the hook runs on the lifecycle's goroutine while the
	// test's goroutine is still assigning the harness.
	cancelReady := make(chan context.CancelFunc, 1)
	fake.publishHook = func(attempt int) {
		if attempt == 1 {
			(<-cancelReady)()
		}
	}
	h := startServeHarness(t, fake)
	cancelReady <- h.cancel

	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil — startup cancellation is a normal stop", err)
	}

	newInfo, _, publish, closeServer, shutdown, _ := fake.counts()
	if publish != 1 {
		t.Errorf("publish = %d, want 1 — a collision must not be retried once the server is already exiting", publish)
	}
	if newInfo != 1 {
		t.Errorf("newInfo = %d, want 1 — no replacement generation is minted while exiting", newInfo)
	}
	if closeServer != 1 || shutdown != 0 {
		t.Errorf("closeServer = %d, shutdown = %d, want one Close and no Shutdown before steady state", closeServer, shutdown)
	}
	if strings.Contains(h.stderr.String(), docsServeDiscoveryUnavailable) {
		t.Errorf("stderr carries the continuation warning for a server that is already exiting: %q", h.stderr.String())
	}
}

// ---------------------------------------------------------------------------
// publication results
// ---------------------------------------------------------------------------

func TestRunDocsServeWithRuntime_OrdinaryPublishError_KeepsServing(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0x90)}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{err: errors.New("read-only filesystem")}}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)

	newInfo, _, publish, _, _, _ := fake.counts()
	if publish != 1 || newInfo != 1 {
		t.Errorf("publish = %d, newInfo = %d, want 1 of each — only a collision is retryable", publish, newInfo)
	}
	if got := strings.Count(h.stderr.String(), docsServeDiscoveryUnavailable); got != 1 {
		t.Errorf("the discovery-unavailable warning appeared %d times, want exactly 1", got)
	}

	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil", err)
	}
}

func TestRunDocsServeWithRuntime_NilLeaseWithNilError_IsAnInternalFailure(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0xA0)}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{publication: docspkg.Publication{}}}

	h := startServeHarness(t, fake)
	err := h.wait(t)
	if !errors.Is(err, errDocsServeInternal) {
		t.Fatalf("runDocsServeWithRuntime = %v, want the fixed internal-contract error", err)
	}
	_, _, _, closeServer, _, leaseClose := fake.counts()
	if closeServer != 1 || leaseClose != 0 {
		t.Errorf("closeServer = %d, closeLease = %d, want one Close and no lease", closeServer, leaseClose)
	}
	if strings.Contains(h.stdout.String(), "Ctrl-C to stop") {
		t.Error("the server announced itself despite an internal publication-contract failure")
	}
}

func TestRunDocsServeWithRuntime_PublicationWarning_SurfacedOnceAndLeaseStillCloses(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0xB0)}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{
		publication: docspkg.Publication{
			Lease:   leasePlaceholder(),
			Warning: errors.New("sync the docs discovery directory: input/output error"),
		},
	}}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil — a durability warning is not a failure", err)
	}

	if got := strings.Count(h.stderr.String(), "input/output error"); got != 1 {
		t.Errorf("the publication warning appeared %d times, want exactly 1", got)
	}
	_, _, _, _, _, leaseClose := fake.counts()
	if leaseClose != 1 {
		t.Errorf("closeLease = %d, want exactly 1 — a warned publication still owns its record", leaseClose)
	}
}

// ---------------------------------------------------------------------------
// steady state
// ---------------------------------------------------------------------------

func TestRunDocsServeWithRuntime_SteadyStateCancel_ShutsDownOnceAndClosesTheLeaseOnce(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0xC0)}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{publication: docspkg.Publication{Lease: leasePlaceholder()}}}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()
	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil", err)
	}

	_, _, _, closeServer, shutdown, leaseClose := fake.counts()
	if shutdown != 1 || leaseClose != 1 {
		t.Errorf("shutdown = %d, closeLease = %d, want exactly 1 of each", shutdown, leaseClose)
	}
	if closeServer != 0 {
		t.Errorf("closeServer = %d, want 0 — steady-state cancellation drains through Shutdown", closeServer)
	}

	// The lease closes AFTER the primary outcome is known.
	events := fake.eventLog()
	shutdownAt, leaseAt := -1, -1
	for i, event := range events {
		if event == "shutdown" {
			shutdownAt = i
		}
		if event == "closeLease" {
			leaseAt = i
		}
	}
	if shutdownAt < 0 || leaseAt < 0 || leaseAt < shutdownAt {
		t.Errorf("events = %v, want closeLease after shutdown", events)
	}
}

func TestRunDocsServeWithRuntime_SteadyStateServeError_SkipsShutdown(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0xC1)}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{publication: docspkg.Publication{Lease: leasePlaceholder()}}}

	h := startServeHarness(t, fake)
	h.waitForBanner(t)

	serveFailure := errors.New("accept: too many open files")
	fake.serveGate <- serveFailure

	err := h.wait(t)
	if !errors.Is(err, serveFailure) {
		t.Fatalf("runDocsServeWithRuntime = %v, want the Serve error", err)
	}
	_, _, _, _, shutdown, leaseClose := fake.counts()
	if shutdown != 0 {
		t.Errorf("shutdown = %d, want 0 — a server that already stopped serving has nothing to drain", shutdown)
	}
	if leaseClose != 1 {
		t.Errorf("closeLease = %d, want exactly 1", leaseClose)
	}
}

func TestRunDocsServeWithRuntime_LeaseCloseFailure_WarnsWithoutReplacingTheResult(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.infoScript = []fakeInfoResult{{info: testGenerationInfo(0xD0)}}
	fake.probeScript = []error{nil}
	fake.publishScript = []fakePublication{{publication: docspkg.Publication{Lease: leasePlaceholder()}}}
	fake.leaseErr = errors.New("remove the docs discovery record: input/output error")

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()

	if err := h.wait(t); err != nil {
		t.Fatalf("runDocsServeWithRuntime = %v, want nil — a cleanup failure must not misreport a session that worked", err)
	}
	if !strings.Contains(h.stderr.String(), "input/output error") {
		t.Errorf("stderr = %q, want the lease-close failure warned", h.stderr.String())
	}
}

// ---------------------------------------------------------------------------
// mixed runtime: real listener, Serve, middleware, and probe
// ---------------------------------------------------------------------------

// TestRunDocsServeWithRuntime_RealHandlerAnswersTheRetryGeneration proves the
// atomic generation source actually changed after a collision.
//
// The fake-only collision test above proves the retry probe RECEIVED a new
// argument, which a bug that forgot to store into the atomic.Value would also
// satisfy. This one keeps the real listener, Serve, identity middleware, and
// ProbeServerGeneration, so the second probe only succeeds if the running
// handler is serving the second generation.
func TestRunDocsServeWithRuntime_RealHandlerAnswersTheRetryGeneration(t *testing.T) {
	idx := testDocsIndex(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	first, second := testGenerationInfo(0xE0), testGenerationInfo(0xF0)
	var mu sync.Mutex
	var minted int
	var publishedGenerations []string

	rt := productionDocsServeRuntime()
	rt.listen = func(string) (net.Listener, error) { return ln, nil }
	rt.serversDir = func() (string, error) { return t.TempDir(), nil }
	rt.newInfo = func(addr, token string) (docspkg.ServerInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		minted++
		info := first
		if minted > 1 {
			info = second
		}
		info.Addr, info.Token = addr, token
		return info, nil
	}
	rt.publish = func(_ string, info docspkg.ServerInfo) (docspkg.Publication, error) {
		mu.Lock()
		publishedGenerations = append(publishedGenerations, info.Generation)
		attempt := len(publishedGenerations)
		mu.Unlock()
		if attempt == 1 {
			return docspkg.Publication{}, docspkg.ErrGenerationCollision
		}
		return docspkg.Publication{Lease: leasePlaceholder()}, nil
	}
	rt.closeLease = func(*docspkg.ServerLease) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetContext(ctx)

	done := make(chan error, 1)
	go func() {
		done <- runDocsServeWithRuntime(cmd, module.Deps{Runner: &exec.FakeRunner{}}, idx, "127.0.0.1:0", false, "", rt)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(stdout.String(), "Ctrl-C to stop") {
		time.Sleep(2 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "Ctrl-C to stop") {
		cancel()
		t.Fatalf("the server never reached steady state; stderr=%q", stderr.String())
	}

	addr := parseServedAddr(stdout.String())
	// The REAL handler must now answer for the second generation and refuse
	// the first. Both probes go through the real no-proxy client.
	if err := docspkg.ProbeServerGeneration(context.Background(), addr, second.Generation); err != nil {
		cancel()
		t.Fatalf("the running handler does not answer for the retry generation: %v — the atomic generation source was not updated", err)
	}
	if err := docspkg.ProbeServerGeneration(context.Background(), addr, first.Generation); err == nil {
		cancel()
		t.Fatal("the running handler still answers for the collided generation")
	}

	mu.Lock()
	published := append([]string(nil), publishedGenerations...)
	mu.Unlock()
	if len(published) != 2 || published[0] != first.Generation || published[1] != second.Generation {
		t.Errorf("published generations = %v, want %q then %q", published, first.Generation, second.Generation)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown = %v, want nil", err)
		}
	case <-time.After(shutdownWaitBudget):
		t.Fatal("the server did not shut down")
	}
}

// TestRunDocsServe_DrainTimeoutForcesCloseAndExitsZero pins the Ctrl-C
// contract. net/http's Server.Shutdown will not close a StateNew connection —
// one a client transport dialed speculatively and never used — until it has
// sat there five seconds, so a drain can legitimately run out of time with
// nothing wrong. Reporting that as a command failure made an idle browser tab
// look like a crash; `docs serve` now forces the close and exits zero, saying
// so on stderr.
func TestRunDocsServe_DrainTimeoutForcesCloseAndExitsZero(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	// Discovery is out of scope here; making it ineligible reaches the banner
	// without scripting a mint/probe/publish sequence.
	fake.serversDirErr = errors.New("no config dir")
	fake.shutdownErr = context.DeadlineExceeded

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()

	if err := h.wait(t); err != nil {
		t.Errorf("runDocsServeWithRuntime = %v, want nil — a drain that ran out of time is not a failed command", err)
	}
	if _, _, _, closeServer, _, _ := fake.counts(); closeServer != 1 {
		t.Errorf("closeServer calls = %d, want 1 — the drain timeout must force the listener closed", closeServer)
	}
	if !strings.Contains(h.stderr.String(), "still open") {
		t.Errorf("stderr = %q, want it to report the forced close", h.stderr.String())
	}
}

// TestRunDocsServe_NonTimeoutShutdownErrorStillFails is the control: only a
// drain timeout is forgiven. A real Shutdown failure must still surface, or
// the arm above would swallow every shutdown defect.
func TestRunDocsServe_NonTimeoutShutdownErrorStillFails(t *testing.T) {
	fake := newFakeServeRuntime(3590)
	fake.serversDirErr = errors.New("no config dir")
	fake.shutdownErr = errors.New("listener exploded")

	h := startServeHarness(t, fake)
	h.waitForBanner(t)
	h.cancel()

	err := h.wait(t)
	if err == nil || !strings.Contains(err.Error(), "listener exploded") {
		t.Fatalf("runDocsServeWithRuntime = %v, want the injected Shutdown error", err)
	}
}
