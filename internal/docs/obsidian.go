package docs

import (
	"bytes"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// obsidianFlavor is the vault-root-only markdown dialect: ==highlight==,
// %%comment%% (inline and block), and #tag chips. It is registered only on
// the vault goldmark instances (newMarkdown's vault flag), so a docs root keeps
// rendering plain GFM byte for byte.
//
// None of the renderers below call html.RenderAttributes, so no
// author-controlled attribute can ride along on the new elements, and their
// output still passes the bluemonday sanitizer like every other render.
type obsidianFlavor struct{}

// Extend implements goldmark.Extender.
func (obsidianFlavor) Extend(m goldmark.Markdown) {
	obsidianComments{}.Extend(m)
	m.Parser().AddOptions(
		parser.WithInlineParsers(
			util.Prioritized(highlightParser{}, 500),
			util.Prioritized(tagParser{}, 500),
		),
	)
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(obsidianRenderer{}, 500),
	))
}

// obsidianComments registers only the %% comment parsers: the part of the
// flavour that changes what a document CONTAINS, not how it looks. The render
// instances get it through obsidianFlavor, and linkscan's vault instance gets
// it directly, so the index (title, headings, links, block ids) sees exactly
// the text the page shows.
type obsidianComments struct{}

// Extend implements goldmark.Extender.
func (obsidianComments) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithBlockParsers(
			// Ahead of the paragraph parser (1000), behind nothing else that
			// triggers on '%' — no GFM block does.
			util.Prioritized(commentBlockParser{}, 500),
		),
		parser.WithInlineParsers(
			util.Prioritized(commentInlineParser{}, 500),
		),
	)
}

var (
	kindHighlight    = ast.NewNodeKind("ObsidianHighlight")
	kindCommentSpan  = ast.NewNodeKind("ObsidianCommentSpan")
	kindCommentBlock = ast.NewNodeKind("ObsidianCommentBlock")
	kindTag          = ast.NewNodeKind("ObsidianTag")
)

// commentMarker is Obsidian's comment delimiter, for both forms.
var commentMarker = []byte("%%")

// ── ==highlight== ──────────────────────────────────────────────────────────

type highlightNode struct{ ast.BaseInline }

func (n *highlightNode) Kind() ast.NodeKind { return kindHighlight }

func (n *highlightNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// highlightDelimiters is the delimiter processor for "==". It is the shape of
// goldmark's own strikethrough extension, so CommonMark's flanking rules come
// for free: "a == b == c" stays literal because a "==" with whitespace on both
// sides can neither open nor close.
type highlightDelimiters struct{}

func (highlightDelimiters) IsDelimiter(b byte) bool { return b == '=' }

func (highlightDelimiters) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char && opener.Length >= 2 && closer.Length >= 2
}

func (highlightDelimiters) OnMatch(int) ast.Node { return &highlightNode{} }

type highlightParser struct{}

func (highlightParser) Trigger() []byte { return []byte{'='} }

func (highlightParser) Parse(_ ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	// A run the previous call declined must not be re-scanned from its
	// second '='. Checked BEFORE ScanDelimiter: scanning first would walk the
	// rest of the run at every '=' in it, which is quadratic in its length.
	if before == '=' {
		return nil
	}
	line, segment := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 2, highlightDelimiters{})
	// Exactly two: a run of three or more ("===") is not a highlight marker.
	if node == nil || node.OriginalLength != 2 {
		return nil
	}
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	pc.PushDelimiter(node)
	return node
}

func (highlightParser) CloseBlock(ast.Node, parser.Context) {}

// ── %%comment%% (inline) ───────────────────────────────────────────────────

type commentSpanNode struct{ ast.BaseInline }

