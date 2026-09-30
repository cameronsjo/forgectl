package docs

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// balanceFragment makes the sanitized body safe to drop inside the shell's
// <div class="doc-body">: no end tag in it can close an element the shell
// opened, and nothing it opens stays open past its own end
// (cameronsjo/forgectl#596). bluemonday filters tags one at a time and never
// balances them, so a document's stray "</div>" survives it and closes
// .doc-body early — the rest of the document renders outside it — while an
// unclosed "<div>" takes the shell's own "</div>" for itself and leaves
// .doc-body open until "</main>" forces it shut.
//
// Output that is already balanced comes back byte for byte. The test is the
// browser's own: parseBody runs the HTML5 tree builder over the body inside
// a <div> in a full document, and the body passes when the tree it builds is
// exactly the tags the source wrote, in the same order — every start tag its
// own element, every end tag closing the element it names, nothing dropped,
// inserted, implied or moved (fragmentWellNested). Such a body parses to the
// same tree inside the shell: every end tag finds its element within the
// body, so the scope search never climbs into the shell's div/main/div
// ancestors, none of which a start tag implicitly closes. An HTML start tag
// inside SVG or MathML that breaks out of foreign content (a <table> or <p>
// in an <svg>) pops the foreign elements, which the source never closed
// there, so the streams differ and the body is rebuilt.
//
// It parses a full document rather than calling html.ParseFragment because
// x/net skips that breakout in fragment mode and browsers do not: fragment
// mode reads "<svg><table/></svg>" as a self-closed SVG <table>, while a
// browser builds an HTML <table> after the <svg> that stays open, takes the
// shell's closers for itself, and pulls the status bar into .doc-body.
//
// A well-nested body is rebuilt all the same when the tree holds an SVG-only
// element in HTML content, which the rebuild unwraps (cameronsjo/forgectl#619,
// strayForeignElements).
//
// Anything else is replaced by that tree re-serialized, which is balanced by
// construction, and re-sanitized: re-serializing is a parse-and-render round
// trip, the classic mutation-XSS shape, so the bytes served are once again
// the sanitizer's output. Only a document whose HTML was not well nested
// changes, and it changes into what a browser would have built from it.
//
// It runs after the sanitizer and BEFORE transformCallouts. The callout
// rewrite is balance-preserving by construction — it swaps a real
// <blockquote><p> for one that is still open the same way and adds a fully
// closed title — so the body stays balanced through it, and running it after
// keeps the re-sanitize from stripping the callout chrome (its svg carries
// aria-hidden, which the policy does not allow).
func balanceFragment(sanitized string) string {
	out, _ := balancePasses(sanitized)
	return out
}

// maxBalancePasses bounds the rebuild. One pass is not always a fixed
// point — the tree builder can build a tree whose serialization it would
// parse differently (a heading foster-parented out of a table lands inside
// another heading) — but the next pass parses the output as-is; see
// TestBalanceFragment_RandomTagSoup for the measured bound.
const maxBalancePasses = 4

// Pass counts balancePasses reports for its two fallbacks.
const (
	// passesEscaped: the markup is served as escaped text.
	passesEscaped = -1
	// passesDeep: the body nested past x/net's parse limit and was balanced
	// by balanceDeep instead.
	passesDeep = -2
)

// balancePasses is balanceFragment reporting how many rebuilds it took: 0
// for input returned unchanged, passesDeep or passesEscaped for a fallback.
//
// Each pass costs a parse and a tokenize, and the tree builder's
// adoption-agency steps are super-linear on adversarial misnesting; the
// render-input cap tracked under cameronsjo/forgectl#596 is what bounds that.
func balancePasses(sanitized string) (string, int) {
	s := sanitized
	for pass := range maxBalancePasses + 1 {
		nodes, err := parseBody(s)
		if err != nil {
			// x/net refuses a tree more than 512 open elements deep: a list
			// nested a few hundred levels, or hundreds of unclosed <b>. The
			// tree builder cannot vouch for such a body, so balanceDeep
			// contains it from the token stream instead
			// (cameronsjo/forgectl#623), and the result is sanitized again
			// so the bytes served are the sanitizer's output.
			return sanitizer.Sanitize(balanceDeep(sanitized)), passesDeep
		}
		if fragmentWellNested(s, nodes) && !strayForeignElements(nodes) {
			return s, pass
		}
		if pass == maxBalancePasses {
			break
		}
		root := fragmentContext()
		for _, n := range nodes {
			root.AppendChild(n)
		}
		unwrapStrayForeign(root)
		hoistVoidChildren(root)
		var buf bytes.Buffer
		for n := root.FirstChild; n != nil; n = n.NextSibling {
			if err := html.Render(&buf, n); err != nil {
				return html.EscapeString(sanitized), passesEscaped
			}
		}
		s = string(sanitizer.SanitizeBytes(buf.Bytes()))
	}
	// The rebuild converges well inside maxBalancePasses, and hoistVoidChildren
	// leaves nothing html.Render refuses; if either ever does not hold, show
	// the markup as text rather than let it reach the shell unbalanced.
	return html.EscapeString(sanitized), passesEscaped
}

