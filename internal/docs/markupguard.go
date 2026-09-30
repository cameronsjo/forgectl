package docs

import (
	"bytes"
	"html"
	"sync"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// The markup guard (forgectl#628, forgectl#596). goldmark's inline pass
// costs about the number of inline delimiters in a block times the length
// of the block, or of the unbroken word they sit in: link openers ("[x]("
// repeated), emphasis runs ("a*", "_a " then "a* "), strikethrough
// ("a~~"), code spans, raw-HTML openers ("<a"), GFM linkify on long words
// ("a_"), and, in a vault, the wikilink, highlight, comment and math
// parsers ("[a]", "a==", "a%%", "$a"). Its block pass costs about the
// square of the containers each line reaches ("> > > …", "1. 1. 1. …", or
// deep indentation under many open list items). Measured on
// origin/main at 100 KB of one run, these take 1 to 10 s to render and as
// long again to index; 200 KB of "_a " then "a* " took 37 s, and 4,000
// "[x](" spread through one 1 MB word took 17 s. None of them can be fixed
// here short of forking goldmark, and a render holds renderMu the whole
// time, so one such document stalls every other page.
//
// markupTooComplex bounds that work from the input side, in linear time,
// before goldmark runs. A document over either bound below is not parsed:
// the reader shows it as plain text under a notice (plainTextDoc), and the
// index lists it by title only, as it does a document over maxScanBytes.

// maxContainerWork bounds the block pass: the sum, over every line, of the
// square of the containers the line can reach (containerWorkOver). goldmark's
// block pass costs about that per line, at about 10 ns a unit: 100 KB of
// "> " on one line takes about 3 s, and 262 lines each indented 4,000
// spaces under 2,000 nested list items took 11 s. The bound holds the
// block pass to about 0.2 s. A written document is far under it: 300
// list items each nested one deeper than the last come to 9M.
const maxContainerWork = 1 << 24

// maxInlineWork bounds the inline pass: the sum, over every block goldmark
// inline-parses, of the delimiter bytes (markupDelimiters) the block holds
// times its length in bytes. Measured at the bound, the worst trigger for
// each delimiter renders in at most about 1 s ("[x](" through one long
// word, "<a" through one long paragraph), and most in under 0.3 s. A
// written document is far under it: the largest of 2,166 markdown files
// sampled, a 338 KB changelog of long, backtick-heavy list items, came to
// 12M. A table counts as one block, as a paragraph of its rows.
const maxInlineWork = 1 << 27

// markupDelimiters are the bytes whose count drives goldmark's superlinear
// inline cases: emphasis and strikethrough delimiters, code-span
// backticks, link, image and wikilink openers, autolink and raw-HTML
// openers, and the vault highlight, comment and math delimiters. Counting
// the vault bytes in a docs root too only ever counts more.
const markupDelimiters = "*_~`[!<=%$"

// isMarkupDelimiter is markupDelimiters as a lookup table.
var isMarkupDelimiter = func() (t [256]bool) {
	for i := 0; i < len(markupDelimiters); i++ {
		t[markupDelimiters[i]] = true
	}
	return t
}()

// markupGuardParser is goldmark's block pass with its default block
// parsers and nothing else: no inline parsers, which keeps it linear once
// the containers are bounded, and no paragraph transformers. Its blocks are
// therefore never finer than any pipeline's, so it never counts less:
//
//   - A paragraph transformer only removes lines from a paragraph (link
//     reference definitions) or splits it (a GFM table into cells). Which
//     one applies differs by pipeline: the docs index parser has no table
//     transformer, so it inline-parses a table as one paragraph, and in
//     the others a link reference definition stripped from a paragraph's
//     head can leave a delimiter row with no header, which is then no
//     table. With neither, a table here is always one paragraph of its
//     rows.
//   - Every block parser the pipelines add ($$ math, %% comments,
//     frontmatter) opens a raw block that is not inline-parsed, and none
//     can interrupt a paragraph, so each only takes lines out of the
//     paragraphs counted here.
//
// TestMarkupGuard_NeverFinerThanAPipeline checks that against every
// pipeline's own parser. It is used under its own lock, as
// fragmentMarkdown is.
var (
	markupGuardMu     sync.Mutex
	markupGuardParser = parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...))
)

