package docs

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"go.abhg.dev/goldmark/wikilink"
)

// obsidianFlavor is the vault-root-only markdown dialect: ==highlight==,
// %%comment%% (inline and block), #tag chips, and [[wikilink]] parsing. It is
// registered only on the vault goldmark instances (newMarkdown's vault flag),
// so a docs root keeps rendering plain GFM byte for byte. linkscan's vault
// parser is one of those instances too (linkMarkdownVault), so the index and
// the page parse a vault note with the identical parser set.
//
// None of the renderers below call html.RenderAttributes, so no
// author-controlled attribute can ride along on the new elements, and their
// output still passes the bluemonday sanitizer like every other render. A
// wikilink becomes an anchor only when the render put a resolver in the
// parser context (wikilinkTransformer), and its href is then built from the
// indexed doc it resolved to, never from the link's own text.
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
			// Ahead of goldmark's link parser (200), the priority the
			// library's own Extender uses. Only the Parser is taken: the
			// library's Renderer and Extender build hrefs from the link's
			// own text. wikilinkTransformer and the renderers below
			// replace them.
			util.Prioritized(&wikilink.Parser{}, 199),
			util.Prioritized(highlightParser{}, 500),
			util.Prioritized(commentInlineParser{}, 500),
			util.Prioritized(tagParser{}, 500),
		),
		parser.WithASTTransformers(
			util.Prioritized(commentTransformer{}, 500),
			// After commentTransformer, so a heading's id is settled
			// before any wikilink in it is replaced.
			util.Prioritized(wikilinkTransformer{}, 600),
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
	kindWikilink     = ast.NewNodeKind("ObsidianWikilink")
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

// commentSpanNode is an inline comment: everything between a paired "%%"
// opener and closer, moved under it as children by goldmark's delimiter
// pass. It renders nothing and its children are skipped. Start and Stop are
// its source range, markers included, which the heading-id transformer cuts
// out of a heading line.
type commentSpanNode struct {
	ast.BaseInline
	Start, Stop int
}

func (n *commentSpanNode) Kind() ast.NodeKind { return kindCommentSpan }

func (n *commentSpanNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// commentDelimiters is the delimiter processor for one "%%" delimiter. The
// inline form is a goldmark delimiter, like ==highlight==, so goldmark's own
// inline pass settles every boundary question: a code span, autolink or
// [[wikilink]] is consumed before its '%' is ever seen, "\%" is an escape
// and never triggers, delimiters inside link text pair within the link, and
// a "%%" left unpaired at the end of the paragraph becomes literal text.
// There is no hand-written scan to disagree with the parse.
//
// Each delimiter gets its own processor so the pairing can record the
// comment's source range: goldmark asks the OPENER's processor
// CanOpenCloser for a candidate closer and, for a run of two against a run of
// two, always calls that same processor's OnMatch next.
type commentDelimiters struct {
	start int
	stop  int
}

func (*commentDelimiters) IsDelimiter(b byte) bool { return b == '%' }

func (p *commentDelimiters) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	if opener.Char != closer.Char || opener.Length < 2 || closer.Length < 2 {
		return false
	}
	p.stop = closer.Segment.Start + len(commentMarker)
	return true
}

func (p *commentDelimiters) OnMatch(int) ast.Node {
	return &commentSpanNode{Start: p.start, Stop: p.stop}
}

// commentInlineParser pushes a "%%" delimiter. Obsidian allows spaces inside
// ("%% note %%"), so every "%%" may both open and close, instead of
// following CommonMark's flanking rules.
type commentInlineParser struct{}

func (commentInlineParser) Trigger() []byte { return []byte{'%'} }

func (commentInlineParser) Parse(_ ast.Node, block text.Reader, pc parser.Context) ast.Node {
	// A run the previous call declined is not re-scanned from its second
	// '%', which keeps a long run linear, as for '='.
	if block.PrecendingCharacter() == '%' {
		return nil
	}
	line, segment := block.PeekLine()
	run := 0
	for run < len(line) && line[run] == '%' {
		run++
	}
	if run != len(commentMarker) {
		return nil
	}
	d := parser.NewDelimiter(true, true, run, '%', &commentDelimiters{start: segment.Start})
	d.Segment = segment.WithStop(segment.Start + run)
	block.Advance(run)
	pc.PushDelimiter(d)
	return d
}

func (commentInlineParser) CloseBlock(ast.Node, parser.Context) {}

// commentTransformer runs after the parse on every vault instance, render
// and scan alike:
//
//   - It gives each heading its id, the job WithAutoHeadingID does for a docs
//     root, from the same input goldmark would use (the heading's last source
//     line) with every comment's source range cut out, through the same
//     parser.IDs collection, so duplicate suffixes work as before. A heading
//     with no comment gets exactly the id goldmark would give it, and scan
//     and render agree because both run this one transformer.
//   - It drops a paragraph left holding nothing but comments, so a note line
//     that is only a comment leaves no empty <p> behind.
type commentTransformer struct{}

func (commentTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	source := reader.Source()
	var headings []*ast.Heading
	var empty []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindHeading:
			if h, ok := n.(*ast.Heading); ok {
				headings = append(headings, h)
			}
		case ast.KindParagraph:
			if onlyComments(n, source) {
				empty = append(empty, n)
			}
		}
		return ast.WalkContinue, nil
	})
	for _, h := range headings {
		var value []byte
		if last := h.Lines().Len() - 1; last >= 0 {
			value = visibleSource(h, h.Lines().At(last), source)
		}
		h.SetAttribute([]byte("id"), pc.IDs().Generate(value, ast.KindHeading))
	}
	var removed []text.Segment
	for _, n := range empty {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if cs, ok := c.(*commentSpanNode); ok {
				removed = append(removed, text.NewSegment(cs.Start, cs.Stop))
			}
		}
		n.Parent().RemoveChild(n.Parent(), n)
	}
	pc.Set(removedCommentsKey, removed)
}

