package docs

import (
	"context"
	"errors"
	"fmt"
	"html"
	"runtime"
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
// 0.58 s at 256 KiB, 1.76 s at 512 KiB and 5.4 s at 1 MiB on one machine,
// and 4.1 s at 512 KiB on another.
//
// Go cannot stop a goroutine, and goldmark's parse has no cancellation
// point, so an abandoned render keeps running until it ends by itself.
// These bounds keep that safe:
//   - maxRenderBytes bounds the work any one render can do.
//   - renderDeadline bounds a request end to end: waiting for renderMu,
//     the markup guard, goldmark, and the post-processing after it. Past it the page is the
//     source as text, and the render is abandoned.
//   - renderInFlight allows one goldmark render at a time, abandoned ones
//     included. While an abandoned one runs, every other page is shown as
//     source text and starts no goldmark render of its own.
//   - renderPostSlots bounds the post-processing stages running at once,
//     abandoned ones included, since the balancer's superlinear cost is
//     there. A render abandoned before its turn skips the stage.
//   - The request's context ends the wait too: a request whose client has
//     gone starts no render and waits for none.

// maxRenderBytes is the largest document renderHidden formats. A larger one,
// up to the 1 MiB read cap (renderCapBytes), is shown as its source text
// under a notice. It keeps the worst known inputs to a few seconds of
// work; renderDeadline bounds the rest. The largest written document in
// the markup guard's sweeps, a 338 KB changelog, is under it.
const maxRenderBytes = 512 << 10

// renderDeadline is how long a request waits for its render, from asking
// for renderMu to the finished page, before it abandons the render and
// shows the source as text. It is a variable only so tests can change it.
var renderDeadline = 5 * time.Second

// renderPostSlots bounds the post-processing stages (renderPost) that run
// at once, abandoned ones included: half the CPUs, and at least two, so
// one slow page's balancer never holds up every other page's.
var renderPostSlots = make(chan struct{}, max(2, runtime.GOMAXPROCS(0)/2))

// errRenderAbandoned is what a render goroutine returns when its request
// gave up before its post-processing began. No request reads it.
var errRenderAbandoned = errors.New("docs: render abandoned before post-processing")

// renderInFlight is set while a goldmark stage runs, including one whose
// request has abandoned it. It is set while renderMu is held and cleared
// by the render goroutine as its goldmark stage ends.
var renderInFlight atomic.Bool

// Test seams; production never sets them. renderAcquireHook runs on the
// request's goroutine just before it waits for renderMu. The rest run on
// the render goroutine: renderStallHook before the goldmark stage,
// renderPostHook after it and before the post-processing, and
// renderExitHook as the goroutine exits, after it has handed back its
// result.
var (
	renderAcquireHook func()
	renderStallHook   func()
	renderPostHook    func()
	renderExitHook    func()
)

// The data-forgectl-notice value of each page shown as source text.
const (
	noticePlainText      = "plain-text"
	noticeRenderSize     = "render-size"
	noticeRenderDeadline = "render-deadline"
	noticeRenderBusy     = "render-busy"
)

// renderOutcome is a render's result: the page body, the source ranges it
// hides, and the notice it shows in place of the formatted document ("" if
// none).
type renderOutcome struct {
	html   string
	hidden []text.Segment
	notice string
	err    error
}

// renderBounded renders source within renderDeadline and while ctx lasts.
// It takes renderMu, runs the goldmark stage (the markup guard, then
// goldmark's parse and render) on a render goroutine and
// releases renderMu when that stage ends, then waits for the
// post-processing, which runs outside renderMu on the same goroutine,
// under renderPostSlots.
// Past the deadline, or while an abandoned goldmark stage still runs, the
// page is the source as text; once ctx is done the answer is ctx's error.
// renderMu is never held past either.
func renderBounded(ctx context.Context, md goldmark.Markdown, source []byte, kind RootKind, resolve wikilinkResolver) renderOutcome {
	if err := ctx.Err(); err != nil {
		return renderOutcome{err: err}
	}
	timer := time.NewTimer(renderDeadline)
	defer timer.Stop()
	if hook := renderAcquireHook; hook != nil {
		hook()
	}
	select {
	case renderMu <- struct{}{}:
	case <-timer.C:
		return sourceOutcome(noticeRenderBusy, source)
	case <-ctx.Done():
		return renderOutcome{err: ctx.Err()}
	}
	held := true
	release := func() {
		if held {
			held = false
			<-renderMu
		}
	}
	defer release()
	// select picks at random among ready cases, so a done ctx can lose to a
	// free slot; it still starts no render.
	if err := ctx.Err(); err != nil {
		return renderOutcome{err: err}
	}
	if !renderInFlight.CompareAndSwap(false, true) {
		return sourceOutcome(noticeRenderBusy, source)
	}
	parsed := make(chan struct{})
	// Closed when this request stops waiting, so a render goroutine still
	// waiting for a post-processing slot gives up instead.
	abandoned := make(chan struct{})
	defer close(abandoned)
	// Buffered, so an abandoned goroutine's send never blocks and the
	// goroutine always exits.
	done := make(chan renderOutcome, 1)
	go func() {
		done <- runRender(md, source, kind, resolve, parsed, abandoned)
		if hook := renderExitHook; hook != nil {
			hook()
		}
	}()
	select {
	case <-parsed:
		release()
	case <-timer.C:
		return sourceOutcome(noticeRenderDeadline, source)
	case <-ctx.Done():
		return renderOutcome{err: ctx.Err()}
	}
	select {
	case r := <-done:
		return r
	case <-timer.C:
		return sourceOutcome(noticeRenderDeadline, source)
	case <-ctx.Done():
		return renderOutcome{err: ctx.Err()}
	}
}

// runRender is the render goroutine's body. It clears renderInFlight before
// it closes parsed, so a request that sees its goldmark stage end, and the
// next request after it, never see that stage as still in flight. A panic
// becomes an error: a render goroutine is not under net/http's per-request
// recover, so one would otherwise take the whole server down.
func runRender(md goldmark.Markdown, source []byte, kind RootKind, resolve wikilinkResolver, parsed chan<- struct{}, abandoned <-chan struct{}) (out renderOutcome) {
	goldmarkDone := false
	endGoldmark := func() {
		if !goldmarkDone {
			goldmarkDone = true
			renderInFlight.Store(false)
			close(parsed)
		}
	}
	defer func() {
		if p := recover(); p != nil {
			out = renderOutcome{err: fmt.Errorf("render markdown: panic: %v", p)}
		}
		endGoldmark()
	}()
	if hook := renderStallHook; hook != nil {
		hook()
	}
	// A document goldmark would take superlinear time on is shown as plain
	// text instead. The guard measures the block structure md itself gives
	// source (markupguard.go). It runs here, inside the deadline, because
	// its parse is serialized too (markupGuardMu), and a wait for it
	// outside the deadline would be a wait nothing bounds.
	tooComplex, err := markupTooComplex(md, source)
	if err != nil {
		return renderOutcome{err: err}
	}
	if tooComplex {
		return renderOutcome{html: plainTextDoc(source), notice: noticePlainText}
	}
	g, err := renderGoldmark(md, source, kind, resolve)
	endGoldmark()
	if err != nil {
		return renderOutcome{err: err}
	}
	select {
	case <-abandoned:
		return renderOutcome{err: errRenderAbandoned}
	default:
	}
	select {
	case renderPostSlots <- struct{}{}:
	case <-abandoned:
		return renderOutcome{err: errRenderAbandoned}
	}
	defer func() { <-renderPostSlots }()
	if hook := renderPostHook; hook != nil {
		hook()
	}
	return renderOutcome{html: renderPost(g, kind), hidden: g.hidden}
}

// sourceOutcome is a renderOutcome that shows source as text under the
// notice named kind.
func sourceOutcome(kind string, source []byte) renderOutcome {
	var title, body string
	switch kind {
	case noticeRenderSize:
		title = "Shown as plain text"
		body = "This document is over " + strconv.Itoa(maxRenderBytes>>10) + " KiB, more than the reader formats, so it is shown as its source text."
	case noticeRenderDeadline:
		title = "Too slow to render"
		body = "This document took longer than " + renderDeadline.String() + " to format, so it is shown as its source text."
	default: // noticeRenderBusy
		kind = noticeRenderBusy
		title = "Shown as plain text"
		body = "The reader is busy with a document that is slow to render, so this one is shown as its source text."
	}
	return renderOutcome{html: sourceTextDoc(kind, title, body, source), notice: kind}
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
