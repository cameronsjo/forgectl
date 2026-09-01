package docs

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"sync"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
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
var markdown = newMarkdown(true)

// markdownPlain is the same pipeline without the frontmatter extension. It
// serves any document hasWellFormedFrontmatter rejects: the extension treats
// EVERY leading ---/+++ fence as an opener and, unterminated, consumes to end
// of file — so a doc that merely opens with a thematic break would otherwise
// render empty. Two instances beat one instance plus source rewriting; the
// gate decides which parser sees the bytes, and neither path mutates them.
var markdownPlain = newMarkdown(false)

func newMarkdown(withFrontmatter bool) goldmark.Markdown {
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
	}
	if withFrontmatter {
		// Consumes a leading YAML/TOML frontmatter block at parse time, so the
		// delimiters stop rendering as a thematic break + mangled heading. The
		// parsed data is read back per-render (frontmatter.Get) and presented
		// as a collapsed metadata disclosure — see frontmatterHTML.
		extenders = append(extenders, &frontmatter.Extender{})
	}
	return goldmark.New(
		goldmark.WithExtensions(extenders...),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
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
	allowInlineSVG(p)
	return p
}

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
func allowInlineSVG(p *bluemonday.Policy) {
	// Structural and shape elements. No scripting, no animation, no external
	// references — see the doc comment.
	p.AllowElements(
		"svg", "g", "defs", "symbol",
		"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
		"text", "tspan",
		"marker", "clipPath", "mask",
		"linearGradient", "radialGradient", "stop",
	)

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
//
// A leading frontmatter block, when present, is rendered as a collapsed
// disclosure ABOVE the sanitized body. That block is generated here from
// parsed values with every fragment HTML-escaped, which is why prepending it
// after sanitization does not reopen the XSS door the sanitizer closes: the
// document author's bytes only ever reach it through html.EscapeString.
// Building it post-sanitizer keeps the bluemonday allowlist untouched —
// details/summary/dl stay denied for document-authored HTML.
func Render(source []byte) (string, error) {
	// Route through the frontmatter-aware parser only when a well-formed
	// block actually opens the document. The extension's opener is greedy —
	// any leading --- fence starts a block, and an unterminated one consumes
	// the REST OF THE FILE — so without this gate a doc opening with a
	// thematic break renders as an empty page.
	md := markdown
	if !hasWellFormedFrontmatter(source) {
		md = markdownPlain
	}
	renderMu.Lock()
	var buf bytes.Buffer
	ctx := parser.NewContext()
	err := md.Convert(source, &buf, parser.WithContext(ctx))
	renderMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	body := string(sanitizer.SanitizeBytes(dropDuplicateSVGNamespaces(buf.Bytes())))
	return frontmatterHTML(ctx) + body, nil
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
	lines := bytes.Split(source, []byte("\n"))
	delim, count := frontmatterDelim(lines[0])
	if delim == 0 {
		return false
	}
	for i := 1; i < len(lines); i++ {
		d, c := frontmatterDelim(lines[i])
		if d != delim || c != count {
			continue
		}
		// First matching fence closes the block, same as the extension.
		if delim == '+' {
			return true
		}
		var m map[string]any
		block := bytes.Join(lines[1:i], []byte("\n"))
		// A nil (empty) mapping still counts: `---` immediately closed by
		// `---` is legal, empty frontmatter, not a pair of thematic breaks.
		return yaml.Unmarshal(block, &m) == nil
	}
	return false
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
	var b strings.Builder
	pairs := 0
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		writeKV(&b, key.Value, yamlScalar(value))
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
		writeKV(&b, k, strings.TrimSpace(string(b2)))
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

func writeKV(b *strings.Builder, key, value string) {
	b.WriteString("<dt>")
	b.WriteString(html.EscapeString(key))
	b.WriteString("</dt><dd>")
	b.WriteString(html.EscapeString(value))
	b.WriteString("</dd>")
}

func wrapFrontmatter(kvBody string, pairs int) string {
	if pairs == 0 {
		return ""
	}
	noun := "keys"
	if pairs == 1 {
		noun = "key"
	}
	return fmt.Sprintf(`<div class="accordion surface-tool"><details class="frontmatter"><summary>Front matter <span class="badge">%d %s</span></summary><div class="accordion__body"><dl class="kv">%s</dl></div></details></div>`,
		pairs, noun, kvBody)
}