// bodyDocPrefix places the body where the shell does: inside a <div> in a
// document's <body>, in the "in body" insertion mode.
const bodyDocPrefix = "<!DOCTYPE html><html><head></head><body><div>"

// errNoBodyWrapper reports a parse that did not keep bodyDocPrefix's <div>
// as the first child of <body>. No input is known to cause it — even an
// unsanitized <frameset> leaves the wrapper in place — so it guards the
// assumption, and balancePasses treats it as it treats a parse error.
var errNoBodyWrapper = errors.New("docs: body wrapper missing from parsed document")

// parseBody builds the tree a browser builds for s inside the shell and
// returns its top-level nodes, detached: the wrapper <div>'s children, then
// whatever a stray closer pushed out after the wrapper, in document order.
func parseBody(s string) ([]*html.Node, error) {
	doc, err := html.Parse(strings.NewReader(bodyDocPrefix + s))
	if err != nil {
		return nil, err
	}
	wrap := bodyWrapper(doc)
	if wrap == nil {
		return nil, errNoBodyWrapper
	}
	var nodes []*html.Node
	for n := wrap.FirstChild; n != nil; n = n.NextSibling {
		nodes = append(nodes, n)
	}
	for a := wrap; a.Parent != nil; a = a.Parent {
		for n := a.NextSibling; n != nil; n = n.NextSibling {
			nodes = append(nodes, n)
		}
	}
	for _, n := range nodes {
		n.Parent.RemoveChild(n)
	}
	return nodes, nil
}

// bodyWrapper finds bodyDocPrefix's <div>: the first child of <body>.
func bodyWrapper(doc *html.Node) *html.Node {
	for h := doc.FirstChild; h != nil; h = h.NextSibling {
		if h.Type != html.ElementNode || h.DataAtom != atom.Html {
			continue
		}
		for b := h.FirstChild; b != nil; b = b.NextSibling {
			if b.Type == html.ElementNode && b.DataAtom == atom.Body {
				if w := b.FirstChild; w != nil && w.Type == html.ElementNode && w.DataAtom == atom.Div {
					return w
				}
				return nil
			}
		}
	}
	return nil
}

// svgOnlyElements are the policy's SVG element names other than <svg> itself,
// lowercased as the tree builder leaves them in HTML content. An <svg> start
// tag always opens SVG content, so only these can land in HTML.
var svgOnlyElements = func() map[string]bool {
	m := map[string]bool{}
	for _, name := range svgElements {
		if name != "svg" {
			m[strings.ToLower(name)] = true
		}
	}
	return m
}()

// strayForeignElements reports whether the tree holds an SVG-only element in
// HTML content (cameronsjo/forgectl#619). bluemonday allows the SVG names
// anywhere, so prose like "cat <path>: No such file" reaches the tree builder
// as an HTML <path> that is well nested and would otherwise be served as is.
// The test is the element's namespace, not an <svg> ancestor: it is what the
// browser decided, so a <path> after an HTML tag broke out of the <svg>, or
// in an HTML island under an integration point, counts as stray even with an
// <svg> above it, and one in an <svg> nested in that island does not.
func strayForeignElements(nodes []*html.Node) bool {
	return slices.ContainsFunc(nodes, hasStrayForeign)
}

// hasStrayForeign reports whether n or a descendant is a stray SVG-only
// element.
func hasStrayForeign(n *html.Node) bool {
	if isStrayForeign(n) {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasStrayForeign(c) {
			return true
		}
	}
	return false
}

