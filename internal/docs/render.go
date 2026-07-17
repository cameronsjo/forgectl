package docs

import (
	"bytes"
	"fmt"
	"sync"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
)

// chromaStyle is the fixed syntax-highlighting palette. It is deliberately
// one style, not theme-matched to Artificer's light/dark tokens (that's a
// PR2+ concern if it turns out to matter) — chroma's class-based output
// (WithClasses(true) below) means swapping styles later is a CSS-only
// change, never a re-render.
const chromaStyle = "monokai"

// markdown is the shared goldmark instance: GFM (tables, strikethrough,
// autolinks, task lists) + chroma-backed fenced-code highlighting emitting
// CSS classes (never inline styles, so the same render serves both Artificer
// themes once a matching stylesheet exists) + goldmark's own heading-ID
// generation (bluemonday's default policy already allows the "id"
// attribute globally, so anchors survive sanitization for free).
//
// html.WithUnsafe() lets raw HTML blocks and inline HTML in the source pass
// through goldmark's renderer instead of being escaped to text — this is
// safe ONLY because every render is piped through sanitizer (below) before
// it ever reaches a client; WithUnsafe alone, without the bluemonday pass,
// would be an XSS hole.
var markdown = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(
			highlighting.WithStyle(chromaStyle),
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
)

// sanitizer is the bluemonday policy applied to every rendered doc — the
// hygiene pass named in forgectl#93 ("HTML sanitization as ordinary
// hygiene"). Built once; bluemonday policies are safe for concurrent use
// after construction.
var sanitizer = newSanitizer()

func newSanitizer() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// Chroma's class-based token spans (WithClasses(true) above) and
	// goldmark-GFM's table/heading wrapper classes need "class" through the
	// sanitizer; UGCPolicy declines it by default ("we are not allowing
	// users to style their own content" — but here WE are the ones
	// generating the classes, not the document author, so the usual UGC
	// threat model doesn't apply).
	p.AllowStyling()
	return p
}

// chromaCSS is the class-based syntax-highlighting stylesheet for
// chromaStyle, generated once at package init via chroma's own CSS writer
// (the same mechanism goldmark-highlighting uses internally) rather than
// hand-copied — it can never drift from the style actually applied above.
var chromaCSS = mustChromaCSS(chromaStyle)

func mustChromaCSS(styleName string) []byte {
	style := styles.Get(styleName)
	if style == nil {
		panic(fmt.Sprintf("docs: unknown chroma style %q", styleName))
	}
	formatter := chromahtml.New(chromahtml.WithClasses(true))
	var buf bytes.Buffer
	if err := formatter.WriteCSS(&buf, style); err != nil {
		panic(fmt.Sprintf("docs: generating chroma CSS: %v", err))
	}
	return buf.Bytes()
}

// ChromaCSS returns the generated syntax-highlighting stylesheet — served by
// the HTTP layer at a static asset path.
func ChromaCSS() []byte {
	return chromaCSS
}

// renderMu serializes goldmark.Convert calls. goldmark's Markdown value is
// safe for concurrent Convert calls per its own docs in the common case, but
// the highlighting extension's CSSWriter option (unused here) and some
// third-party extensions are documented as not concurrency-safe; a mutex
// costs nothing at docs-server request volumes and removes the question
// entirely.
var renderMu sync.Mutex

// Render converts markdown source to sanitized HTML: goldmark (GFM +
// class-based chroma highlighting) then bluemonday (UGCPolicy + class
// styling allowed). The result is safe to embed directly into a response —
// sanitization is the last step, not a pre-filter goldmark's raw-HTML
// passthrough could bypass.
func Render(source []byte) (string, error) {
	renderMu.Lock()
	var buf bytes.Buffer
	err := markdown.Convert(source, &buf)
	renderMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return string(sanitizer.SanitizeBytes(buf.Bytes())), nil
}
