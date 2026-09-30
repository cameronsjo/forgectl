package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The waits in these tests are for a goroutine to reach a point or to
// finish, with a generous limit so a hang reads red. None bounds how long a
// render takes (#879); slowness comes from the render seams.

// TestMain lifts the render deadline for the whole package, so a test that
// renders a heavy document never meets the production 5 s deadline on a
// slow or -race run and reads the too-slow notice as a wrong page. The
// deadline tests set their own through trackRenders.
func TestMain(m *testing.M) {
	renderDeadline = time.Hour
	os.Exit(m.Run())
}

// renderTracker installs the render seams for one test: renders block in
// the goldmark stage while stall is set and until release is called, and
// every render goroutine is counted in and out. At cleanup it releases
// the renders, waits for every render goroutine to exit, and restores the
// seams and the deadline.
type renderTracker struct {
	starts, exits atomic.Int32
	gate          chan struct{}
	released      bool
}

func trackRenders(t *testing.T, deadline time.Duration, stall bool) *renderTracker {
	t.Helper()
	rt := &renderTracker{gate: make(chan struct{})}
	oldDeadline := renderDeadline
	oldAcquire, oldStall, oldEnd, oldPost, oldExit := renderAcquireHook, renderStallHook, renderGoldmarkEndHook, renderPostHook, renderExitHook
	renderDeadline = deadline
	renderStallHook = func() {
		rt.starts.Add(1)
		if stall {
			<-rt.gate
		}
	}
	renderExitHook = func() { rt.exits.Add(1) }
	t.Cleanup(func() {
		rt.release()
		waitFor(t, "every render goroutine to exit", func() bool { return rt.exits.Load() == rt.starts.Load() })
		renderDeadline = oldDeadline
		renderAcquireHook, renderStallHook, renderGoldmarkEndHook, renderPostHook, renderExitHook = oldAcquire, oldStall, oldEnd, oldPost, oldExit
	})
	return rt
}

func (rt *renderTracker) release() {
	if !rt.released {
		rt.released = true
		close(rt.gate)
	}
}