// isStrayForeign reports whether n is an SVG-only element in HTML content.
func isStrayForeign(n *html.Node) bool {
	return n.Type == html.ElementNode && n.Namespace == "" && svgOnlyElements[n.Data]
}

// unwrapStrayForeign replaces every SVG-only element in HTML content under n
// with its children, as bluemonday treats an element it does not allow: the
// tag goes, the content stays.
func unwrapStrayForeign(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		unwrapStrayForeign(c)
		if isStrayForeign(c) {
			for gc := c.FirstChild; gc != nil; gc = c.FirstChild {
				c.RemoveChild(gc)
				n.InsertBefore(gc, c)
			}
			n.RemoveChild(c)
		}
		c = next
	}
}

// hoistVoidChildren moves the children of any element bearing a void
// element's name out to follow it. The tree builder can hang content under
// one — a "</br>" inside SVG becomes an svg-namespace <br> that later text
// lands in — and html.Render refuses such a tree for a void name in any
// namespace ("void element <br> has child nodes"). The next pass re-parses
// whatever this produces, so it only has to be renderable, not exact.
func hoistVoidChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		hoistVoidChildren(c)
	}
	if n.Type != html.ElementNode || !voidElements[n.Data] {
		return
	}
	for c := n.LastChild; c != nil; c = n.LastChild {
		n.RemoveChild(c)
		n.Parent.InsertBefore(c, n.NextSibling)
	}
}

// fragmentContext is the container a rebuilt body is rendered from: the
// shell's .doc-body <div>.
func fragmentContext() *html.Node {
	return &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
}

// fragmentWellNested reports whether the tag stream of src is exactly the
// open/close stream of the tree the parser built from it.
func fragmentWellNested(src string, nodes []*html.Node) bool {
	var tree []string
	for _, n := range nodes {
		tree = appendTreeTags(tree, n)
	}
	return slices.Equal(sourceTags(src), tree)
}

// appendTreeTags appends n's element open/close events in document order: a
// "+name" when the element opens and a "-name" when it closes. An HTML void
// element has no close. Names are lowercased because the tree builder
// restores SVG's camelCase (linearGradient) and the tokenizer does not.
func appendTreeTags(out []string, n *html.Node) []string {
	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			out = appendTreeTags(out, c)
		}
		return out
	}
	name := strings.ToLower(n.Data)
	out = append(out, "+"+name)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = appendTreeTags(out, c)
	}
	if n.Namespace == "" && voidElements[name] {
		return out
	}
	return append(out, "-"+name)
}

// sourceTags is appendTreeTags's stream read off the source's own tokens.
// A self-closing tag closes itself only in foreign (SVG/MathML) content; in
// HTML the slash is ignored and a non-void element stays open, as a browser
// reads it. Tracking foreign content needs the open-element stack, which is
// only trustworthy while the stream is well nested — and a stream that is not
// can never equal the tree's, which always is.
func sourceTags(src string) []string {
	var out, stack []string
	foreign := 0
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				// The tokenizer has no buffer cap set, so it reports only its
				// reader's errors, and a strings.Reader has none but io.EOF.
				// Were one to appear, the unmatchable stream sends the body
				// down the rebuild path.
				return append(out, "!error")
			}
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			raw, _ := z.TagName()
			name := string(raw)
			out = append(out, "+"+name)
			opensForeign := name == "svg" || name == "math"
			inForeign := foreign > 0 || opensForeign
			if tt == html.SelfClosingTagToken && inForeign {
				out = append(out, "-"+name)
				continue
			}
			if !inForeign && voidElements[name] {
				continue
			}
			stack = append(stack, name)
			if opensForeign {
				foreign++
			}
		case html.EndTagToken:
			raw, _ := z.TagName()
			name := string(raw)
			out = append(out, "-"+name)
			if n := len(stack); n > 0 && stack[n-1] == name {
				stack = stack[:n-1]
				if name == "svg" || name == "math" {
					foreign--
				}
			}
		}
	}
}

// voidElements are the HTML elements with no end tag — x/net/html's own
// render set, so both streams agree on which elements close.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "keygen": true, "link": true,
	"meta": true, "param": true, "source": true, "track": true, "wbr": true,
}