// commentSpanEnd reports the length of the "%%…%%" span s opens (s starts
// with "%%"), or false when there is none to hide. It walks the text after
// the opener code-span-aware: a backtick run with a same-length closing run
// later on the line is a code span and is skipped whole, so a "%%" inside it
// is never the closer; a backtick run with no closing run makes the boundary
// unknowable, and the span is declined (keep-when-unsure). The closer is the
// first "%%" outside any code span. The inline parser, the one-line block
// opener and stripCommentSpans all decide with this one helper, so the three
// cannot disagree about where a comment ends.
func commentSpanEnd(s []byte) (int, bool) {
	for i := len(commentMarker); i < len(s); {
		if s[i] == '`' {
			run := 1
			for i+run < len(s) && s[i+run] == '`' {
				run++
			}
			closeAt := codeSpanClose(s[i+run:], run)
			if closeAt == 0 {
				return 0, false
			}
			i += run + closeAt
			continue
		}
		if bytes.HasPrefix(s[i:], commentMarker) {
			return i + len(commentMarker), true
		}
		i++
	}
	return 0, false
}

// stripCommentSpans returns line without the "%%…%%" spans the inline
// parser would hide in it. It walks the line the way goldmark's inline pass
// does for the constructs that matter here: a backslash escapes the next
// byte, and a code span (a backtick run closed by a run of the same length)
// is skipped whole, so a "%%" inside code is kept, as it is on the page. It
// serves the heading-id generator and the vault title, which read the raw
// source line rather than the parsed inline nodes.
func stripCommentSpans(line []byte) []byte {
	var out []byte
	stripped := false
	last := 0
	for i := 0; i < len(line); {
		switch line[i] {
		case '\\':
			i += 2
			continue
		case '`':
			run := 1
			for i+run < len(line) && line[i+run] == '`' {
				run++
			}
			i += run + codeSpanClose(line[i+run:], run)
			continue
		case '%':
			if bytes.HasPrefix(line[i:], commentMarker) {
				if n, ok := commentSpanEnd(line[i:]); ok {
					out = append(out, line[last:i]...)
					i += n
					last = i
					stripped = true
					continue
				}
			}
		}
		i++
	}
	if !stripped {
		return line
	}
	return append(out, line[last:]...)
}

// codeSpanClose returns how far past rest the code span closes: the offset
// just after the first backtick run in rest of exactly length run, or 0 when
// there is none (the opening run is then literal text).
func codeSpanClose(rest []byte, run int) int {
	for j := 0; j < len(rest); {
		if rest[j] != '`' {
			j++
			continue
		}
		k := j
		for k < len(rest) && rest[k] == '`' {
			k++
		}
		if k-j == run {
			return k
		}
		j = k
	}
	return 0
}

// commentStrippingIDs is the vault heading-id generator: goldmark's own,
// fed the heading line with its comment spans removed. goldmark builds an
// id from the raw source line, not the parsed inline nodes, so without this
// "## Plan %%secret%%" would get the id plan-secret, and the comment text
// would reach the page's anchors and outline hrefs. Render and scan both use
// it (newVaultIDs), which is what keeps an indexed slug equal to the id the
// page renders.
type commentStrippingIDs struct{ inner parser.IDs }

// newVaultIDs returns a fresh id collection for one vault parse. goldmark's
// default collection type is unexported, so it is taken from a new Context.
func newVaultIDs() parser.IDs {
	return commentStrippingIDs{inner: parser.NewContext().IDs()}
}

func (c commentStrippingIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	return c.inner.Generate(stripCommentSpans(value), kind)
}

func (c commentStrippingIDs) Put(value []byte) { c.inner.Put(value) }

func (n *commentSpanNode) Kind() ast.NodeKind { return kindCommentSpan }