// waitFor polls cond until it holds, failing after a generous limit.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; !cond(); i++ {
		if i > 2000 {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// slotFree reports whether renderMu can be taken now, without keeping it.
func slotFree() bool {
	select {
	case renderMu <- struct{}{}:
		<-renderMu
		return true
	default:
		return false
	}
}

// renderAsync runs RenderDocForContext on another goroutine and fails the
// test if it does not return within a generous wait, so a missing bound
// reads red rather than hanging the suite.
func renderAsync(t *testing.T, ctx context.Context, source string) (RenderedDoc, error) {
	t.Helper()
	type result struct {
		r   RenderedDoc
		err error
	}
	got := make(chan result, 1)
	go func() {
		r, err := RenderDocForContext(ctx, RootDocs, []byte(source), nil, nil)
		got <- result{r, err}
	}()
	select {
	case res := <-got:
		return res.r, res.err
	case <-time.After(10 * time.Second):
		t.Fatal("RenderDocForContext did not return: nothing bounded the wait")
		return RenderedDoc{}, nil
	}
}

func mustRender(t *testing.T, source string) RenderedDoc {
	t.Helper()
	r, err := renderAsync(t, context.Background(), source)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return r
}

func assertFormatted(t *testing.T, r RenderedDoc, want string) {
	t.Helper()
	if r.Notice != "" || strings.Contains(r.HTML, "data-forgectl-notice") || !strings.Contains(r.HTML, want) {
		t.Fatalf("page was not formatted (notice %q, want %q):\n%s", r.Notice, want, r.HTML)
	}
}

func assertSourceNotice(t *testing.T, r RenderedDoc, notice, escapedSource string) {
	t.Helper()
	if r.Notice != notice || !strings.Contains(r.HTML, `data-forgectl-notice="`+notice+`"`) {
		t.Fatalf("notice = %q, want %q:\n%s", r.Notice, notice, r.HTML)
	}
	if !strings.Contains(r.HTML, `<pre class="doc-plain-text">`+escapedSource+`</pre>`) {
		t.Fatalf("page does not show the escaped source %q:\n%s", escapedSource, r.HTML)
	}
}

// A render that passes the deadline is abandoned: the page is the escaped
// source under the too-slow notice, and renderMu is free again although the
// render is still blocked. Mutations: waiting on the render without the
// timer hangs past renderAsync's limit; keeping renderMu until the render
// ends fails the slotFree check.
func TestRenderDeadline_SlowRenderShowsNoticeAndReleasesLock(t *testing.T) {
	rt := trackRenders(t, 20*time.Millisecond, true)
	r := mustRender(t, "# Title\n\n<b>bold</b> text\n")
	assertSourceNotice(t, r, noticeRenderDeadline, "# Title\n\n&lt;b&gt;bold&lt;/b&gt; text\n")
	if !strings.Contains(r.HTML, "Too slow to render") {
		t.Fatalf("too-slow notice has no title:\n%s", r.HTML)
	}
	if !renderInFlight.Load() {
		t.Fatal("the abandoned render should still be running")
	}
	if !slotFree() {
		t.Fatal("renderMu is still held after the deadline passed")
	}
	rt.release()
}

// While an abandoned render still runs, another page starts no render of
// its own: it is shown as source text, so at most one goldmark render ever
// runs. Once the abandoned render ends, pages render again. Mutation:
// dropping the renderInFlight check starts a second render.
func TestRenderDeadline_OneAbandonedRenderAtATime(t *testing.T) {
	rt := trackRenders(t, 20*time.Millisecond, true)
	_ = mustRender(t, "first\n")
	second := mustRender(t, "second <i>page</i>\n")
	assertSourceNotice(t, second, noticeRenderBusy, "second &lt;i&gt;page&lt;/i&gt;\n")
	if n := rt.starts.Load(); n != 1 {
		t.Fatalf("render goroutines started = %d, want 1", n)
	}
	rt.release()
	waitFor(t, "the abandoned render to end", func() bool { return rt.exits.Load() == 1 })
	renderDeadline = time.Hour
	assertFormatted(t, mustRender(t, "third **page**\n"), "<strong>page</strong>")
}

// A request that cannot get renderMu within the deadline is shown as source
// text and starts no render. Mutation: acquiring renderMu without the
// timer case waits for the slot forever.
func TestRenderDeadline_BlockedSlotServesBusyWithinDeadline(t *testing.T) {
	rt := trackRenders(t, 20*time.Millisecond, false)
	renderMu <- struct{}{} // another render holds the slot
	defer func() { <-renderMu }()
	r := mustRender(t, "blocked *page*\n")
	assertSourceNotice(t, r, noticeRenderBusy, "blocked *page*\n")
	if n := rt.starts.Load(); n != 0 {
		t.Fatalf("a request that never got renderMu started %d render(s)", n)
	}
}

// A request whose context is done starts no render: not when it arrives
// done, and not when it ends while waiting for renderMu. Mutations:
// dropping the ctx case from the acquire makes the waiting request hang;
// dropping both ctx checks around the acquire lets a done request render
// whenever select picks the free slot, which some of the rounds hit.
func TestRenderDeadline_CancelledContextNeverRenders(t *testing.T) {
	rt := trackRenders(t, time.Hour, false)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	for range 32 {
		if _, err := renderAsync(t, done, "x\n"); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	}
	if n := rt.starts.Load(); n != 0 {
		t.Fatalf("a request with a done context started %d render(s)", n)
	}

	renderMu <- struct{}{} // another render holds the slot
	defer func() { <-renderMu }()
	waiting, stop := context.WithCancel(context.Background())
	atAcquire := make(chan struct{})
	renderAcquireHook = func() { close(atAcquire) }
	defer func() { renderAcquireHook = nil }()
	errs := make(chan error, 1)
	go func() {
		_, err := RenderDocForContext(waiting, RootDocs, []byte("y\n"), nil, nil)
		errs <- err
	}()
	<-atAcquire // past the ctx check, about to wait for renderMu
	stop()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a request cancelled while waiting for renderMu did not return")
	}
	if n := rt.starts.Load(); n != 0 {
		t.Fatalf("a request cancelled while waiting started %d render(s)", n)
	}
}