// balanceDeep balances a body too deep for the tree builder, from its token
// stream alone (cameronsjo/forgectl#623). What it guarantees is containment:
// nothing it emits can close an element the shell opened, and nothing it
// opens stays open past its end. It does not promise the tree a browser
// would build — a tokenizer stack cannot follow the tree builder's implied
// closes and adoption-agency moves — only that the document stays inside
// .doc-body however the browser builds it.
//
// Dropping stray end tags is not enough for that on its own. A browser pops
// open elements that no end tag names: a second <li> closes the first and
// every <div> inside it, so in "<li><div><li>x</div>" the "</div>", matched
// on a tokenizer's stack, finds no <div> left in the document and closes
// .doc-body (Chromium, checked with Playwright). No end tag reaches an
// element that is not in the document if no end tag can name one: the
// shell's ancestors of .doc-body are html, body, main and div, so <div>
// becomes <section>, which renders as the same block and keeps its
// attributes, and the other three names are dropped. No start tag reaches
// one either: the implied closes of <li>, <dd> and <dt> stop at <main>, and
// the shell leaves no <p>, heading, <a> or <button> open around the
// document.
//
// Everything else is balance, so the shell's own closers find their
// elements: an end tag with no open element of its name is dropped, one
// that does have one closes everything opened after it, and at the end of
// the body every element still open is closed, innermost first. An end tag
// naming an element the browser already closed implicitly closes nothing
// outside the document, by the argument above (at worst "</p>" inserts an
// empty paragraph). A <table>, cell or caption the document opens — the
// elements a closer's scope search stops at — is always closed by name, so
// the shell's "</div>" is never left out of scope. Of the browser's rules
// for SVG and MathML it models these: a self-closing tag closes only in
// foreign content; an HTML tag that breaks out of foreign content (and a
// "</p>" or "</br>" there) closes the foreign elements first, so a <b> in
// an <svg> ends the <svg> and a later "<a/>" opens an HTML <a> it then
// closes; the HTML integration points (SVG foreignObject and desc, MathML
// annotation-xml with an HTML encoding) and MathML text integration points
// read their children as HTML; and an <svg> under annotation-xml starts a
// new SVG root. Not modelled: the case-adjusted tag and attribute names,
// mglyph and malignmark, and SVG <title> as an integration point (it is
// dropped below as raw text). The elements whose parsing a tag stack
// cannot follow —
// <select>, <template>, the other scope-stopping <object>, <applet> and
// <marquee>, and the raw-text elements (script, style, textarea, title and
// the like) — are dropped with their content kept, escaped where it is raw
// text; the policy allows none of them.
//
// It is linear in the input: each element is pushed and popped once, and an
// end tag with no element of its name is dropped on a count lookup rather
// than a stack search. Its output is sanitized again by its caller.
func balanceDeep(src string) string {
	var (
		b     strings.Builder
		stack []deepElement
		count = map[string]int{}
	)
	closeTo := func(depth int) {
		for len(stack) > depth {
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			count[top.name]--
			_, _ = b.WriteString("</" + top.emit + ">")
		}
	}
	// breakOut pops foreign elements until the current node is HTML or an
	// integration point, as a browser does for an HTML tag inside SVG.
	breakOut := func() {
		n := len(stack)
		for n > 0 && !htmlContext(stack[n-1]) {
			n--
		}
		closeTo(n)
	}
	b.Grow(len(src))
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			closeTo(0)
			return b.String()
		case html.TextToken:
			_, _ = b.WriteString(html.EscapeString(string(z.Text())))
		case html.StartTagToken, html.SelfClosingTagToken:
			raw := string(z.Raw())
			tok := z.Token()
			name := tok.Data
			if deepDropped[name] {
				continue
			}
			foreign := len(stack) > 0 && !htmlContext(stack[len(stack)-1])
			if foreign && name == "svg" && stack[len(stack)-1].ns == "math" && stack[len(stack)-1].name == "annotation-xml" {
				// A browser reads <svg> under <annotation-xml> by the HTML
				// rules: a fresh SVG root, whose <desc> is an integration point.
				foreign = false
			}
			if foreign && breaksOutOfForeign(tok) {
				breakOut()
				foreign = false
			}
			el := deepElement{name: name, emit: name}
			switch {
			case foreign:
				el.ns = stack[len(stack)-1].ns
			case name == "svg" || name == "math":
				el.ns = name
			}
			if el.ns == "" && svgOnlyElements[name] {
				// An SVG-only name in HTML content is dropped, content kept,
				// as balancePasses unwraps it (cameronsjo/forgectl#619). Never
				// pushed, so its end tag finds no element and is dropped too.
				continue
			}
			el.integration = el.ns == "svg" && (name == "foreignobject" || name == "desc") ||
				el.ns == "math" && name == "annotation-xml" && htmlAnnotation(tok)
			if name == "div" {
				tok.Type, tok.Data, tok.DataAtom = html.StartTagToken, "section", atom.Section
				raw, el.emit = tok.String(), "section"
			}
			_, _ = b.WriteString(raw)
			if el.ns != "" && tt == html.SelfClosingTagToken || el.ns == "" && voidElements[name] {
				continue
			}
			stack = append(stack, el)
			count[name]++
		case html.EndTagToken:
			tok := z.Token()
			name := tok.Data
			if deepDropped[name] {
				continue
			}
			if (name == "br" || name == "p") && len(stack) > 0 && !htmlContext(stack[len(stack)-1]) {
				breakOut()
			}
			if count[name] == 0 {
				continue
			}
			i := len(stack) - 1
			for stack[i].name != name {
				i--
			}
			closeTo(i)
		}
	}
}

