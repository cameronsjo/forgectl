// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package deskweb

import (
	"bytes"
	"html/template"

	"github.com/microcosm-cc/bluemonday"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// markdown renders ask steps. Raw HTML in the source is NOT passed through:
// goldmark's default renderer (no html.WithUnsafe) replaces every raw HTML
// block and inline tag with a comment, so an agent's <script> or <img
// onerror> never becomes markup. The sanitizer below is the second layer.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Linkify, extension.Table),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(linkHosts{}, 100))),
)

// linkHosts appends " (host)" as plain text after every markdown link whose
// label is not its own URL, so an agent cannot label one site as another on
// a phone, where there is no hover to show the address.
type linkHosts struct{}

func (linkHosts) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var links []*ast.Link
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			links = append(links, l)
		}
		return ast.WalkContinue, nil
	})
	src := reader.Source()
	for _, l := range links {
		if string(linkText(l, src)) == string(l.Destination) {
			continue
		}
		host := ask.DisplayHost(string(l.Destination))
		if host == "" {
			// Not a link the page will show (the sanitizer drops it), or a
			// host with no safe display form: say so beside the label.
			host = "unsafe address"
		}
		l.Parent().InsertAfter(l.Parent(), l, ast.NewString([]byte(" ("+host+")")))
	}
}

// linkText is the plain text of a link's label.
func linkText(n ast.Node, src []byte) []byte {
	var out []byte
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			out = append(out, t.Segment.Value(src)...)
			continue
		}
		out = append(out, linkText(c, src)...)
	}
	return out
}

// sanitizer is a closed allowlist, built from nothing rather than narrowed
// from UGCPolicy: text formatting, lists, code, quotes, tables, and links to
// absolute http(s) URLs only. No images (a remote image is a tracking pixel
// an agent could plant), no relative URLs, no ids or classes, no style.
var sanitizer = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "strong", "em", "b", "i", "del", "s", "code", "pre",
		"blockquote", "ul", "ol", "li", "h1", "h2", "h3", "h4", "h5", "h6", "hr",
		"table", "thead", "tbody", "tr", "th", "td")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https")
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(false)
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

// renderSteps turns untrusted markdown into inert HTML.
func renderSteps(src string) template.HTML {
	if src == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(src), &buf); err != nil {
		// Fall back to the escaped source: never the raw bytes.
		return template.HTML("<pre>" + template.HTMLEscapeString(src) + "</pre>") //nolint:gosec // G203: escaped above
	}
	return template.HTML(sanitizer.SanitizeBytes(buf.Bytes())) //nolint:gosec // G203: sanitized by a closed allowlist
}