// removedCommentsKey holds, in the parser context, the source range of each
// comment in a paragraph commentTransformer removed. Those comments are no
// longer in the tree, so linkscan reads them from here to keep a block-id
// marker inside one out of the index.
var removedCommentsKey = parser.NewContextKey()

// removedComments returns the ranges commentTransformer recorded in pc.
func removedComments(pc parser.Context) []text.Segment {
	if v, ok := pc.Get(removedCommentsKey).([]text.Segment); ok {
		return v
	}
	return nil
}

// onlyComments reports whether paragraph p holds at least one comment and
// otherwise only whitespace text.
func onlyComments(p ast.Node, source []byte) bool {
	found := false
	for c := p.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.Kind() {
		case kindCommentSpan:
			found = true
		case ast.KindText:
			t, ok := c.(*ast.Text)
			if !ok || !util.IsBlank(t.Segment.Value(source)) {
				return false
			}
		default:
			return false
		}
	}
	return found
}

// visibleSource returns the bytes of seg with the source range of every
// comment under n cut out. It is how a heading's id and a vault note's title
// leave comment text behind, with everything else exactly as written.
func visibleSource(n ast.Node, seg text.Segment, source []byte) []byte {
	var cuts [][2]int
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if cs, ok := c.(*commentSpanNode); ok {
				cuts = append(cuts, [2]int{cs.Start, cs.Stop})
				return ast.WalkSkipChildren, nil
			}
		}
		return ast.WalkContinue, nil
	})
	var out []byte
	at := seg.Start
	for _, cut := range cuts {
		lo, hi := max(cut[0], seg.Start), min(cut[1], seg.Stop)
		if lo >= hi {
			continue
		}
		if lo > at {
			out = append(out, source[at:lo]...)
		}
		at = max(at, hi)
	}
	if at < seg.Stop {
		out = append(out, source[at:seg.Stop]...)
	}
	return out
}

// ── %%comment%% (block) ────────────────────────────────────────────────────