// The goldmark stage releases renderMu and clears renderInFlight before the
// post-processing runs, so while one page is being sanitized and balanced
// the next page renders in full. Mutations: clearing renderInFlight only
// after the result is sent shows the second page as render-busy; releasing
// renderMu only when the whole render ends makes the second page wait out
// the deadline.
func TestRenderDeadline_PostProcessingRunsOutsideTheSlot(t *testing.T) {
	trackRenders(t, time.Hour, false)
	inPost := make(chan struct{})
	postGate := make(chan struct{})
	// Registered after trackRenders, so it runs first: a failing assertion
	// below still frees the first render, and fails one test rather than
	// hanging the suite.
	var gateClosed atomic.Bool
	openGate := func() {
		if gateClosed.CompareAndSwap(false, true) {
			close(postGate)
		}
	}
	t.Cleanup(openGate)
	var first atomic.Bool
	renderPostHook = func() {
		if first.CompareAndSwap(false, true) {
			close(inPost)
			<-postGate
		}
	}
	// A short deadline for the second page only: if it has to wait for the
	// first page's slot, it gives up well before renderAsync's limit.
	type result struct {
		r   RenderedDoc
		err error
	}
	firstDone := make(chan result, 1)
	go func() {
		r, err := RenderDocForContext(context.Background(), RootDocs, []byte("first *page*\n"), nil, nil)
		firstDone <- result{r, err}
	}()
	select {
	case <-inPost:
	case <-time.After(10 * time.Second):
		t.Fatal("the first render never reached post-processing")
	}
	renderDeadline = 200 * time.Millisecond
	assertFormatted(t, mustRender(t, "second *page*\n"), "<em>page</em>")
	openGate()
	var res result
	select {
	case res = <-firstDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the first render never finished")
	}
	if res.err != nil {
		t.Fatal(res.err)
	}
	// The first page's deadline was set before the change above.
	assertFormatted(t, res.r, "<em>page</em>")
}

// An abandoned render goroutine still exits once its render ends: its
// result send cannot block. Mutation: an unbuffered result channel leaves
// the goroutine blocked on its send forever, and renderExitHook never runs.
func TestRenderDeadline_AbandonedRenderGoroutineExits(t *testing.T) {
	rt := trackRenders(t, 20*time.Millisecond, true)
	r := mustRender(t, "slow\n")
	if r.Notice != noticeRenderDeadline {
		t.Fatalf("notice = %q, want %q", r.Notice, noticeRenderDeadline)
	}
	rt.release()
	waitFor(t, "the abandoned render goroutine to exit", func() bool { return rt.exits.Load() == 1 })
}

// A render that finishes inside the deadline is served as before.
// Mutation: a zero-length wait (the timer always winning) serves the notice.
func TestRenderDeadline_FastRenderIsFormatted(t *testing.T) {
	trackRenders(t, time.Hour, false)
	assertFormatted(t, mustRender(t, "fast *page*\n"), "<em>page</em>")
}

// A panic on the render goroutine is an error, not a dead server, and it
// frees renderMu and renderInFlight. Mutation: dropping the recover
// crashes the test binary.
func TestRenderDeadline_PanicIsAnError(t *testing.T) {
	rt := trackRenders(t, time.Hour, false)
	renderStallHook = func() {
		rt.starts.Add(1)
		panic("boom")
	}
	_, err := RenderDoc([]byte("x\n"))
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("err = %v, want the panic as an error", err)
	}
	if renderInFlight.Load() || !slotFree() {
		t.Fatal("a panicked render left renderInFlight set or renderMu held")
	}
}

// A document over maxRenderBytes is shown as source text under the size
// notice without being parsed; one at exactly the cap is formatted. The
// deadline is lifted here, so the at-cap render is never timed.
// Mutations: `>` to `>=` fails the at-cap case; dropping the check (or
// raising the cap) fails the over-cap case.
func TestRenderCap(t *testing.T) {
	rt := trackRenders(t, time.Hour, false)
	const head = "# Cap\n\n*em*\n\n"
	para := strings.Repeat("plain prose words ", 60) + "\n\n"
	body := func(n int) string {
		return (head + strings.Repeat(para, n/len(para)+1))[:n]
	}
	atCap := body(maxRenderBytes)
	assertFormatted(t, mustRender(t, atCap), "<em>em</em>")
	if n := rt.starts.Load(); n != 1 {
		t.Fatalf("at-cap render goroutines = %d, want 1", n)
	}

	r := mustRender(t, body(maxRenderBytes+1))
	if r.Notice != noticeRenderSize || !strings.Contains(r.HTML, "over 512 KiB") {
		t.Fatalf("over-cap document did not get the size notice: %.300s", r.HTML)
	}
	if strings.Contains(r.HTML, "<em>") || !strings.Contains(r.HTML, `<pre class="doc-plain-text"># Cap`) {
		t.Fatalf("over-cap document was formatted instead of shown as source: %.300s", r.HTML)
	}
	if n := rt.starts.Load(); n != 1 {
		t.Fatal("an over-cap document reached the render goroutine")
	}
}

