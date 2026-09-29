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

// balancePasses is balanceFragment reporting how many rebuilds it took: 0
// for input returned unchanged, -1 for a fallback.
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
			// nested a few hundred levels, or hundreds of unclosed <b>. Serve
			// the sanitized body as the renderer did before balancing
			// existed. The shell's own closers still shut what it leaves
			// open; a stray closer in such a document goes unbalanced.
			return sanitized, -1
		}
		if fragmentWellNested(s, nodes) {
			return s, pass
		}
		if pass == maxBalancePasses {
			break
		}
		root := fragmentContext()
		for _, n := range nodes {
			root.AppendChild(n)
		}
		hoistVoidChildren(root)
		var buf bytes.Buffer
		for n := root.FirstChild; n != nil; n = n.NextSibling {
			if err := html.Render(&buf, n); err != nil {
				return html.EscapeString(sanitized), -1
			}
		}
		s = string(sanitizer.SanitizeBytes(buf.Bytes()))
	}
	// The rebuild converges well inside maxBalancePasses, and hoistVoidChildren
	// leaves nothing html.Render refuses; if either ever does not hold, show
	// the markup as text rather than let it reach the shell unbalanced.
	return html.EscapeString(sanitized), -1
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