func (n *commentSpanNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// commentInlineParser hides "%%…%%" when BOTH markers sit on the same line
// and commentSpanEnd can bound the span. Otherwise it declines, and
// the text renders literally: a comment is hidden only once its boundary is
// located, never on a guess.
type commentInlineParser struct{}

func (commentInlineParser) Trigger() []byte { return []byte{'%'} }

func (commentInlineParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if !bytes.HasPrefix(line, commentMarker) {
		return nil
	}
	n, ok := commentSpanEnd(line)
	if !ok {
		return nil
	}
	block.Advance(n)
	return &commentSpanNode{}
}

// ── %%comment%% (block) ────────────────────────────────────────────────────

// commentBlockNode is a hidden block. Its Lines hold every source line it
// consumed, opener and closer included, which is how linkscan masks them
// from the block-id scan; the renderer never reads them.
type commentBlockNode struct {
	ast.BaseBlock
	// oneLine marks a block whose opener line also carried its closer; the
	// next Continue closes it without consuming anything.
	oneLine bool
}

func (n *commentBlockNode) Kind() ast.NodeKind { return kindCommentBlock }

// IsRaw keeps goldmark's inline pass off the consumed lines: they are held
// only for masking, and inline-parsing them would put the comment's links
// back into the tree.
func (n *commentBlockNode) IsRaw() bool { return true }

func (n *commentBlockNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// commentCloser is the ONE predicate both the opener's look-ahead and
// Continue use to find a block comment's end, so the two cannot disagree
// about where it is. found reports a "%%" anywhere on the line; clean reports
// that nothing but whitespace follows it.
func commentCloser(line []byte) (found, clean bool) {
	i := bytes.Index(line, commentMarker)
	if i < 0 {
		return false, false
	}
	return true, util.IsBlank(line[i+len(commentMarker):])
}

// commentBlockParser hides a "%%" block. The invariant is keep-when-unsure:
// an unterminated "%%" must never hide the rest of the document, so Open
// only commits after it has located the closing line.
//
//   - A one-line block ("%%note%%" alone on its line) closes immediately. If
//     text follows the closer, it is declined and left to the paragraph and
//     the inline comment parser, which hides only the delimited part.
//   - A multi-line block opens only at the top level of the document, where
//     the lines Continue will see are exactly the source lines the look-ahead
//     scans. Inside a blockquote or list item the container can end before
//     the closer does, so there the "%%" stays literal.
//   - The look-ahead takes the first line containing "%%". If text follows
//     the "%%" on that line it declines, rather than hide text Obsidian would
//     show.
//   - The look-ahead also declines when a line before the closer opens a
//     fence (``` or ~~~) or an HTML block (a line starting with "<"). The
//     "%%" it would close on may sit inside that fence or block; closing
//     there would orphan the fence's own closer, which then opens a new
//     fence that swallows the rest of the document into a code block.
//   - A one-line block ends where the inline form would (commentSpanEnd):
//     at the first "%%" outside a code span, declining on an unclosed
//     backtick run.
type commentBlockParser struct{}

func (commentBlockParser) Trigger() []byte { return []byte{'%'} }

func (commentBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !bytes.HasPrefix(line[pos:], commentMarker) {
		return nil, parser.NoChildren
	}
	rest := line[pos+len(commentMarker):]
	if found, _ := commentCloser(rest); found {
		if n, ok := commentSpanEnd(line[pos:]); !ok || !util.IsBlank(line[pos+n:]) {
			return nil, parser.NoChildren
		}
		node := &commentBlockNode{oneLine: true}
		node.Lines().Append(segment)
		reader.Advance(segment.Len() - 1)
		return node, parser.NoChildren
	}
	if parent.Kind() != ast.KindDocument || !hasCleanCommentCloser(reader.Source(), segment.Stop) {
		return nil, parser.NoChildren
	}
	node := &commentBlockNode{}
	node.Lines().Append(segment)
	reader.Advance(segment.Len() - 1)
	return node, parser.NoChildren
}

// hasCleanCommentCloser scans source from offset line by line and reports
// whether the first line containing "%%" is a clean closer, with no fence or
// HTML block opening on any line before it (opensFenceOrHTML).
func hasCleanCommentCloser(source []byte, offset int) bool {
	for offset < len(source) {
		end := bytes.IndexByte(source[offset:], '\n')
		next := len(source)
		if end >= 0 {
			next = offset + end + 1
		}
		line := source[offset:next]
		if opensFenceOrHTML(line) {
			return false
		}
		if found, clean := commentCloser(line); found {
			return clean
		}
		offset = next
	}
	return false
}

// opensFenceOrHTML reports whether line, after at most three spaces of
// indent, starts a code fence (``` or ~~~) or an HTML block ("<").
func opensFenceOrHTML(line []byte) bool {
	i := 0
	for i < 3 && i < len(line) && line[i] == ' ' {
		i++
	}
	rest := line[i:]
	return bytes.HasPrefix(rest, []byte("```")) || bytes.HasPrefix(rest, []byte("~~~")) || bytes.HasPrefix(rest, []byte("<"))
}

func (commentBlockParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	if n, ok := node.(*commentBlockNode); ok && n.oneLine {
		return parser.Close
	}
	line, segment := reader.PeekLine()
	found, _ := commentCloser(line)
	node.Lines().Append(segment)
	reader.AdvanceToEOL()
	if found {
		return parser.Close
	}
	return parser.Continue | parser.NoChildren
}

func (commentBlockParser) Close(ast.Node, text.Reader, parser.Context) {}

func (commentBlockParser) CanInterruptParagraph() bool { return false }

func (commentBlockParser) CanAcceptIndentedLine() bool { return false }

// ── #tag ───────────────────────────────────────────────────────────────────

type tagNode struct {
	ast.BaseInline
	Name []byte
}

func (n *tagNode) Kind() ast.NodeKind { return kindTag }

func (n *tagNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Name": string(n.Name)}, nil)
}

// tagParser turns "#name" into a tag chip, following Obsidian's rules: the
// '#' starts a line or follows whitespace (so "C#" and "a#b" stay text), the
// name is letters, digits, '_', '-' and '/', and it holds at least one
// non-digit (so "#123" stays text) and at least one letter or digit (so
// "#-" and "#/" stay text). Code spans, code blocks, raw HTML,
// headings and frontmatter never reach an inline parser.
type tagParser struct{}

func (tagParser) Trigger() []byte { return []byte{'#'} }

func (tagParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	if !util.IsSpaceRune(block.PrecendingCharacter()) {
		return nil
	}
	line, _ := block.PeekLine()
	n := 1
	nonDigit, letterOrDigit := false, false
	for n < len(line) {
		r, size := utf8.DecodeRune(line[n:])
		if !isTagRune(r) {
			break
		}
		if !unicode.IsDigit(r) {
			nonDigit = true
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letterOrDigit = true
		}
		n += size
	}
	// A name of only punctuation ("#-", "#/", "#_") is not a tag.
	if !nonDigit || !letterOrDigit {
		return nil
	}
	name := append([]byte(nil), line[1:n]...)
	block.Advance(n)
	return &tagNode{Name: name}
}

func isTagRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '/'
}

// ── rendering ──────────────────────────────────────────────────────────────

type obsidianRenderer struct{}

func (obsidianRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindHighlight, renderHighlight)
	reg.Register(kindCommentSpan, renderNothing)
	reg.Register(kindCommentBlock, renderNothing)
	reg.Register(kindTag, renderTag)
}

func renderHighlight(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString("<mark>")
	} else {
		_, _ = w.WriteString("</mark>")
	}
	return ast.WalkContinue, nil
}

// renderNothing is a comment's whole rendering: its content never reaches
// the page, and SkipChildren keeps any child from rendering either.
func renderNothing(util.BufWriter, []byte, ast.Node, bool) (ast.WalkStatus, error) {
	return ast.WalkSkipChildren, nil
}

func renderTag(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	t, ok := n.(*tagNode)
	if !ok {
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(`<span class="tag">#`)
	_, _ = w.Write(util.EscapeHTML(t.Name))
	_, _ = w.WriteString(`</span>`)
	return ast.WalkSkipChildren, nil
}