// The doc handler renders under the request's context: a request whose
// client has already gone starts no render and writes no page. Mutation:
// rendering under context.Background() in handleDoc starts the render.
func TestServer_DocRenderUsesRequestContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("# Note\n\ntext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	rt := trackRenders(t, time.Hour, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/doc/"+idx.Roots()[0].Label+"/note.md", nil))
	if n := rt.starts.Load(); n != 0 {
		t.Fatalf("a request whose context was done started %d render(s)", n)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("a request whose context was done got a page: %.200s", rec.Body.String())
	}
}

// Post-processing stages are bounded, abandoned ones included: with every
// post-processing slot taken, a render waits for one only until its
// deadline, then its goroutine gives up without running the stage and
// exits. Mutations: dropping the renderPostSlots acquire runs the stage
// (renderPostHook fires); dropping its abandoned case leaves the goroutine
// waiting for a slot after its request has gone.
func TestRenderDeadline_PostProcessingSlotsAreBounded(t *testing.T) {
	rt := trackRenders(t, 20*time.Millisecond, false)
	var posts atomic.Int32
	renderPostHook = func() { posts.Add(1) }
	for range cap(renderPostSlots) {
		renderPostSlots <- struct{}{}
	}
	defer func() {
		for range cap(renderPostSlots) {
			<-renderPostSlots
		}
	}()
	r := mustRender(t, "post *slots*\n")
	assertSourceNotice(t, r, noticeRenderDeadline, "post *slots*\n")
	waitFor(t, "the abandoned render goroutine to exit while every post-processing slot is taken", func() bool { return rt.exits.Load() == 1 })
	if n := posts.Load(); n != 0 {
		t.Fatalf("post-processing ran %d time(s) with no slot free", n)
	}
	if !slotFree() || renderInFlight.Load() {
		t.Fatal("a render waiting for a post-processing slot held renderMu or renderInFlight")
	}
}

// The markup guard runs inside the deadline: its block pass is serialized
// by markupGuardMu, which an index scan can hold, and a render waiting for
// it still answers at the deadline. The fixture is dense enough to need
// the guard's block pass. Mutation: running the guard in
// renderHiddenContext, before renderBounded, waits for markupGuardMu with
// nothing bounding it.
func TestRenderDeadline_MarkupGuardWaitIsInsideTheDeadline(t *testing.T) {
	rt := trackRenders(t, 20*time.Millisecond, false)
	source := strings.Repeat("*a", 6000)
	if countDelimiters([]byte(source))*len(source) <= maxInlineWork/2 {
		t.Fatal("fixture does not reach the guard's block pass")
	}
	markupGuardMu.Lock() // an index scan's guard parse is running
	locked := true
	defer func() {
		if locked {
			markupGuardMu.Unlock()
		}
	}()
	r := mustRender(t, source)
	if r.Notice != noticeRenderDeadline {
		t.Fatalf("notice = %q, want %q", r.Notice, noticeRenderDeadline)
	}
	markupGuardMu.Unlock()
	locked = false
	waitFor(t, "the abandoned render goroutine to exit", func() bool { return rt.exits.Load() == 1 })
}

// The in-flight flag is clear before parsed closes: a request that sees its
// goldmark stage end, and the next request it lets through, must never
// find that stage still in flight. renderGoldmarkEndHook runs between the
// two steps. Mutation: closing parsed before clearing the flag leaves the
// flag set when the hook runs.
func TestRenderDeadline_FlagClearsBeforeParsedCloses(t *testing.T) {
	trackRenders(t, time.Hour, false)
	var sawInFlight, ran atomic.Bool
	renderGoldmarkEndHook = func() {
		ran.Store(true)
		sawInFlight.Store(renderInFlight.Load())
	}
	assertFormatted(t, mustRender(t, "order *check*\n"), "<em>check</em>")
	if !ran.Load() {
		t.Fatal("renderGoldmarkEndHook never ran")
	}
	if sawInFlight.Load() {
		t.Fatal("renderInFlight was still set when the goldmark stage signalled its end")
	}
}

