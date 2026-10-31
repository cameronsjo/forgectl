// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package deskweb

import (
	"bytes"
	"html/template"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// markdown renders ask steps. Raw HTML in the source is NOT passed through:
// goldmark's default renderer (no html.WithUnsafe) replaces every raw HTML
// block and inline tag with a comment, so an agent's <script> or <img
// onerror> never becomes markup. The sanitizer below is the second layer.
var markdown = goldmark.New(goldmark.WithExtensions(extension.Strikethrough, extension.Linkify, extension.Table))

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
