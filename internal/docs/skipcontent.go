package docs

import (
	"bytes"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// skipContentElements mirrors the sanitizer's skip-content set: bluemonday
// v1.0.27's defaults (addDefaultSkipElementContent) plus the SVG containers
// allowInlineSVG adds. An element in this set is dropped WITH its contents,
// which is what keeps a denied SVG container's children from being hoisted
// into live HTML (see allowInlineSVG). The cost is that an UNCLOSED one drops
// the rest of the document (forgectl#622).
//
// Each entry is the display spelling the banner may show. The tokenizer
// lowercases tag names, so matching is on strings.ToLower of the entry.
// TestSkipContentElements_MatchPolicy pins this slice to the live policy's
// set, so a bluemonday bump or an allowInlineSVG edit that changes the set
// fails the build instead of silently blinding the detector.
var skipContentElements = []string{
	// bluemonday v1.0.27 defaults.
	"frame", "frameset", "iframe", "noembed", "noframes", "noscript",
	"nostyle", "object", "script", "style", "title",
	// allowInlineSVG additions ("title" is shared with the defaults).
	"foreignObject", "use", "image", "animate", "animateTransform",
	"animateMotion", "set", "filter", "pattern", "desc",
}

// skipContentByName maps a tokenizer (lowercased) tag name to its display
// spelling in skipContentElements.
var skipContentByName = func() map[string]string {
	m := make(map[string]string, len(skipContentElements))
	for _, name := range skipContentElements {
		m[strings.ToLower(name)] = name
	}
	return m
}()

// unclosedSkipContent reports which skip-content element swallowed the end
// of a document, if one did: it returns that element's display name (always
// an entry of skipContentElements, never author bytes) and true when the
// sanitizer, fed exactly this input, is still discarding content at end of
// input and has discarded some visible content on the way there.
//
// It replays bluemonday v1.0.27's own state machine (Policy.sanitize) over
// the same golang.org/x/net/html tokenizer, rather than looking for an
// opener with no closer, because the two disagree:
//
//   - <script> and <style> never touch the skip counter. Their start and end
//     tags short-circuit before it; what hides an unclosed one is the
//     tokenizer's raw-text mode, which runs its text to end of input, and
//     the sanitizer drops text that follows a script or style start tag.
//   - The counter decrements on ANY closer in the set, matched or not. A
//     stray </object> takes it to -1, so a later <object></object> leaves it
//     at -1 with skipping switched on for good. A structural matcher would
//     call that document balanced while every byte after it vanishes.
//
// The replay relies on no skip-content name being an allowed element and on
// the policy registering no element regexps; the parity test asserts both.
// Detection is read-only: the sanitizer's input and output are unchanged.
func unclosedSkipContent(sanitizerInput []byte) (string, bool) {
	var (
		skipping bool   // bluemonday's skipElementContent
		count    int64  // bluemonday's skippingElementsCount
		skipName string // the opener that switched skipping on
		skipLost bool   // visible content dropped since skipping began
		rawName  string // an open script/style whose raw text is dropped
		rawLost  bool   // non-blank raw text dropped since rawName opened
	)
	z := html.NewTokenizer(bytes.NewReader(sanitizerInput))
	for {
		if z.Next() == html.ErrorToken {
			if z.Err() != io.EOF {
				// The sanitizer stops at the same error; stay quiet rather
				// than guess what it emitted.
				return "", false
			}
			break
		}
		tok := z.Token()
		switch tok.Type {
		case html.StartTagToken:
			if tok.Data == "script" || tok.Data == "style" {
				if !skipping {
					rawName, rawLost = skipContentByName[tok.Data], false
				} else {
					skipLost = true
				}
				continue
			}
			if name, ok := skipContentByName[tok.Data]; ok {
				if !skipping {
					skipName, skipLost = name, false
				}
				skipping = true
				count++
				continue
			}
			if skipping {
				skipLost = true
			}
		case html.SelfClosingTagToken:
			if skipping {
				skipLost = true
			}
		case html.EndTagToken:
			if tok.Data == "script" || tok.Data == "style" {
				if skipContentByName[tok.Data] == rawName {
					rawName = ""
				}
				continue
			}
			if _, ok := skipContentByName[tok.Data]; ok {
				count--
				if count == 0 {
					skipping = false
				}
			}
		case html.TextToken:
			if strings.TrimSpace(tok.Data) == "" {
				continue
			}
			if skipping {
				skipLost = true
			} else if rawName != "" {
				rawLost = true
			}
		}
	}
	switch {
	case skipping && skipLost:
		return skipName, true
	case rawName != "" && rawLost:
		return rawName, true
	}
	return "", false
}

// skipContentBanner is the notice prepended above a body whose tail the
// sanitizer dropped. It is a fixed template: the one varying part is name,
// which the caller takes from skipContentElements (so it is never document
// bytes), and it is HTML-escaped here anyway. The data attribute makes the
// banner unforgeable by content, because the sanitizer never admits data-*
// attributes from a document.
func skipContentBanner(name string) string {
	return `<blockquote class="callout warning" role="note" data-forgectl-notice="skip-content">` +
		`<div class="callout-title"><svg viewBox="0 0 24 24" aria-hidden="true">` + calloutTriangleIcon + `</svg> Part of this document is hidden</div>` +
		`<p>An unclosed <code>&lt;` + html.EscapeString(name) + `&gt;</code> tag hides everything after it. ` +
		`Close the tag in the source to show the rest of the document.</p></blockquote>`
}
