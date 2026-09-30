package docs

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/frontmatter"
	"gopkg.in/yaml.v3"
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
var markdown = newMarkdown(true, false)

// markdownPlain is the same pipeline without the frontmatter extension. It
// serves any document hasWellFormedFrontmatter rejects: the extension treats
// EVERY leading ---/+++ fence as an opener and, unterminated, consumes to end
// of file — so a doc that merely opens with a thematic break would otherwise
// render empty. Two instances beat one instance plus source rewriting; the
// gate decides which parser sees the bytes, and neither path mutates them.
var markdownPlain = newMarkdown(false, false)

// markdownVault and markdownVaultPlain are the same two pipelines plus the
// Obsidian flavour (obsidian.go: ==highlight==, %%comment%%, #tag, and
// [[wikilink]]s rendered as links to the notes they resolve to when the
// render has an index, and as marked source text otherwise). They serve
// RootVault roots only; a docs root never reaches them, so the docs-root
// instances above stay plain GFM byte for byte.
var (
	markdownVault      = newMarkdown(true, true)
	markdownVaultPlain = newMarkdown(false, true)
)

// newMarkdown builds a render pipeline. extra goes first, ahead of every
// extension, so a goldmark.WithParser in it (blockOnlyTwin) receives every
// parser option the pipeline registers.
func newMarkdown(withFrontmatter, vault bool, extra ...goldmark.Option) goldmark.Markdown {
	extenders := []goldmark.Extender{
		extension.GFM,
		highlighting.NewHighlighting(
			highlighting.WithStyle(chromaStyle),
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
		// Promotes ```mermaid fences off ast.KindFencedCodeBlock so chroma
		// never claims them — see mermaid.go for why a second renderer
		// alongside the highlighting extension is not an option.
		mermaidExtension{},
		// Block-shaped $$…$$ and ```math fences become escaped math markup
		// for a client-side renderer, and so do $…$ and a mid-line $$…$$ on
		// vault roots only — see math.go.
		mathExtension{singleDollar: vault, midLineDisplay: vault},
	}
	if withFrontmatter {
		// Consumes a leading YAML/TOML frontmatter block at parse time, so the
		// delimiters stop rendering as a thematic break + mangled heading. The
		// parsed data is read back per-render (frontmatter.Get) and presented
		// as a collapsed metadata disclosure — see frontmatterHTML.
		extenders = append(extenders, &frontmatter.Extender{})
	}
	if vault {
		extenders = append(extenders, obsidianFlavor{})
	}
	return goldmark.New(append(extra,
		goldmark.WithExtensions(extenders...),
		goldmark.WithParserOptions(headingParserOptions(vault)...),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)...)
}

// headingParserOptions is the ONE place the heading-id rule is configured.
// Both the rendering pipeline above and scanDoc's link parser (linkscan.go)
// build their goldmark instance from it, so the slug the resolver matches an
// anchor against is, by construction, the id the browser is handed — the two
// cannot drift apart through one call site being edited without the other.
//
// A vault instance turns goldmark's auto heading id off: it slugs the raw
// source line, comments included. commentTransformer (obsidian.go), which
// every vault instance carries through obsidianFlavor, sets the id instead
// from the same line with its comments cut out.
func headingParserOptions(vault bool) []parser.Option {
	if vault {
		return nil
	}
	return []parser.Option{parser.WithAutoHeadingID()}
}

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
	// img alt: UGCPolicy limits it to bluemonday's Paragraph pattern, which
	// drops the whole alt for ordinary punctuation (':', '?', '%', '&', '"',
	// '#', ...). Safety does not come from a character allowlist but from
	// bluemonday HTML-escaping every attribute value on output (" becomes
	// &#34;, < becomes &lt;, & becomes &amp;), so no alt can close the
	// attribute or open a tag. attrTextPattern therefore only refuses control
	// characters; tab, LF and CR stay because a markdown alt may span a soft
	// line break. bluemonday v1.0.27 ORs the policies registered for an
	// attribute, so this rule widens what UGCPolicy admits and the global
	// Paragraph rule still applies alongside it (it also admits \f).
	p.AllowAttrs("alt").Matching(attrTextPattern).OnElements("img")
	// title: same drop, same fix, on the two elements markdown emits a title
	// for (link and image). The global title rule is untouched, and being
	// OR'd with it this only ever admits more, never less, on a and img.
	p.AllowAttrs("title").Matching(attrTextPattern).OnElements("a", "img")
	allowInlineSVG(p)
	return p
}

// attrTextPattern accepts any attribute text free of control characters.
var attrTextPattern = regexp.MustCompile(`^[^\x00-\x08\x0B\x0C\x0E-\x1F\x7F]*$`)

// svgPaint matches the values a paint-ish SVG attribute (fill, stroke,
// stop-color) may carry: a keyword, a hex or rgb() color, or a same-document
// url(#id) reference to a gradient or pattern. Anything else — notably a url()
// pointing off-document — is dropped.
//
// Deliberately NOT claiming this makes the reader beacon-proof. It does not: an
// ordinary markdown image reaches a remote URL by design under UGCPolicy. What
// this narrows is the SVG surface specifically, so opening it adds no new
// exfiltration path of its own.
var svgPaint = regexp.MustCompile(`^(?i)(none|transparent|currentColor|inherit|#[0-9a-f]{3,8}|rgba?\([0-9.,%\s]+\)|[a-z]{3,20}|url\(#[\w.:-]+\))$`)

// svgLocalRef matches an attribute that may ONLY reference something inside this
// same document (markers, clip paths, masks). Same reasoning as svgPaint, but
// these have no legitimate non-url value beyond "none".
var svgLocalRef = regexp.MustCompile(`^(?i)(none|url\(#[\w.:-]+\))$`)

