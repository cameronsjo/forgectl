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
	m.Parser().AddOptions(
		parser.WithBlockParsers(
			// Ahead of the paragraph parser (1000), behind nothing else that
			// triggers on '%' — no GFM block does.
			util.Prioritized(commentBlockParser{}, 500),
		),
		parser.WithInlineParsers(
			util.Prioritized(highlightParser{}, 500),
			util.Prioritized(commentInlineParser{}, 500),
			util.Prioritized(tagParser{}, 500),
		),
	)
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(obsidianRenderer{}, 500),
	))
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
	line, segment := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 2, highlightDelimiters{})
	// Exactly two: a run of three or more ("===") is not a highlight marker,
	// and a run the previous call declined must not be re-scanned from its
	// second '=' (before == '=').
	if node == nil || node.OriginalLength != 2 || before == '=' {
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

func (n *commentSpanNode) Kind() ast.NodeKind { return kindCommentSpan }

func (n *commentSpanNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// commentInlineParser hides "%%…%%" when BOTH markers sit on the same line.
// With no closer on that line it declines, and the text renders literally:
// a comment is hidden only once its boundary is located, never on a guess.
type commentInlineParser struct{}

func (commentInlineParser) Trigger() []byte { return []byte{'%'} }

func (commentInlineParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if !bytes.HasPrefix(line, commentMarker) {
		return nil
	}
	end := bytes.Index(line[len(commentMarker):], commentMarker)
	if end < 0 {
		return nil
	}
	block.Advance(len(commentMarker) + end + len(commentMarker))
	return &commentSpanNode{}
}

// ── %%comment%% (block) ────────────────────────────────────────────────────

type commentBlockNode struct {
	ast.BaseBlock
	// oneLine marks a block whose opener line also carried its closer; the
	// next Continue closes it without consuming anything.
	oneLine bool
}

func (n *commentBlockNode) Kind() ast.NodeKind { return kindCommentBlock }

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
type commentBlockParser struct{}

func (commentBlockParser) Trigger() []byte { return []byte{'%'} }

func (commentBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !bytes.HasPrefix(line[pos:], commentMarker) {
		return nil, parser.NoChildren
	}
	rest := line[pos+len(commentMarker):]
	if found, clean := commentCloser(rest); found {
		if !clean {
			return nil, parser.NoChildren
		}
		reader.Advance(segment.Len() - 1)
		return &commentBlockNode{oneLine: true}, parser.NoChildren
	}
	if parent.Kind() != ast.KindDocument || !hasCleanCommentCloser(reader.Source(), segment.Stop) {
		return nil, parser.NoChildren
	}
	reader.Advance(segment.Len() - 1)
	return &commentBlockNode{}, parser.NoChildren
}

// hasCleanCommentCloser scans source from offset line by line and reports
// whether the first line containing "%%" is a clean closer.
func hasCleanCommentCloser(source []byte, offset int) bool {
	for offset < len(source) {
		end := bytes.IndexByte(source[offset:], '\n')
		next := len(source)
		if end >= 0 {
			next = offset + end + 1
		}
		if found, clean := commentCloser(source[offset:next]); found {
			return clean
		}
		offset = next
	}
	return false
}

func (commentBlockParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	if n, ok := node.(*commentBlockNode); ok && n.oneLine {
		return parser.Close
	}
	line, _ := reader.PeekLine()
	found, _ := commentCloser(line)
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
// non-digit (so "#123" stays text). Code spans, code blocks, raw HTML,
// headings and frontmatter never reach an inline parser.
type tagParser struct{}

func (tagParser) Trigger() []byte { return []byte{'#'} }

func (tagParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	if !util.IsSpaceRune(block.PrecendingCharacter()) {
		return nil
	}
	line, _ := block.PeekLine()
	n := 1
	nonDigit := false
	for n < len(line) {
		r, size := utf8.DecodeRune(line[n:])
		if !isTagRune(r) {
			break
		}
		if !unicode.IsDigit(r) {
			nonDigit = true
		}
		n += size
	}
	if n == 1 || !nonDigit {
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
