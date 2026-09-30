package docs

import (
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/text"
)

// The render deadline and the render-CPU cap (forgectl#596). The markup
// guard (markupguard.go) bounds the superlinear cases it knows how to
// count, but not the ones nobody has measured yet, and not the work
// after goldmark: the balancer is superlinear on "</div>x" blocks.
// Measured on origin/main with the guard on, the worst known inputs took
// 0.58 s at 256 KiB, 1.76 s at 512 KiB and 5.4 s at 1 MiB.
//
// Go cannot stop a goroutine, and goldmark's parse has no cancellation
// point, so an abandoned render keeps running until it ends by itself.
// These three bounds keep that safe:
//   - maxRenderBytes keeps every known input well inside the deadline.
//   - renderDeadline bounds how long a request waits and holds renderMu.
//   - renderInFlight allows one render goroutine at a time. While an
//     abandoned render runs, every other page is shown as source text and
//     starts no render of its own, so render CPU stays at one core.

// maxRenderBytes is the largest document renderHidden formats. A larger one,
// up to the 1 MiB read cap (renderCapBytes), is shown as its source text
// under a notice. The worst input measured at this size renders in about a
// third of renderDeadline; the largest written document in the markup
// guard's sweeps, a 338 KB changelog, is under it.
const maxRenderBytes = 512 << 10

// renderDeadline is how long a request waits for its render before it
// abandons it and shows the source as text. It is a variable only so tests
// can shorten it.
var renderDeadline = 5 * time.Second

// renderInFlight is set while a render goroutine runs, including one its
// request has abandoned. It is set under renderMu and cleared by the
// goroutine as it ends.
var renderInFlight atomic.Bool

// renderStallHook, when set, runs on the render goroutine before the
// render. It is a test seam for a render that is slow; production never
// sets it.
var renderStallHook func()

// renderResult is what the render goroutine hands back to its request.
type renderResult struct {
	html   string
	hidden []text.Segment
	err    error
}

// renderBounded runs renderPipeline on its own goroutine and waits for it
// under renderMu, for at most renderDeadline. Past that, or while an
// abandoned render still runs, the page is the source as text under a
// notice, and renderMu is released on return either way.
func renderBounded(md goldmark.Markdown, source []byte, kind RootKind, resolve wikilinkResolver) (string, []text.Segment, error) {
	renderMu.Lock()
	defer renderMu.Unlock()
	if !renderInFlight.CompareAndSwap(false, true) {
		slog.Warn("docs: an abandoned render is still running; served the source as text.", "bytes", len(source))
		return renderBusyDoc(source), nil, nil
	}
	// Buffered, so an abandoned goroutine's send never blocks.
	done := make(chan renderResult, 1)
	go func() { done <- runRender(md, source, kind, resolve) }()
	timer := time.NewTimer(renderDeadline)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.html, r.hidden, r.err
	case <-timer.C:
		slog.Warn("docs: render passed its deadline; served the source as text.", "bytes", len(source), "deadline", renderDeadline)
		return tooSlowToRenderDoc(source), nil, nil
	}
}

// runRender is the render goroutine's body. It clears renderInFlight before
// its result is sent, so a request that has its result never sees its own
// render as still in flight. A panic becomes an error: a render goroutine
// is not under net/http's per-request recover, so one would otherwise take
// the whole server down.
func runRender(md goldmark.Markdown, source []byte, kind RootKind, resolve wikilinkResolver) (r renderResult) {
	defer func() {
		if p := recover(); p != nil {
			r = renderResult{err: fmt.Errorf("render markdown: panic: %v", p)}
		}
		renderInFlight.Store(false)
	}()
	if hook := renderStallHook; hook != nil {
		hook()
	}
	r.html, r.hidden, r.err = renderPipeline(md, source, kind, resolve)
	return r
}

// sourceTextDoc is a page body that shows source as text: a fixed notice
// (kind names it for data-forgectl-notice; title and bodyHTML are fixed
// markup, never document bytes), then the whole source HTML-escaped in a
// <pre>, so the reader still shows every byte of it.
func sourceTextDoc(kind, title, bodyHTML string, source []byte) string {
	return `<blockquote class="callout warning" role="note" data-forgectl-notice="` + kind + `">` +
		`<div class="callout-title"><svg viewBox="0 0 24 24" aria-hidden="true">` + calloutTriangleIcon + `</svg> ` + title + `</div>` +
		`<p>` + bodyHTML + `</p></blockquote>` +
		`<pre class="doc-plain-text">` + html.EscapeString(string(source)) + `</pre>`
}

// tooLargeToRenderDoc is the page body for a document over maxRenderBytes.
func tooLargeToRenderDoc(source []byte) string {
	return sourceTextDoc("render-size", "Shown as plain text",
		"This document is over "+strconv.Itoa(maxRenderBytes>>10)+" KiB, more than the reader formats, so it is shown as its source text.",
		source)
}

// tooSlowToRenderDoc is the page body for a render that passed
// renderDeadline.
func tooSlowToRenderDoc(source []byte) string {
	return sourceTextDoc("render-deadline", "Too slow to render",
		"This document took longer than "+renderDeadline.String()+" to format, so it is shown as its source text.",
		source)
}

// renderBusyDoc is the page body for a request refused while an abandoned
// render still runs.
func renderBusyDoc(source []byte) string {
	return sourceTextDoc("render-busy", "Shown as plain text",
		"The reader is still finishing a document that was too slow to render, so this one is shown as its source text. Reload in a few seconds to format it.",
		source)
}