// A request whose context ends while its goldmark stage runs stops
// waiting at once, with ctx's error. Mutation: dropping the ctx case from
// the goldmark wait holds the request until the (hour-long) deadline.
func TestRenderDeadline_CancelDuringGoldmarkStage(t *testing.T) {
	rt := trackRenders(t, time.Hour, true)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := RenderDocForContext(ctx, RootDocs, []byte("g\n"), nil, nil)
		errs <- err
	}()
	waitFor(t, "the render to reach its goldmark stage", func() bool { return rt.starts.Load() == 1 })
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a request cancelled during its goldmark stage did not return")
	}
}

// A request whose context ends while its post-processing runs stops
// waiting at once, with ctx's error. Mutation: dropping the ctx case from
// the post-processing wait holds the request until the deadline.
func TestRenderDeadline_CancelDuringPostProcessing(t *testing.T) {
	trackRenders(t, time.Hour, false)
	inPost := make(chan struct{})
	postGate := make(chan struct{})
	var gateClosed atomic.Bool
	openGate := func() {
		if gateClosed.CompareAndSwap(false, true) {
			close(postGate)
		}
	}
	t.Cleanup(openGate)
	renderPostHook = func() {
		close(inPost)
		<-postGate
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := RenderDocForContext(ctx, RootDocs, []byte("p\n"), nil, nil)
		errs <- err
	}()
	select {
	case <-inPost:
	case <-time.After(10 * time.Second):
		t.Fatal("the render never reached post-processing")
	}
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a request cancelled during post-processing did not return")
	}
}

// A render whose request gave up during its goldmark stage skips
// post-processing even when a slot is free. The acquire alone would pick
// the free slot or the abandon at random; the check before it makes the
// skip certain, and the rounds catch its absence. Mutation: dropping the
// abandoned pre-check runs post-processing in about half the rounds.
func TestRenderDeadline_AbandonedRenderSkipsPostProcessing(t *testing.T) {
	for round := range 20 {
		rt := trackRenders(t, 20*time.Millisecond, true)
		var posts atomic.Int32
		renderPostHook = func() { posts.Add(1) }
		if r := mustRender(t, "abandon\n"); r.Notice != noticeRenderDeadline {
			t.Fatalf("round %d: notice = %q, want %q", round, r.Notice, noticeRenderDeadline)
		}
		rt.release()
		waitFor(t, "the abandoned render goroutine to exit", func() bool { return rt.exits.Load() == rt.starts.Load() })
		if n := posts.Load(); n != 0 {
			t.Fatalf("round %d: an abandoned render ran post-processing", round)
		}
	}
}

// A render goroutine uses the seams its request captured, never the
// globals: it can outlive its request and its test, and a global read then
// races the next test's writes. Swapping the globals while a render is
// stalled makes a global read call the wrong hook every time, where the
// race itself shows only now and then. Mutations: reading renderExitHook
// or renderPostHook on the goroutine calls the swapped-in hook.
func TestRenderDeadline_GoroutineUsesCapturedHooks(t *testing.T) {
	rt := trackRenders(t, time.Hour, true)
	var captured, swapped atomic.Int32
	renderPostHook = func() { captured.Add(1) }
	errs := make(chan error, 1)
	go func() {
		_, err := RenderDocForContext(context.Background(), RootDocs, []byte("hooks\n"), nil, nil)
		errs <- err
	}()
	waitFor(t, "the render to reach its goldmark stage", func() bool { return rt.starts.Load() == 1 })
	// What the next test's trackRenders would do, while this render runs.
	renderPostHook = func() { swapped.Add(1) }
	renderExitHook = func() { swapped.Add(1) }
	rt.release()
	select {
	case err := <-errs:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("render did not finish")
	}
	waitFor(t, "the render goroutine to exit", func() bool { return rt.exits.Load()+swapped.Load() >= 1 })
	if n := swapped.Load(); n != 0 {
		t.Fatalf("the render goroutine called %d hook(s) set after it started", n)
	}
	if captured.Load() != 1 || rt.exits.Load() != 1 {
		t.Fatalf("captured post hook ran %d time(s), exit hook %d, want 1 each", captured.Load(), rt.exits.Load())
	}
}