// markupTooComplex reports whether source is over maxContainerWork or
// maxInlineWork, and so must not be handed to goldmark.
func markupTooComplex(source []byte) bool {
	// The container bound first: the guard's own block pass is goldmark's,
	// and costs what the render's does.
	if containerWorkOver(source) {
		return true
	}
	// Every block's delimiters times its length is at most the document's
	// delimiters times its length, so a document under the bound on those
	// totals needs no block pass at all. Most documents stop here.
	if countDelimiters(source)*len(source) <= maxInlineWork {
		return false
	}
	markupGuardMu.Lock()
	doc := markupGuardParser.Parse(text.NewReader(source))
	markupGuardMu.Unlock()
	return inlineWork(doc, source, maxInlineWork) > maxInlineWork
}

// inlineWork is the sum, over the blocks of doc that goldmark inline-parses
// (every non-raw block with lines), of the delimiter bytes in the block
// times its length. It stops counting once the sum passes limit.
func inlineWork(doc ast.Node, source []byte, limit int) int {
	work := 0
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || n.Type() != ast.TypeBlock || n.IsRaw() {
			return ast.WalkContinue, nil
		}
		lines := n.Lines()
		k, size := 0, 0
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			v := seg.Value(source)
			k += countDelimiters(v)
			size += len(v)
		}
		work += k * size
		if work > limit {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return work
}

// countDelimiters counts the markupDelimiters bytes in b.
func countDelimiters(b []byte) int {
	n := 0
	for _, c := range b {
		if isMarkupDelimiter[c] {
			n++
		}
	}
	return n
}

// containerWorkOver reports whether the sum over source's lines of their
// containers squared is over maxContainerWork.
//
// A line's containers are an upper bound on the block containers goldmark
// walks for it: the blockquote markers (">") and list markers ("-", "+",
// "*", or 1 to 9 digits and "." or ")", each followed by a space, a tab or
// the line's end) at its start, plus the list items it can continue by
// indentation. A list item needs at least two columns of indentation per
// level (a one-character marker and a space), so a line indented c
// columns (a tab reaching the next multiple of four) continues at most
// c/2 of them, and never more than are open. The open count is every list
// marker seen, reset only where every list is certainly closed: a line at
// column 0, after a blank line, that starts with no marker. A marker is
// counted whether or not goldmark opens a container there ("- - -" is a
// thematic break), and a closed list item is still counted as open, which
// only ever counts more.
func containerWorkOver(source []byte) bool {
	work, open := 0, 0
	blank := true
	for len(source) > 0 {
		line := source
		if i := bytes.IndexByte(source, '\n'); i >= 0 {
			line, source = source[:i], source[i+1:]
		} else {
			source = nil
		}
		quotes, lists, cols, i := 0, 0, 0, 0
	prefix:
		for i < len(line) {
			switch c := line[i]; {
			case c == ' ':
				cols++
				i++
			case c == '\t':
				cols += 4 - cols%4
				i++
			case c == '>':
				quotes++
				i++
			default:
				j := listMarkerEnd(line, i)
				if j == i {
					break prefix
				}
				lists++
				i = j
			}
		}
		rest := bytes.TrimSpace(line[i:])
		if len(rest) == 0 && quotes == 0 && lists == 0 {
			blank = true
			continue
		}
		if blank && cols == 0 && quotes == 0 && lists == 0 {
			open = 0
		}
		blank = false
		d := quotes + lists + min(open, cols/2)
		open += lists
		work += d * d
		if work > maxContainerWork {
			return true
		}
	}
	return false
}

// listMarkerEnd returns the index just past a list marker at line[i], or i
// when none starts there. A marker must be followed by a space, a tab, a
// carriage return or the line's end.
func listMarkerEnd(line []byte, i int) int {
	j := i
	switch {
	case line[j] == '-' || line[j] == '+' || line[j] == '*':
		j++
	case line[j] >= '0' && line[j] <= '9':
		for j < len(line) && j-i < 9 && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j == len(line) || (line[j] != '.' && line[j] != ')') {
			return i
		}
		j++
	default:
		return i
	}
	if j < len(line) && line[j] != ' ' && line[j] != '\t' && line[j] != '\r' {
		return i
	}
	return j
}

// plainTextDoc is the page body for a document markupTooComplex refuses:
// a fixed notice, then the whole source HTML-escaped in a <pre>, so the
// reader still shows every byte of it, as text.
func plainTextDoc(source []byte) string {
	return `<blockquote class="callout warning" role="note" data-forgectl-notice="plain-text">` +
		`<div class="callout-title"><svg viewBox="0 0 24 24" aria-hidden="true">` + calloutTriangleIcon + `</svg> Shown as plain text</div>` +
		`<p>This document has more nested or unclosed markup than the reader formats in reasonable time, so it is shown as its source text. ` +
		`Its links and headings are not indexed.</p></blockquote>` +
		`<pre class="doc-plain-text">` + html.EscapeString(string(source)) + `</pre>`
}
