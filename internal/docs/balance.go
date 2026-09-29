package docs

import (
	"bytes"
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
// browser's own: html.ParseFragment runs the HTML5 tree builder in a <div>
// context, and the fragment passes when the tree it builds is exactly the
// tags the source wrote, in the same order — every start tag its own
// element, every end tag closing the element it names, nothing dropped,
// inserted, implied or moved (fragmentWellNested). Such a fragment parses to
// the same tree inside the shell: every end tag finds its element within the
// fragment, so the scope search never climbs into the shell's
// div/main/div ancestors, none of which a start tag implicitly closes.
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
// for input returned unchanged, -1 for the escaped fallback.
func balancePasses(sanitized string) (string, int) {
	s := sanitized
	for pass := range maxBalancePasses + 1 {
		nodes, err := html.ParseFragment(strings.NewReader(s), fragmentContext())
		if err != nil {
			break
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
	// A strings.Reader cannot fail, and the rebuild converges well inside
	// maxBalancePasses; if either ever does not hold, show the markup as text
	// rather than let it reach the shell unbalanced.
	return html.EscapeString(sanitized), -1
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

// fragmentContext is the element the body is parsed inside: the shell's
// .doc-body <div>.
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
				// Unreachable for a strings.Reader; an unmatchable stream
				// sends the fragment down the rebuild path.
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
