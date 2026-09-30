package docs

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stallRenders makes every render block in renderStallHook until the
// returned release is called, with a short deadline. It waits, at cleanup,
// for any render still in flight to end, so no abandoned render outlives
// the test. The waits here are for a goroutine to finish, never a bound on
// how long a render takes (#879).
func stallRenders(t *testing.T) (release func(), calls *atomic.Int32) {
	t.Helper()
	gate := make(chan struct{})
	calls = new(atomic.Int32)
	oldDeadline, oldHook := renderDeadline, renderStallHook
	renderDeadline = 20 * time.Millisecond
	renderStallHook = func() {
		calls.Add(1)
		<-gate
	}
	released := false
	release = func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(func() {
		release()
		waitRenderIdle(t)
		renderDeadline, renderStallHook = oldDeadline, oldHook
	})
	return release, calls
}

// waitRenderIdle waits until no render goroutine is running.
func waitRenderIdle(t *testing.T) {
	t.Helper()
	for i := 0; renderInFlight.Load(); i++ {
		if i > 2000 {
			t.Fatal("render goroutine never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// renderAsync runs RenderDoc on another goroutine and fails the test if it
// does not return within a generous wait, so a missing deadline reads red
// rather than hanging the suite.
func renderAsync(t *testing.T, source string) RenderedDoc {
	t.Helper()
	got := make(chan RenderedDoc, 1)
	errs := make(chan error, 1)
	go func() {
		r, err := RenderDoc([]byte(source))
		errs <- err
		got <- r
	}()
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("RenderDoc: %v", err)
		}
		return <-got
	case <-time.After(10 * time.Second):
		t.Fatal("RenderDoc did not return: the render deadline did not abandon a stalled render")
		return RenderedDoc{}
	}
}

// A render that passes the deadline is abandoned: the page is the escaped
// source under the too-slow notice, and renderMu is free again although the
// render itself is still blocked. Mutations: waiting on the result without
// the timer hangs past renderAsync's wait; returning on the timer without
// releasing renderMu fails the TryLock.
func TestRenderDeadline_SlowRenderShowsNoticeAndReleasesLock(t *testing.T) {
	release, _ := stallRenders(t)
	r := renderAsync(t, "# Title\n\n<b>bold</b> text\n")
	if !strings.Contains(r.HTML, `data-forgectl-notice="render-deadline"`) || !strings.Contains(r.HTML, "Too slow to render") {
		t.Fatalf("stalled render did not serve the too-slow notice:\n%s", r.HTML)
	}
	if !strings.Contains(r.HTML, `<pre class="doc-plain-text"># Title`+"\n\n&lt;b&gt;bold&lt;/b&gt; text\n</pre>") {
		t.Fatalf("too-slow page does not show the escaped source:\n%s", r.HTML)
	}
	if !renderInFlight.Load() {
		t.Fatal("the abandoned render should still be running")
	}
	if !renderMu.TryLock() {
		t.Fatal("renderMu is still held after the deadline passed")
	}
	renderMu.Unlock()
	release()
	waitRenderIdle(t)
}

// While an abandoned render still runs, another page starts no render of
// its own: it is shown as source text at once, so at most one render
// goroutine ever runs. Once the abandoned render ends, pages render again.
// Mutation: dropping the renderInFlight check starts a second stalled
// render (calls 2) and serves the too-slow notice instead of render-busy.
func TestRenderDeadline_OneAbandonedRenderAtATime(t *testing.T) {
	release, calls := stallRenders(t)
	_ = renderAsync(t, "first\n")
	second := renderAsync(t, "second <i>page</i>\n")
	if !strings.Contains(second.HTML, `data-forgectl-notice="render-busy"`) {
		t.Fatalf("a render during an abandoned one was not refused:\n%s", second.HTML)
	}
	if !strings.Contains(second.HTML, "second &lt;i&gt;page&lt;/i&gt;") {
		t.Fatalf("refused page does not show the escaped source:\n%s", second.HTML)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("render goroutines started = %d, want 1", n)
	}
	release()
	waitRenderIdle(t)
	renderStallHook = nil
	third := renderAsync(t, "third **page**\n")
	if strings.Contains(third.HTML, "data-forgectl-notice") || !strings.Contains(third.HTML, "<strong>page</strong>") {
		t.Fatalf("render after the abandoned one ended was not formatted:\n%s", third.HTML)
	}
}

// A render that finishes inside the deadline is served as before.
// Mutation: a zero-length wait (the timer always winning) serves the notice.
func TestRenderDeadline_FastRenderIsFormatted(t *testing.T) {
	r := renderAsync(t, "fast *page*\n")
	if strings.Contains(r.HTML, "data-forgectl-notice") || !strings.Contains(r.HTML, "<em>page</em>") {
		t.Fatalf("fast render was not formatted:\n%s", r.HTML)
	}
}

// A panic on the render goroutine is an error, not a dead server, and it
// frees the render slot. Mutation: dropping the recover crashes the test
// binary.
func TestRenderDeadline_PanicIsAnError(t *testing.T) {
	old := renderStallHook
	renderStallHook = func() { panic("boom") }
	t.Cleanup(func() { renderStallHook = old })
	_, err := RenderDoc([]byte("x\n"))
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("err = %v, want the panic as an error", err)
	}
	if renderInFlight.Load() {
		t.Fatal("a panicked render left renderInFlight set")
	}
}

// A document over maxRenderBytes is shown as source text under the size
// notice without being parsed; one at exactly the cap is formatted. The
// deadline is lifted here, so the at-cap render is never timed.
// Mutations: `>` to `>=` fails the at-cap case; dropping the check (or
// raising the cap) fails the over-cap case.
func TestRenderCap(t *testing.T) {
	var calls atomic.Int32
	oldDeadline, oldHook := renderDeadline, renderStallHook
	renderDeadline = time.Hour
	renderStallHook = func() { calls.Add(1) }
	t.Cleanup(func() { renderDeadline, renderStallHook = oldDeadline, oldHook })

	const head = "# Cap\n\n*em*\n\n"
	para := strings.Repeat("plain prose words ", 60) + "\n\n"
	body := func(n int) string {
		return (head + strings.Repeat(para, n/len(para)+1))[:n]
	}
	atCap := body(maxRenderBytes)
	r, err := RenderDoc([]byte(atCap))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "data-forgectl-notice") || !strings.Contains(r.HTML, "<em>em</em>") {
		t.Fatalf("a document at the cap (%d bytes) was not formatted: %.300s", len(atCap), r.HTML)
	}
	if calls.Load() != 1 {
		t.Fatalf("at-cap render goroutines = %d, want 1", calls.Load())
	}

	calls.Store(0)
	r, err = RenderDoc([]byte(body(maxRenderBytes + 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.HTML, `data-forgectl-notice="render-size"`) || !strings.Contains(r.HTML, "over 512 KiB") {
		t.Fatalf("over-cap document did not get the size notice: %.300s", r.HTML)
	}
	if strings.Contains(r.HTML, "<em>") || !strings.Contains(r.HTML, `<pre class="doc-plain-text"># Cap`) {
		t.Fatalf("over-cap document was formatted instead of shown as source: %.300s", r.HTML)
	}
	if calls.Load() != 0 {
		t.Fatal("an over-cap document reached the render goroutine")
	}
}