// svgNumericish matches lengths, coordinates, and lists of them — digits, signs,
// decimals, exponents, separators, and the unit suffixes SVG allows. Deliberately
// value-shaped rather than permissive: it is what keeps a geometry attribute from
// smuggling a function call or a url().
// At least one digit is required: the previous form also matched the empty
// string and digit-free junk like "e", so an attribute named "numericish"
// accepted nothing-at-all. Neither was dangerous (no function call, url, or
// quote can pass), but a value-shaped constraint should reject non-values.
var svgNumericish = regexp.MustCompile(`^[-+.,\s]*[0-9][-+0-9.,eE\s]*(px|em|rem|pt|%)?$`)

// svgTransform matches the SVG transform functions and nothing else.
var svgTransform = regexp.MustCompile(`^(?i)(\s*(matrix|translate|scale|rotate|skewX|skewY)\s*\([-+0-9.,eE\s]*\)\s*)+$`)

// svgNamespace accepts only the canonical, case-sensitive SVG namespace.
// Omission remains valid; any authored alternative loses this attribute while
// the rest of the allowlisted SVG remains intact.
var svgNamespace = regexp.MustCompile(`^http://www\.w3\.org/2000/svg$`)

func dropDuplicateSVGNamespaces(rendered []byte) []byte {
	var sanitized []byte
	for searchFrom := 0; searchFrom < len(rendered); {
		start := findSVGOpeningTag(rendered, searchFrom)
		if start < 0 {
			return append(sanitized, rendered[searchFrom:]...)
		}
		sanitized = append(sanitized, rendered[searchFrom:start]...)
		end, attributes, malformed := scanSVGOpeningTag(rendered, start)
		if end < 0 {
			// A quote-aware scan could not find the end of this opening tag.
			// Its boundary is ambiguous, so dropping the remainder is the only
			// repair that cannot accidentally promote attacker text into HTML.
			return sanitized
		}
		if malformed {
			sanitized = append(sanitized, "<svg>"...)
		} else if len(attributes) < 2 {
			sanitized = append(sanitized, rendered[start:end]...)
		} else {
			cursor := start
			for _, attribute := range attributes {
				sanitized = append(sanitized, rendered[cursor:attribute[0]]...)
				cursor = attribute[1]
			}
			sanitized = append(sanitized, rendered[cursor:end]...)
		}
		searchFrom = end
	}
	return sanitized
}

func findSVGOpeningTag(rendered []byte, from int) int {
	for i := from; i < len(rendered); i++ {
		if rendered[i] != '<' {
			continue
		}
		if bytes.HasPrefix(rendered[i:], []byte("<!--")) {
			commentEnd := bytes.Index(rendered[i+4:], []byte("-->"))
			if commentEnd < 0 {
				return -1
			}
			i += 4 + commentEnd + 2
			continue
		}
		if bytes.HasPrefix(rendered[i:], []byte("<![CDATA[")) {
			cdataEnd := bytes.Index(rendered[i+9:], []byte("]]>"))
			if cdataEnd < 0 {
				return -1
			}
			i += 9 + cdataEnd + 2
			continue
		}
		if i+4 <= len(rendered) && equalASCIIFold(rendered[i+1:i+4], "svg") &&
			(i+4 == len(rendered) || isHTMLSpace(rendered[i+4]) || rendered[i+4] == '>' || rendered[i+4] == '/') {
			return i
		}
		end := markupTagEnd(rendered, i)
		if end < 0 {
			return -1
		}
		i = end - 1
	}
	return -1
}

func markupTagEnd(rendered []byte, start int) int {
	quote := byte(0)
	for i := start + 1; i < len(rendered); i++ {
		if quote != 0 {
			if rendered[i] == quote {
				quote = 0
			}
			continue
		}
		if rendered[i] == '\'' || rendered[i] == '"' {
			quote = rendered[i]
			continue
		}
		if rendered[i] == '>' {
			return i + 1
		}
	}
	return -1
}

func scanSVGOpeningTag(rendered []byte, start int) (int, [][2]int, bool) {
	quote := byte(0)
	end := -1
	for i := start + 4; i < len(rendered); i++ {
		if quote != 0 {
			if rendered[i] == quote {
				quote = 0
			}
			continue
		}
		if rendered[i] == '\'' || rendered[i] == '"' {
			quote = rendered[i]
		} else if rendered[i] == '>' {
			end = i + 1
			break
		}
	}
	if end < 0 {
		return -1, nil, false
	}

	var attributes [][2]int
	malformed := false
	for cursor := start + 4; cursor < end-1; {
		for cursor < end-1 && isHTMLSpace(rendered[cursor]) {
			cursor++
		}
		if cursor >= end-1 || rendered[cursor] == '/' {
			break
		}
		attributeStart := cursor
		for cursor < end-1 && !isHTMLSpace(rendered[cursor]) && rendered[cursor] != '=' && rendered[cursor] != '>' && rendered[cursor] != '/' {
			cursor++
		}
		nameEnd := cursor
		if nameEnd == attributeStart {
			return end, attributes, true
		}
		isNamespace := equalASCIIFold(rendered[attributeStart:nameEnd], "xmlns")
		for cursor < end-1 && isHTMLSpace(rendered[cursor]) {
			cursor++
		}
		hasValue := cursor < end-1 && rendered[cursor] == '='
		if hasValue {
			cursor++
			for cursor < end-1 && isHTMLSpace(rendered[cursor]) {
				cursor++
			}
			if cursor >= end-1 {
				return end, attributes, malformed || isNamespace
			}
			if rendered[cursor] == '\'' || rendered[cursor] == '"' {
				valueQuote := rendered[cursor]
				cursor++
				for cursor < end-1 && rendered[cursor] != valueQuote {
					cursor++
				}
				if cursor >= end-1 {
					return end, attributes, malformed || isNamespace
				}
				cursor++
			} else {
				for cursor < end-1 && !isHTMLSpace(rendered[cursor]) && rendered[cursor] != '>' {
					cursor++
				}
			}
		}
		if !isNamespace {
			continue
		}
		if !hasValue {
			malformed = true
		}
		attributes = append(attributes, [2]int{attributeStart, cursor})
	}
	return end, attributes, malformed
}

func isHTMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func equalASCIIFold(value []byte, literal string) bool {
	if len(value) != len(literal) {
		return false
	}
	for i, b := range value {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if b != literal[i] {
			return false
		}
	}
	return true
}

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
//   - <title> and <desc> — x/net/html tokenizes <title> as RAW TEXT, so an
//     UNCLOSED one swallows the remainder of the rendered document into
//     unrendered title text. A document could then display something materially
//     different from what it says, which is the single thing a reader whose whole
//     job is "show me what this file actually says" must never do.
//     role/aria-label on <svg> (allowed below) covers the accessible-name need.
//
// Denying an element is NOT sufficient on its own, and that is what makes the
// SkipElementsContent call below load-bearing rather than decorative.
// bluemonday drops a disallowed element's TAG but HOISTS ITS CHILDREN — a whole
// subtree is discarded only for elements in its skip-content set. So denying
// <foreignObject> alone turns
//
//	<svg><foreignObject><img src="http://evil/beacon.png"></foreignObject></svg>
//
// into <svg><img src="http://evil/beacon.png"></svg>; and because <img> and <a>
// are HTML-breakout tags inside SVG, the browser makes those live HTML. The
// containers therefore have to be named as skip-content so their contents leave
// with them.
//
// Scoping this honestly: it is not a NEW capability. An ordinary markdown image
// (![](http://evil/x.png)) reaches a remote URL by design under UGCPolicy, so a
// document could already cause a request. What the skip set buys is that opening
// the SVG surface does not silently widen the ways to do it — and that the
// comments here stop asserting a guarantee the code did not have.
//
// The `style` ATTRIBUTE is likewise absent: AllowStyling above grants `class`
// only, so a diagram styles itself through the Artificer token classes
// (.dia-node, .dia-edge) rather than inline CSS.
//
// Note this is NOT the path mermaid output takes. Mermaid renders in the browser,
// after sanitization, so its generated SVG never passes through this policy —
// which is why this list can stay narrow instead of having to accommodate
// everything mermaid emits.
//
// bluemonday has no context rules, so it allows these names anywhere, not only
// inside an <svg>: prose like "cat <path>: No such file" would otherwise reach
// the page as an open HTML <path>. balancePasses removes them outside SVG
// content (cameronsjo/forgectl#619) — see strayForeignElements.
func allowInlineSVG(p *bluemonday.Policy) {
	// Structural and shape elements. No scripting, no animation, no external
	// references — see the doc comment.
	p.AllowElements(svgElements...)

	// Discard these elements' CONTENTS along with their tags. Without this,
	// bluemonday hoists a denied container's children into the surviving SVG —
	// see the doc comment above for the <foreignObject><img> case this closes.
	// "a" is deliberately absent: UGCPolicy allows <a> for ordinary markdown
	// links, and an allowed element wins over a skip-content entry, so listing it
	// would be a no-op that implies a guarantee we do not have. An <img> inside an
	// SVG <a> therefore survives — identically to an ordinary markdown image, and
	// for the same reason. That is the pre-existing UGCPolicy contract, not
	// something this allowlist widens.
	p.SkipElementsContent(
		"foreignObject", "use", "image",
		"animate", "animateTransform", "animatemotion", "set",
		"filter", "pattern", "title", "desc",
	)

	// Grouping and definition containers legitimately carry no attributes.
	// bluemonday drops an allowed element's tag when zero attributes survive
	// unless it is opted in here — and for <defs>/<mask> that is not cosmetic:
	// losing the wrapper promotes definition-only geometry into PAINTED shapes,
	// so a masked diagram renders its mask as a visible rectangle.
	p.AllowNoAttrs().OnElements(
		"svg", "g", "defs", "symbol", "mask", "clipPath", "marker",
		"path", "text", "tspan", "linearGradient", "radialGradient",
	)

	// The root element's framing attributes. viewBox is what makes pan/zoom
	// possible at all, so it is the load-bearing one here.
	p.AllowAttrs("viewBox", "preserveAspectRatio", "role", "aria-label").OnElements("svg")
	p.AllowAttrs("xmlns").Matching(svgNamespace).OnElements("svg")
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
	// Scoped to non-root SVG elements rather than Globally(). On the OUTERMOST
	// <svg>, transform is a presentation attribute mapping to CSS transform — an
	// overlay primitive, which a document could use to scale a fake prompt over
	// the page. The pan/zoom wrapper's overflow:hidden happens to clip that
	// today, but that makes containment depend on a stylesheet and a script
	// loading, which is not where a security boundary belongs.
	p.AllowAttrs("transform").Matching(svgTransform).OnElements(
		"g", "defs", "symbol", "mask", "clipPath", "marker",
		"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
		"text", "tspan",
	)

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

// svgElements are the SVG elements allowInlineSVG allows. Every name is
// SVG-only — none is also an HTML element — which is what lets the balancer
// drop any of them it finds in HTML content (svgOnlyElements). Keep it so: an
// HTML name added here would be unwrapped wherever the document uses it.
var svgElements = []string{
	"svg", "g", "defs", "symbol",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan",
	"marker", "clipPath", "mask",
	"linearGradient", "radialGradient", "stop",
}

// ChromaCSS returns the syntax-highlighting stylesheet served at
// /assets/chroma.css. It is the hand-authored Artificer token mapping
// (assets/chroma.css), not a chroma-generated style sheet: chroma's
// class-based output names token TYPES, so the stylesheet is free to bind
// them to the design system's theme-following vars instead of any fixed
// palette. The trade against the old generated-monokai approach is
// deliberate — a generated sheet could never drift from a style the page
// no longer wants, while this one follows the theme the page actually has.
func ChromaCSS() []byte {
	return chromaArtificerCSS
}

// renderMu serializes renders. goldmark's Markdown value is
// safe for concurrent Convert calls per its own docs in the common case, but
// the highlighting extension's CSSWriter option (unused here) and some
// third-party extensions are documented as not concurrency-safe; a mutex
// costs nothing at docs-server request volumes and removes the question
// entirely. It is a one-slot semaphore rather than a sync.Mutex so that
// acquiring it can give up at the render deadline or when the request goes
// away. A request holds it only while goldmark runs, and never past
// renderDeadline; a render that outlives that keeps goldmark to itself
// through renderInFlight instead (renderdeadline.go).
var renderMu = make(chan struct{}, 1)

// Render converts markdown source to sanitized HTML: goldmark (GFM +
// class-based chroma highlighting) then bluemonday (UGCPolicy + class
// styling allowed). The result is safe to embed directly into a response —
// sanitization is the last step, not a pre-filter goldmark's raw-HTML
// passthrough could bypass.
//
// A leading frontmatter block, when present, is rendered as a collapsed
// disclosure ABOVE the sanitized body. That block is generated here from
// parsed values with every fragment HTML-escaped, which is why prepending it
// after sanitization does not reopen the XSS door the sanitizer closes: the
// document author's bytes only ever reach it through html.EscapeString.
// Building it post-sanitizer keeps the bluemonday allowlist untouched. The
// same holds for the fixed skip-content banner (skipContentBanner) placed
// above both when an unclosed skip-content element swallowed the tail. (An
// earlier version of this comment said details/summary stay denied for
// document-authored HTML; UGCPolicy has always allowed them, with only the
// `open` attribute on <details>. TestRender_DetailsAllowedWithOpenOnly pins
// that.)
//
// Render is the docs-root pipeline (plain GFM); render picks by root kind.
func Render(source []byte) (string, error) {
	return render(source, RootDocs)
}

// render is Render for a given root kind: a RootVault root gets the Obsidian
// flavour and callout aliases, every other kind the plain GFM pipeline.
func render(source []byte, kind RootKind) (string, error) {
	return renderWith(source, kind, nil)
}

// renderWith is render with a wikilink resolver for a vault page. A nil
// resolve, or any other root kind, renders exactly as render does. resolve
// runs in the goldmark stage, one render at a time, so it must never
// render.
func renderWith(source []byte, kind RootKind, resolve wikilinkResolver) (string, error) {
	rendered, _, err := renderHidden(source, kind, resolve)
	return rendered, err
}

// renderHidden is renderWith that also returns the source range of every
// %% comment the page hides (vault roots only; nil otherwise), taken from
// the same parse the page is rendered from, so countWords can leave comment
// text out of the reading estimate.
func renderHidden(source []byte, kind RootKind, resolve wikilinkResolver) (string, []text.Segment, error) {
	out := renderHiddenContext(context.Background(), source, kind, resolve)
	return out.html, out.hidden, out.err
}

// renderHiddenContext is renderHidden for a request: the render gives up,
// and starts no render, once ctx is done, and it reports which notice, if
// any, the page shows instead of the formatted document.
func renderHiddenContext(ctx context.Context, source []byte, kind RootKind, resolve wikilinkResolver) renderOutcome {
	// A document over the render-CPU cap is shown as its source text,
	// before any parse, the frontmatter decode included (renderdeadline.go).
	if len(source) > maxRenderBytes {
		return sourceOutcome(noticeRenderSize, source)
	}
	// Route through the frontmatter-aware parser only when a well-formed
	// block actually opens the document. The extension's opener is greedy —
	// any leading --- fence starts a block, and an unterminated one consumes
	// the REST OF THE FILE — so without this gate a doc opening with a
	// thematic break renders as an empty page.
	withFrontmatter := hasWellFormedFrontmatter(source)
	var md goldmark.Markdown
	switch {
	case kind == RootVault && withFrontmatter:
		md = markdownVault
	case kind == RootVault:
		md = markdownVaultPlain
	case withFrontmatter:
		md = markdown
	default:
		md = markdownPlain
	}
	return renderBounded(ctx, md, source, kind, resolve)
}

// goldmarkOutput is what the goldmark stage hands the post-processing
// stage.
type goldmarkOutput struct {
	html   []byte
	pc     parser.Context
	hidden []text.Segment
}

// renderGoldmark is the goldmark stage: parse and render. It runs only on a
// render goroutine renderBounded starts, never two at a time, and may
// outlive the request that started it, so it must not touch anything
// request-scoped beyond its arguments.
func renderGoldmark(md goldmark.Markdown, source []byte, kind RootKind, resolve wikilinkResolver) (goldmarkOutput, error) {
	var buf bytes.Buffer
	ctx := newParseContext()
	if kind == RootVault && resolve != nil {
		ctx.Set(wikilinkResolverKey, resolve)
	}
	// md.Convert split in two, so the parsed tree can be read for comments.
	doc := md.Parser().Parse(text.NewReader(source), parser.WithContext(ctx))
	err := md.Renderer().Render(&buf, source, doc)
	var hidden []text.Segment
	if err == nil && kind == RootVault {
		hidden = hiddenComments(doc, ctx)
	}
	if err != nil {
		return goldmarkOutput{}, fmt.Errorf("render markdown: %w", err)
	}
	return goldmarkOutput{html: buf.Bytes(), pc: ctx, hidden: hidden}, nil
}

// renderPost is the stage after goldmark: the sanitizer, the balancer and
// the post-sanitizer additions. None of it shares state between renders
// (the bluemonday policy is read-only once built, and every other step
// works on its arguments), so it runs outside renderMu, as it always has.
func renderPost(g goldmarkOutput, kind RootKind) string {
	input := dropDuplicateSVGNamespaces(g.html)
	body := stripChromeClasses(balanceFragment(string(sanitizer.SanitizeBytes(input))))
	// An unclosed skip-content element makes the sanitizer drop the rest of
	// the document (forgectl#622). The sanitizer is left exactly as it is,
	// because that skip set is what keeps denied SVG containers' children
	// inert; the reader is told instead, with a fixed banner.
	notice := ""
	if name, ok := unclosedSkipContent(input); ok {
		notice = skipContentBanner(name)
	}
	return notice + frontmatterHTML(g.pc) + transformCallouts(body, kind)
}

// hiddenComments returns the source range of every %% comment in a parsed
// vault document: each inline comment span, each block comment's lines
// (opener and closer included), and each comment in a paragraph
// commentTransformer removed from the tree.
func hiddenComments(doc ast.Node, pc parser.Context) []text.Segment {
	var hidden []text.Segment
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := n.(type) {
		case *commentSpanNode:
			hidden = append(hidden, text.NewSegment(c.Start, c.Stop))
			return ast.WalkSkipChildren, nil
		case *commentBlockNode:
			if lines := c.Lines(); lines.Len() > 0 {
				hidden = append(hidden, text.NewSegment(lines.At(0).Start, lines.At(lines.Len()-1).Stop))
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return append(hidden, removedComments(pc)...)
}

// cutSegments returns source with every segment in cuts removed; cuts may
// overlap and come in any order.
func cutSegments(source []byte, cuts []text.Segment) []byte {
	if len(cuts) == 0 {
		return source
	}
	sorted := append([]text.Segment(nil), cuts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	out := make([]byte, 0, len(source))
	at := 0
	for _, c := range sorted {
		lo, hi := max(c.Start, at), min(c.Stop, len(source))
		if lo >= hi {
			continue
		}
		out = append(out, source[at:lo]...)
		at = hi
	}
	return append(out, source[at:]...)
}

// OutlineItem is one "On this page" entry — an h2 or h3 with the id
// goldmark's auto-heading pass assigned it. The h1 stays out: it is the page
// title, already the first thing on the page, and repeating it as the
// outline's first row only pushed the sections down. h4+ stays out too.
type OutlineItem struct {
	Level int
	Text  string
	ID    string
}

// RenderedDoc is Render plus the page furniture the v2 shell wants: the
// heading outline and a word count for the status bar.
type RenderedDoc struct {
	HTML    string
	Outline []OutlineItem
	Words   int
	Minutes int
	// Notice names the notice the page shows in place of the formatted
	// document (its data-forgectl-notice value), or is empty when the
	// document was formatted.
	Notice string
}

// RenderDoc renders a docs-root document and derives its outline and
// reading stats.
func RenderDoc(source []byte) (RenderedDoc, error) {
	return RenderDocFor(RootDocs, source, nil, nil)
}

// RenderDocFor is RenderDoc for a document in a root of the given kind. For
// a vault page, idx and from (the page's own indexed Doc) resolve its
// wikilinks into links; with either nil, every wikilink renders as an
// unresolved miss instead.
func RenderDocFor(kind RootKind, source []byte, idx *Index, from *Doc) (RenderedDoc, error) {
	return RenderDocForContext(context.Background(), kind, source, idx, from)
}

// RenderDocForContext is RenderDocFor for a request: once ctx is done the
// render gives up with ctx's error and starts no render.
func RenderDocForContext(ctx context.Context, kind RootKind, source []byte, idx *Index, from *Doc) (RenderedDoc, error) {
	var resolve wikilinkResolver
	if idx != nil && from != nil {
		budget := newFragmentBudget()
		resolve = wikilinkResolver(func(ref LinkRef) (string, Miss) {
			return idx.wikilinkTarget(from, ref, budget)
		})
	}
	out := renderHiddenContext(ctx, source, kind, resolve)
	if out.err != nil {
		return RenderedDoc{}, out.err
	}
	rendered := out.html
	// A vault page's %% comments are not on the page, so they are not read.
	words := countWords(cutSegments(source, out.hidden))
	minutes := (words + 199) / 200
	if minutes < 1 {
		minutes = 1
	}
	return RenderedDoc{
		HTML:    rendered,
		Outline: extractOutline(rendered),
		Words:   words,
		Minutes: minutes,
		Notice:  out.notice,
	}, nil
}

// countWords counts the body's words, skipping a well-formed frontmatter
// block so metadata doesn't inflate the reading estimate.
func countWords(source []byte) int {
	body := source
	if fm, ok := splitFrontmatter(source); ok {
		body = fm.body
	}
	return len(strings.Fields(string(body)))
}

// outlineHeading matches the h2/h3 elements of OUR rendered output — this
// scans HTML the pipeline just produced, never document-authored bytes, so a
// regexp over the known goldmark shape is sufficient.
var outlineHeading = regexp.MustCompile(`(?s)<h([23]) id="([^"]+)">(.*?)</h[23]>`)

// stripTags removes inline markup from a heading's rendered text.
var stripTags = regexp.MustCompile(`<[^>]*>`)

// outlineMath matches the math span math.go emits inside a heading. The
// outline is plain text the client never typesets, so the span is reduced to
// its TeX source with the $ delimiters dropped ("Energy E=mc^2", not
// "Energy $E=mc^2$"). The class attribute is the exact one math.go writes.
var outlineMath = regexp.MustCompile(`<span class="(` + regexp.QuoteMeta(mathInlineClass) + `|` +
	regexp.QuoteMeta(mathDisplayClass) + `)">(.*?)</span>`)

// outlineText renders a heading's inner HTML as outline text.
func outlineText(inner string) string {
	inner = outlineMath.ReplaceAllStringFunc(inner, func(span string) string {
		m := outlineMath.FindStringSubmatch(span)
		delim := "$"
		if m[1] == mathDisplayClass {
			delim = mathDelim
		}
		return strings.TrimSuffix(strings.TrimPrefix(m[2], delim), delim)
	})
	return strings.TrimSpace(html.UnescapeString(stripTags.ReplaceAllString(inner, "")))
}

func extractOutline(rendered string) []OutlineItem {
	var items []OutlineItem
	for _, m := range outlineHeading.FindAllStringSubmatch(rendered, -1) {
		level := 2
		if m[1] == "3" {
			level = 3
		}
		// The captured text is rendered HTML, so entities are escaped
		// (&amp; etc.). Unescape back to plain text: the template escapes
		// once more on output, and without this step "Q&A" displays as
		// "Q&amp;A".
		items = append(items, OutlineItem{
			Level: level,
			Text:  outlineText(m[3]),
			ID:    m[2],
		})
	}
	return items
}

// calloutTiers maps a GFM alert kind to its Artificer tier and title icon.
// Tier colors per the v2 handoff: note→accent, tip→success,
// warning→attention, danger→urgent; IMPORTANT reads as a note,
// CAUTION as danger — GitHub's five kinds onto four tiers.
var calloutTiers = map[string]calloutStyle{
	"NOTE":      {"note", "Note", calloutStarIcon},
	"IMPORTANT": {"note", "Important", calloutStarIcon},
	"TIP":       {"tip", "Tip", calloutCheckIcon},
	"WARNING":   {"warning", "Warning", calloutTriangleIcon},
	"CAUTION":   {"danger", "Caution", calloutOctagonIcon},
	"DANGER":    {"danger", "Danger", calloutOctagonIcon},
}

// calloutStyle is one callout kind's tier class, fixed title label, and
// title icon. Every field is our own constant; none comes from the document.
type calloutStyle struct{ tier, label, icon string }

// obsidianCalloutTiers is the vault-root callout map, keyed lowercase and
// matched case-insensitively: Obsidian's callout types and their aliases on
// the same four tiers. The six GFM kinds keep their docs-root tier and label,
// so an existing vault doc does not change colour. Labels come from this map,
// never from the author's marker text.
var obsidianCalloutTiers = map[string]calloutStyle{
	"note":      {"note", "Note", calloutStarIcon},
	"important": {"note", "Important", calloutStarIcon},
	"abstract":  {"note", "Abstract", calloutStarIcon},
	"summary":   {"note", "Summary", calloutStarIcon},
	"tldr":      {"note", "Tldr", calloutStarIcon},
	"info":      {"note", "Info", calloutStarIcon},
	"todo":      {"note", "Todo", calloutStarIcon},
	"question":  {"note", "Question", calloutStarIcon},
	"help":      {"note", "Help", calloutStarIcon},
	"faq":       {"note", "FAQ", calloutStarIcon},
	"example":   {"note", "Example", calloutStarIcon},
	"quote":     {"note", "Quote", calloutStarIcon},
	"cite":      {"note", "Cite", calloutStarIcon},
	"tip":       {"tip", "Tip", calloutCheckIcon},
	"hint":      {"tip", "Hint", calloutCheckIcon},
	"success":   {"tip", "Success", calloutCheckIcon},
	"check":     {"tip", "Check", calloutCheckIcon},
	"done":      {"tip", "Done", calloutCheckIcon},
	"warning":   {"warning", "Warning", calloutTriangleIcon},
	"attention": {"warning", "Attention", calloutTriangleIcon},
	"caution":   {"danger", "Caution", calloutOctagonIcon},
	"failure":   {"danger", "Failure", calloutOctagonIcon},
	"fail":      {"danger", "Fail", calloutOctagonIcon},
	"missing":   {"danger", "Missing", calloutOctagonIcon},
	"danger":    {"danger", "Danger", calloutOctagonIcon},
	"error":     {"danger", "Error", calloutOctagonIcon},
	"bug":       {"danger", "Bug", calloutOctagonIcon},
}

// calloutStarIcon is the reference shell's note glyph.
const calloutStarIcon = `<path d="M12 3l1.9 5.8H20l-4.9 3.6 1.9 5.8-5-3.6-5 3.6 1.9-5.8L4 8.8h6.1z"/>`

const calloutCheckIcon = `<circle cx="12" cy="12" r="9"/><path d="m9 12 2 2 4-4"/>`

const calloutTriangleIcon = `<path d="m21.73 18-8-14a2 2 0 0 0-3.46 0l-8 14A2 2 0 0 0 4 20h16a2 2 0 0 0 1.73-2Z"/><path d="M12 9v4"/><path d="M12 17h.01"/>`

const calloutOctagonIcon = `<path d="M7.86 2h8.28L22 7.86v8.28L16.14 22H7.86L2 16.14V7.86L7.86 2Z"/><path d="M12 8v4"/><path d="M12 16h.01"/>`

// calloutBlockquote opens a callout's blockquote: bare, or carrying the one
// attribute the vault pipeline gives a blockquote, a block id from a
// standalone "^id" line after it (standaloneBlockID). Its character class
// is blockIDPattern's, so the kept id holds nothing that needs escaping.
// Group 1 is the id attribute, kept on the rewritten blockquote.
const calloutBlockquote = `<blockquote( id="\^[A-Za-z0-9_-]+")?>`

// calloutOpen matches a sanitized blockquote whose first paragraph opens
// with a GFM alert marker ([!NOTE] etc.). Whatever follows the marker is
// left for calloutTitle.
var calloutOpen = regexp.MustCompile(`(?s)` + calloutBlockquote + `\s*<p>\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION|DANGER)\]`)

// calloutOpenVault is calloutOpen for vault roots: any obsidianCalloutTiers
// key in any case, plus Obsidian's optional fold marker ([!info]- or
// [!info]+), which it swallows: folding is not rendered, so a foldable
// callout shows open. The alternation is built from the map keys,
// so the regexp and the map cannot disagree about which kinds exist.
var calloutOpenVault = regexp.MustCompile(`(?s)` + calloutBlockquote + `\s*<p>\[!(?i:(` + calloutAlternation(obsidianCalloutTiers) + `))\][+-]?`)

// calloutAlternation joins the map's keys, regexp-quoted and sorted (for a
// stable pattern), into an alternation.
func calloutAlternation(m map[string]calloutStyle) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

// transformCallouts rewrites alert blockquotes into tiered callout markup:
// GFM's six uppercase kinds for a docs root, and Obsidian's callout types
// (obsidianCalloutTiers) for a vault root. It runs AFTER sanitization on
// pipeline-produced HTML: the marker arrives as escaped-safe text, and
// everything injected here is our own fixed markup plus, for a custom
// title, plain text that calloutTitle re-escapes. The document author
// contributes only the already-sanitized body that follows the marker,
// which stays where it was.
func transformCallouts(rendered string, kind RootKind) string {
	open, tiers, fold := calloutOpen, calloutTiers, strings.ToUpper
	if kind == RootVault {
		open, tiers, fold = calloutOpenVault, obsidianCalloutTiers, strings.ToLower
	}
	var b strings.Builder
	last := 0
	for _, m := range open.FindAllStringSubmatchIndex(rendered, -1) {
		c, ok := tiers[fold(rendered[m[4]:m[5]])]
		if !ok {
			// (?i) folds more than ToLower undoes (U+017F LATIN SMALL
			// LETTER LONG S matches 's'), so a match can miss the map.
			// Leave that blockquote exactly as it was.
			continue
		}
		title, n := calloutTitle(rendered[m[1]:])
		if title == "" {
			title = c.label
		}
		b.WriteString(rendered[last:m[0]])
		b.WriteString(`<blockquote`)
		if m[2] >= 0 {
			b.WriteString(rendered[m[2]:m[3]])
		}
		b.WriteString(` class="callout ` + c.tier + `"><div class="callout-title"><svg viewBox="0 0 24 24" aria-hidden="true">` + c.icon + `</svg> ` + title + `</div><p>`)
		last = m[1] + n
	}
	if last == 0 {
		return rendered
	}
	b.WriteString(rendered[last:])
	return b.String()
}

// calloutTitle reads an Obsidian custom callout title ("> [!tip] My title")
// from rest, the sanitized HTML right after a callout marker. It returns
// the title as escaped text, and how many bytes of rest it takes up (the
// title and the line break after it, so the body starts on the next line).
//
// Only plain text qualifies: the run up to the first '<' or newline, and
// only when that run ends the line (a newline, a hard break, or the end of
// the paragraph). A title holding any markup, such as emphasis, a code span,
// a link or a tag, returns "" and consumes only the whitespace after the
// marker, which is today's rendering: the fixed label, with the line left
// in the body. The text is unescaped and escaped again, so an entity the
// sanitizer wrote (&amp;) is neither doubled nor turned back into markup.
func calloutTitle(rest string) (string, int) {
	ws := len(rest) - len(strings.TrimLeft(rest, " \t\n\f\r"))
	end := strings.IndexAny(rest, "<\n")
	if end < 0 {
		return "", ws
	}
	raw := strings.TrimSpace(rest[:end])
	if raw == "" {
		return "", ws
	}
	tail := rest[end:]
	switch {
	case strings.HasPrefix(tail, "\n"):
		end++
	case strings.HasPrefix(tail, "<br>\n"):
		end += len("<br>\n")
	case strings.HasPrefix(tail, "</p>"):
		// A title-only callout: the paragraph closes here, as it does
		// after a bare marker.
	default:
		return "", ws
	}
	return html.EscapeString(html.UnescapeString(raw)), end
}

// hasWellFormedFrontmatter reports whether source opens with a frontmatter
// block safe to hand to the frontmatter extension. It mirrors the extension's
// own delimiter rules (a first line of three-plus repeated - or +, closed by
// an identical line) and then applies the judgment the extension skips: a ---
// fence shares syntax with a thematic break, so an unterminated block, or one
// whose body is not a YAML mapping, is markdown — not metadata — and must
// reach the parser that treats it that way. A +++ TOML fence collides with no
// markdown syntax, so termination alone qualifies it.
func hasWellFormedFrontmatter(source []byte) bool {
	_, ok := splitFrontmatter(source)
	return ok
}

// frontmatterBlock is splitFrontmatter's view of a document: the fence byte that
// opened the block, the block's raw bytes (fences excluded), and the body
// that follows the closing fence.
type frontmatterBlock struct {
	delim byte
	block []byte
	body  []byte
}

// splitFrontmatter is the ONE place the frontmatter fence rule lives: a
// first line of three-plus repeated - or +, closed by the first later line
// that repeats the same byte at the same length. A --- block must also
// decode as a YAML mapping (an empty mapping counts) — see
// hasWellFormedFrontmatter for why; a +++ TOML block needs only
// termination. Every consumer — the renderer's well-formedness gate,
// countWords, and scanDoc's alias extraction — reads through this function
// so they can never disagree about where a document's metadata ends and
// its body begins.
func splitFrontmatter(source []byte) (frontmatterBlock, bool) {
	lines := bytes.SplitAfter(source, []byte("\n"))
	if len(lines) == 0 {
		return frontmatterBlock{}, false
	}
	delim, count := frontmatterDelim(bytes.TrimSuffix(lines[0], []byte("\n")))
	if delim == 0 {
		return frontmatterBlock{}, false
	}
	for i := 1; i < len(lines); i++ {
		d, c := frontmatterDelim(bytes.TrimSuffix(lines[i], []byte("\n")))
		if d != delim || c != count {
			continue
		}
		fm := frontmatterBlock{
			delim: delim,
			block: bytes.Join(lines[1:i], nil),
			body:  bytes.Join(lines[i+1:], nil),
		}
		// First matching fence closes the block, same as the extension.
		if delim == '+' {
			return fm, true
		}
		var m map[string]any
		// A nil (empty) mapping still counts: `---` immediately closed by
		// `---` is legal, empty frontmatter, not a pair of thematic breaks.
		if yaml.Unmarshal(fm.block, &m) != nil {
			return frontmatterBlock{}, false
		}
		return fm, true
	}
	return frontmatterBlock{}, false
}

// frontmatterDelim interprets one newline-stripped line as a frontmatter
// fence: the opening byte (- or +) repeated for the whole line, minimum
// three. Returns (0, 0) for anything else. The repeat count matters because
// the closing fence must match it exactly.
func frontmatterDelim(line []byte) (byte, int) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) < 3 {
		return 0, 0
	}
	d := line[0]
	if d != '-' && d != '+' {
		return 0, 0
	}
	for _, c := range line[1:] {
		if c != d {
			return 0, 0
		}
	}
	return d, len(line)
}

// frontmatterHTML renders a document's parsed frontmatter as a collapsed
// Artificer disclosure (accordion + kv grid), or "" when the document has
// none. Key order follows the document; a non-scalar value is shown as its
// YAML flow form rather than flattened.
func frontmatterHTML(ctx parser.Context) string {
	fm := frontmatter.Get(ctx)
	if fm == nil {
		return ""
	}
	var node yaml.Node
	if err := fm.Decode(&node); err != nil || len(node.Content) == 0 {
		// TOML frontmatter (or unparseable YAML) has no yaml.Node form —
		// fall back to the unordered map both formats can decode into.
		return frontmatterHTMLUnordered(fm)
	}
	mapping := node.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return frontmatterHTMLUnordered(fm)
	}
	status, staleAfter := trustFields(mapping)
	tr := evalTrust(status, staleAfter, trustNow())
	var b strings.Builder
	pairs := 0
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		writeKV(&b, key.Value, yamlScalar(value), tr)
		pairs++
	}
	return wrapFrontmatter(b.String(), pairs)
}

