package docs

import (
	"bytes"
	"fmt"
	"regexp"
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
		// Promotes ```mermaid fences off ast.KindFencedCodeBlock so chroma
		// never claims them — see mermaid.go for why a second renderer
		// alongside the highlighting extension is not an option.
		mermaidExtension{},
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
	allowInlineSVG(p)
	return p
}

// svgPaint matches the values a paint-ish SVG attribute (fill, stroke,
// stop-color) may carry: a keyword, a hex or rgb() color, or a same-document
// url(#id) reference to a gradient or pattern. Anything else — notably a url()
// pointing off-document — is dropped, so a diagram cannot become a tracking
// beacon that reports when the doc was read.
var svgPaint = regexp.MustCompile(`^(?i)(none|transparent|currentColor|inherit|#[0-9a-f]{3,8}|rgba?\([0-9.,%\s]+\)|[a-z]{3,20}|url\(#[\w.:-]+\))$`)

// svgLocalRef matches an attribute that may ONLY reference something inside this
// same document (markers, clip paths, masks). Same reasoning as svgPaint, but
// these have no legitimate non-url value beyond "none".
var svgLocalRef = regexp.MustCompile(`^(?i)(none|url\(#[\w.:-]+\))$`)

// svgNumericish matches lengths, coordinates, and lists of them — digits, signs,
// decimals, exponents, separators, and the unit suffixes SVG allows. Deliberately
// value-shaped rather than permissive: it is what keeps a geometry attribute from
// smuggling a function call or a url().
var svgNumericish = regexp.MustCompile(`^[-+0-9.,eE\s]*(px|em|rem|pt|%)?$`)

// svgTransform matches the SVG transform functions and nothing else.
var svgTransform = regexp.MustCompile(`^(?i)(\s*(matrix|translate|scale|rotate|skewX|skewY)\s*\([-+0-9.,eE\s]*\)\s*)+$`)

// allowInlineSVG opens the sanitizer to hand-authored inline SVG — the reader's
// diagrams-as-source-in-the-doc case (forgectl#93's pan/zoom requirement).
//
// This is an ALLOWLIST, which is the property doing the security work:
// bluemonday drops every element and attribute not named here, so the dangerous
// surface is excluded by omission rather than by enumeration. Specifically NOT
// allowed, and each for a concrete reason:
//
//   - <script> — the obvious one; SVG can carry script exactly like HTML.
//   - on* handlers (onload, onclick, …) — never named below, so always stripped.
//     <svg onload="…"> is the canonical inline-SVG XSS and it dies here.
//   - <foreignObject> — an escape hatch back into arbitrary HTML inside SVG,
//     which would route around every restriction in this list.
//   - <animate>/<animateTransform>/<set> — SMIL can retarget an attribute at
//     runtime (attributeName="href"), turning a static allowlisted document into
//     a mutating one after sanitization has already run.
//   - <image>, <a>, and <use> — all three take a reference that could point
//     off-document, making a read of the doc observable to a third party.
//     <use> is the tempting one to allow; it is not worth a same-origin argument
//     for a local reader.
//   - <style> — a style element inside SVG can reach out of the diagram and
//     restyle the page, including hiding or overlaying content.
//
// The `style` ATTRIBUTE is likewise absent: AllowStyling above grants `class`
// only, so a diagram styles itself through the Artificer token classes
// (.dia-node, .dia-edge) rather than inline CSS.
//
// Note this is NOT the path mermaid output takes. Mermaid renders in the browser,
// after sanitization, so its generated SVG never passes through this policy —
// which is why this list can stay narrow instead of having to accommodate
// everything mermaid emits.
func allowInlineSVG(p *bluemonday.Policy) {
	// Structural and shape elements. No scripting, no animation, no external
	// references — see the doc comment.
	p.AllowElements(
		"svg", "g", "defs", "symbol", "title", "desc",
		"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
		"text", "tspan",
		"marker", "clipPath", "mask",
		"linearGradient", "radialGradient", "stop",
	)

	// The root element's framing attributes. viewBox is what makes pan/zoom
	// possible at all, so it is the load-bearing one here.
	p.AllowAttrs("viewBox", "preserveAspectRatio", "xmlns", "role", "aria-label").OnElements("svg")
	p.AllowAttrs("width", "height").Matching(svgNumericish).OnElements("svg", "rect", "marker", "mask", "symbol")

	// Geometry.
	p.AllowAttrs("d").OnElements("path")
	p.AllowAttrs("points").OnElements("polyline", "polygon")
	p.AllowAttrs("x", "y", "dx", "dy", "rx", "ry").Matching(svgNumericish).
		OnElements("rect", "text", "tspan", "ellipse", "circle")
	p.AllowAttrs("x1", "y1", "x2", "y2").Matching(svgNumericish).
		OnElements("line", "linearGradient")
	p.AllowAttrs("cx", "cy", "r", "fr").Matching(svgNumericish).
		OnElements("circle", "ellipse", "radialGradient")
	p.AllowAttrs("transform").Matching(svgTransform).Globally()

	// Presentation. Paint values are constrained so a url() cannot leave the
	// document.
	p.AllowAttrs("fill", "stroke", "stop-color").Matching(svgPaint).Globally()
	p.AllowAttrs("stroke-width", "stroke-dasharray", "stroke-dashoffset",
		"opacity", "fill-opacity", "stroke-opacity", "stop-opacity", "offset").
		Matching(svgNumericish).Globally()
	p.AllowAttrs("stroke-linecap", "stroke-linejoin", "fill-rule", "stroke-miterlimit",
		"text-anchor", "dominant-baseline", "font-family", "font-size", "font-weight",
		"letter-spacing", "paint-order").Globally()

	// Same-document references only.
	p.AllowAttrs("marker-start", "marker-mid", "marker-end", "clip-path", "mask").
		Matching(svgLocalRef).Globally()
	p.AllowAttrs("gradientUnits", "gradientTransform", "spreadMethod").
		OnElements("linearGradient", "radialGradient")
	p.AllowAttrs("markerWidth", "markerHeight", "refX", "refY", "orient", "markerUnits").
		OnElements("marker")
	p.AllowAttrs("clipPathUnits").OnElements("clipPath")
	p.AllowAttrs("maskUnits", "maskContentUnits").OnElements("mask")
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