// commentBlockNode is a hidden block. Its Lines hold every source line it
// consumed, opener and closer included, which is how linkscan masks them
// from the block-id scan; the renderer never reads them.
type commentBlockNode struct {
	ast.BaseBlock
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
//   - An opener line that also holds a closer ("%%note%%") is declined: it
//     is a paragraph, and the inline form pairs it (a paragraph left with
//     only comments is then dropped by commentTransformer).
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
type commentBlockParser struct{}

func (commentBlockParser) Trigger() []byte { return []byte{'%'} }

func (commentBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || !bytes.HasPrefix(line[pos:], commentMarker) {
		return nil, parser.NoChildren
	}
	// A "%%" line that also closes itself is the inline form's to pair, as
	// a paragraph; this parser takes only an opener with no closer on it.
	if found, _ := commentCloser(line[pos+len(commentMarker):]); found {
		return nil, parser.NoChildren
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

// ── [[wikilink]] resolution ────────────────────────────────────────────────

// wikilinkResolver resolves one wikilink for the page being rendered,
// returning the href to link it to ("" for none) and its verdict. The render
// stores it in the parser context under wikilinkResolverKey, as this named
// type: wikilinkTransformer's type assertion matches nothing else, and
// without it the render fails closed.
type wikilinkResolver func(LinkRef) (href string, miss Miss)

// wikilinkResolverKey holds the page's wikilinkResolver in the parser
// context. Only a vault render with an index sets it; the link scan never
// does.
var wikilinkResolverKey = parser.NewContextKey()

// The title a wikilink that did not resolve carries, one per reason. They
// are fixed text, so nothing an author writes reaches the attribute, and
// colon-free, because the sanitizer drops a title holding a colon.
const (
	titleNoTarget     = "Broken link (no-target)"
	titleAmbiguous    = "Broken link (ambiguous)"
	titleOutsideRoot  = "Broken link (outside-root)"
	titleAnchorMissed = "Broken link (heading or block not found)"
	titleUnresolved   = "Broken link (unresolved)"
)

// missTitle is the title for a wikilink that missed. docResolved is whether
// the note itself resolved, in which case only its heading or block id did
// not.
func missTitle(miss Miss, docResolved bool) string {
	switch {
	case miss == MissNoTarget && docResolved:
		return titleAnchorMissed
	case miss == MissAmbiguous:
		return titleAmbiguous
	case miss == MissOutsideRoot:
		return titleOutsideRoot
	default:
		return titleNoTarget
	}
}

// isDocHref reports whether href is a reader page path, the only kind of
// href a wikilink may carry.
func isDocHref(href string) bool {
	return strings.HasPrefix(href, "/doc/")
}

// resolvedWikilinkNode is a wikilink after resolution. Href is set only from
// the resolver and only when it is a reader page path; Title is set only for
// a miss, from missTitle. The children are the wikilink's label.
type resolvedWikilinkNode struct {
	ast.BaseInline
	Href  string
	Title string
}

func (n *resolvedWikilinkNode) Kind() ast.NodeKind { return kindWikilink }

func (n *resolvedWikilinkNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Href": n.Href, "Title": n.Title}, nil)
}

// wikilinkTransformer resolves each wikilink on the page through the
// resolver in the parser context, replacing it with a resolvedWikilinkNode.
// With no resolver it leaves the tree exactly as parsed: the link scan runs
// this same parser set, and reads its links off the wikilink nodes. It never
// touches an embed, a wikilink inside a markdown link or image, or one inside
// a comment; renderWikilinkSource renders the first two as source text, and
// a comment renders nothing.
type wikilinkTransformer struct{}

func (wikilinkTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	resolve, ok := pc.Get(wikilinkResolverKey).(wikilinkResolver)
	if !ok || resolve == nil {
		return
	}
	source := reader.Source()
	var links []*wikilink.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case kindCommentSpan, kindCommentBlock, ast.KindLink, ast.KindImage:
			return ast.WalkSkipChildren, nil
		case wikilink.Kind:
			if wl, isWikilink := n.(*wikilink.Node); isWikilink && !wl.Embed {
				links = append(links, wl)
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	for _, wl := range links {
		href, miss := resolve(wikilinkRef(wl, source))
		node := &resolvedWikilinkNode{}
		if isDocHref(href) {
			node.Href = href
		}
		if miss != MissNone || node.Href == "" {
			node.Title = missTitle(miss, node.Href != "")
		}
		for c := wl.FirstChild(); c != nil; {
			next := c.NextSibling()
			node.AppendChild(node, c)
			c = next
		}
		wl.Parent().ReplaceChild(wl.Parent(), wl, node)
	}
}

// ── rendering ──────────────────────────────────────────────────────────────

type obsidianRenderer struct{}

func (obsidianRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindHighlight, renderHighlight)
	reg.Register(kindCommentSpan, renderNothing)
	reg.Register(kindCommentBlock, renderNothing)
	reg.Register(kindTag, renderTag)
	reg.Register(wikilink.Kind, renderWikilinkSource)
	reg.Register(kindWikilink, renderResolvedWikilink)
}

// renderWikilinkSource renders a wikilink wikilinkTransformer left in the
// tree, as its own source text, "[[…]]" or "![[…]]", HTML-escaped. An embed,
// and a wikilink inside a markdown link or image, is exactly that text: an
// embed is not rendered yet, and an anchor inside a link would nest one <a>
// in another. Any other wikilink reaching here was rendered without a
// resolver, so its target was never checked: it fails closed, as its source
// text inside an unresolved miss span, and never as a link. The text runs
// from the node's start to its label's end plus the closing "]]", both set
// by the parser.
func renderWikilinkSource(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	wl, isWikilink := n.(*wikilink.Node)
	literal := !isWikilink || wl.Embed || hasLinkAncestor(n)
	start, stop, ok := wikilinkSourceRange(n, source)
	if !entering {
		// Reached after the children rendered: only a span opened with no
		// source range to write still needs closing.
		if !literal && !ok {
			_, _ = w.WriteString(`</span>`)
		}
		return ast.WalkContinue, nil
	}
	if !literal {
		writeMissSpanOpen(w, titleUnresolved)
	}
	if !ok {
		// No source range to write: render the label as text instead,
		// never nothing.
		return ast.WalkContinue, nil
	}
	_, _ = w.Write(util.EscapeHTML(source[start:stop]))
	if !literal {
		_, _ = w.WriteString(`</span>`)
	}
	return ast.WalkSkipChildren, nil
}

// wikilinkSourceRange is the source range of wikilink n, "[[" (or "![[")
// through "]]", when the parser recorded one.
func wikilinkSourceRange(n ast.Node, source []byte) (start, stop int, ok bool) {
	label, isText := n.LastChild().(*ast.Text)
	if !isText {
		return 0, 0, false
	}
	start = n.Pos()
	stop = label.Segment.Stop + len("]]")
	if start < 0 || stop > len(source) || start >= stop {
		return 0, 0, false
	}
	return start, stop, true
}

// hasLinkAncestor reports whether n sits inside a markdown link or image.
func hasLinkAncestor(n ast.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if k := p.Kind(); k == ast.KindLink || k == ast.KindImage {
			return true
		}
	}
	return false
}

// renderResolvedWikilink renders a wikilink wikilinkTransformer resolved. An
// href, which only ever comes from an indexed doc, makes an anchor: a plain
// one for a hit, a marked one for a doc whose heading or block id is missing.
// Anything else is a miss span with no href. The href is checked for the
// /doc/ prefix again here, so an href that did not come from docHref can
// never reach the page. The label renders as the node's children, which
// goldmark escapes as text.
func renderResolvedWikilink(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	rw, ok := n.(*resolvedWikilinkNode)
	if !ok {
		return ast.WalkContinue, nil
	}
	anchor := isDocHref(rw.Href)
	if !entering {
		if anchor {
			_, _ = w.WriteString(`</a>`)
		} else {
			_, _ = w.WriteString(`</span>`)
		}
		return ast.WalkContinue, nil
	}
	if !anchor {
		title := rw.Title
		if title == "" {
			title = titleNoTarget
		}
		writeMissSpanOpen(w, title)
		return ast.WalkContinue, nil
	}
	if rw.Title == "" {
		_, _ = w.WriteString(`<a class="wikilink" href="`)
	} else {
		_, _ = w.WriteString(`<a class="wikilink wikilink-miss" title="`)
		_, _ = w.Write(util.EscapeHTML([]byte(rw.Title)))
		_, _ = w.WriteString(`" href="`)
	}
	_, _ = w.Write(util.EscapeHTML([]byte(rw.Href)))
	_, _ = w.WriteString(`">`)
	return ast.WalkContinue, nil
}

// writeMissSpanOpen opens the span a wikilink with no link renders in.
func writeMissSpanOpen(w util.BufWriter, title string) {
	_, _ = w.WriteString(`<span class="wikilink wikilink-miss" title="`)
	_, _ = w.Write(util.EscapeHTML([]byte(title)))
	_, _ = w.WriteString(`">`)
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