func frontmatterHTMLUnordered(fm *frontmatter.Data) string {
	var m map[string]any
	if err := fm.Decode(&m); err != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	pairs := 0
	for _, k := range keys {
		b2, err := yaml.Marshal(m[k])
		if err != nil {
			continue // badge counts rendered pairs, so a skipped key is not counted
		}
		// No trust badges here: OKF frontmatter is YAML, and this fallback
		// serves TOML and YAML the node decode could not read.
		writeKV(&b, k, strings.TrimSpace(string(b2)), trustState{})
		pairs++
	}
	return wrapFrontmatter(b.String(), pairs)
}

func yamlScalar(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// propIcons maps known frontmatter keys to an 11px stroke icon (lucide
// paths); unknown keys fall back to propIconDot. These are OUR markup, never
// document-authored, so they may safely join the post-sanitizer prefix.
var propIcons = map[string]string{
	"status": `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 3"/>`,
	"branch": `<path d="M6 3v12"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>`,
	"next":   `<path d="M5 12h14"/><path d="M13 6l6 6-6 6"/>`,
}

const propIconDot = `<circle cx="12" cy="12" r="3"/>`

func propIconSVG(key string) string {
	body, ok := propIcons[key]
	if !ok {
		body = propIconDot
	}
	// Sizing and stroke live in the shell CSS (.props-row .k svg) — the
	// reference shell owns presentation; this emits geometry only.
	return `<svg viewBox="0 0 24 24" aria-hidden="true">` + body + `</svg>`
}

// writeKV renders one properties row. tr carries the document's evaluated
// OKF trust signals (trust.go): a deprecated status and a passed stale_after
// get a trust badge. The badge classes and the "stale" text are our own
// constants; every authored byte still goes through html.EscapeString.
func writeKV(b *strings.Builder, key, value string, tr trustState) {
	b.WriteString(`<div class="props-row"><span class="k">`)
	b.WriteString(propIconSVG(key))
	b.WriteString(html.EscapeString(key))
	b.WriteString(`</span>`)
	switch {
	case key == "status" && tr.Deprecated:
		b.WriteString(`<span class="v"><span class="trust-badge trust-badge--deprecated">`)
		b.WriteString(html.EscapeString(value))
		b.WriteString(`</span></span>`)
	case key == "stale_after":
		b.WriteString(`<span class="v dt">`)
		b.WriteString(html.EscapeString(value))
		if tr.Stale {
			b.WriteString(`<span class="trust-badge trust-badge--stale">stale</span>`)
		}
		b.WriteString(`</span>`)
	case key == "status":
		// Enum-ish values read as a chip.
		b.WriteString(`<span class="v"><span class="status-chip">`)
		b.WriteString(html.EscapeString(value))
		b.WriteString(`</span></span>`)
	case key == "branch" || strings.Contains(value, "/"):
		// Paths and branches align on tabular numerals (reference .v.dt).
		b.WriteString(`<span class="v dt">`)
		b.WriteString(html.EscapeString(value))
		b.WriteString(`</span>`)
	default:
		b.WriteString(`<span class="v">`)
		b.WriteString(html.EscapeString(value))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</div>`)
}

// wrapFrontmatter renders the Obsidian-style properties block — always
// visible, no interaction cost (docs-reader-v2 handoff; supersedes the
// collapsed-disclosure treatment #430 shipped).
func wrapFrontmatter(kvBody string, pairs int) string {
	if pairs == 0 {
		return ""
	}
	// data-forgectl-props marks the reader's own block, which the shell lifts
	// above a doc's tooltips (forgectl#759). A doc cannot forge it: the
	// sanitizer strips every data-* attribute from author HTML.
	return `<div class="props" data-forgectl-props>` + kvBody + `</div>`
}