// deepElement is an element balanceDeep has open.
type deepElement struct {
	name        string // the tag name end tags match against
	emit        string // the tag name balanceDeep wrote, and closes with
	ns          string // "" for HTML, "svg" or "math"
	integration bool   // an HTML integration point
}

// htmlContext reports whether a start tag under el is read as HTML: el is an
// HTML element, an HTML integration point, or a MathML text integration
// point (mglyph and malignmark aside, which no policy allows).
func htmlContext(el deepElement) bool {
	if el.ns == "" || el.integration {
		return true
	}
	if el.ns == "math" {
		switch el.name {
		case "mi", "mo", "mn", "ms", "mtext":
			return true
		}
	}
	return false
}

// breaksOutOfForeign reports whether a start tag inside SVG or MathML makes a
// browser leave foreign content: the HTML tags the spec lists, and a <font>
// carrying color, face or size.
func breaksOutOfForeign(tok html.Token) bool {
	if tok.Data == "font" {
		for _, a := range tok.Attr {
			if a.Key == "color" || a.Key == "face" || a.Key == "size" {
				return true
			}
		}
		return false
	}
	return foreignBreakouts[tok.Data]
}

// htmlAnnotation reports whether a MathML <annotation-xml> is an HTML
// integration point: its encoding names HTML.
func htmlAnnotation(tok html.Token) bool {
	for _, a := range tok.Attr {
		if a.Key == "encoding" {
			e := strings.ToLower(a.Val)
			return e == "text/html" || e == "application/xhtml+xml"
		}
	}
	return false
}

// foreignBreakouts are the start tags that end foreign content (HTML's "in
// foreign content" insertion rules).
var foreignBreakouts = map[string]bool{
	"b": true, "big": true, "blockquote": true, "body": true, "br": true,
	"center": true, "code": true, "dd": true, "div": true, "dl": true,
	"dt": true, "em": true, "embed": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "head": true, "hr": true, "i": true,
	"img": true, "li": true, "listing": true, "menu": true, "meta": true,
	"nobr": true, "ol": true, "p": true, "pre": true, "ruby": true, "s": true,
	"small": true, "span": true, "strong": true, "strike": true, "sub": true,
	"sup": true, "table": true, "tt": true, "u": true, "ul": true, "var": true,
}

// deepDropped are the tags balanceDeep drops, keeping their content: the
// shell's ancestors of .doc-body other than div (which it renames); the
// elements whose parsing a tag stack cannot follow — <select> ignores most
// tags inside it, <template> holds its content out of the document, and
// <object>, <applet> and <marquee> stop a closer's scope search; the
// frameset tags, which can replace <body>; and the raw-text elements, whose
// content the tokenizer hands back as text that balanceDeep escapes. The
// policy allows none of them.
var deepDropped = map[string]bool{
	"html": true, "head": true, "body": true, "main": true,
	"select": true, "option": true, "optgroup": true, "datalist": true,
	"template": true, "object": true, "applet": true, "marquee": true,
	"frameset": true, "frame": true,
	"iframe": true, "noembed": true, "noframes": true, "noscript": true,
	"plaintext": true, "script": true, "style": true, "textarea": true,
	"title": true, "xmp": true,
}
