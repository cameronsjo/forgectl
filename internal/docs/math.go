package docs

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Math support is split in two. This file is the Go half: it finds TeX in the
// markdown ($…$, $$…$$, and ```math fences) and emits it as HTML-escaped text
// inside an element a client-side renderer can find, with the original
// delimiters kept. Without that renderer the reader shows exactly the TeX it
// showed before, except that markdown no longer mangles it (a_1 * b_2 turning
// into emphasis, \{ losing its backslash).
//
// The markup is deliberately plain, so the sanitizer needs no change: span and
// div are already allowed, and AllowStyling (render.go) lets "class" through.
const (
	mathInlineClass  = "math math-inline"
	mathDisplayClass = "math math-display"

	// mathInfo is the fence info string that marks a display-math block.
	mathInfo = "math"

	// mathDelim is the display delimiter. A ```math fence is emitted with it
	// too, so every display block reaches the client in one shape.
	mathDelim = "$$"
)

// kindMathInline is inline math: $…$, or $$…$$ inside a paragraph.
var kindMathInline = ast.NewNodeKind("MathInline")

// mathInline holds the TeX between the delimiters, copied out of the source
// because inline math may span a soft line break, and so more than one
// segment.
type mathInline struct {
	ast.BaseInline
	tex     []byte
	display bool
}

func (*mathInline) Kind() ast.NodeKind { return kindMathInline }

func (n *mathInline) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// kindMathBlock is display math that stands as its own block: a $$ block or a
// ```math fence.
var kindMathBlock = ast.NewNodeKind("MathBlock")

// mathBlock holds the TeX lines of a display block. singleLine marks the
// one-line $$…$$ form, which is emitted without the line breaks a multi-line
// block gets around its body.
type mathBlock struct {
	ast.BaseBlock
	singleLine bool
}

func (*mathBlock) Kind() ast.NodeKind { return kindMathBlock }

// IsRaw keeps goldmark's inline pass off the TeX. Without it the block's lines
// would be parsed as markdown after block parsing, which is the mangling this
// file exists to stop.
func (*mathBlock) IsRaw() bool { return true }

func (n *mathBlock) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

func isMathSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// opensMath reports whether the opener at the start of line may open math.
// $…$ needs a non-space after it. $$ may also end its line (optionally after
// spaces or tabs, the same test the block opener applies), which is how
// display math written straight after paragraph text, or inside a list item
// or blockquote, reaches this parser. A space followed by more text
// ("costs $$ 5") still does not open.
func opensMath(line []byte, delim int) bool {
	if len(line) <= delim {
		return false
	}
	if delim == 2 && util.IsBlank(line[delim:]) {
		return true
	}
	return !isMathSpace(line[delim])
}

// mathInlineParser parses $…$ and $$…$$ inside a paragraph.
//
// The dollar sign is ordinary prose far more often than it is math, so the
// rules are pandoc's tex_math_dollars, chosen because they keep currency and
// shell variables literal:
//
//   - the opening $ must be followed by a non-space (an opening $$ may also
//     end its line; see opensMath);
//   - for $…$, the FIRST unescaped $ after it decides: if it follows
//     whitespace, or is followed by an ASCII digit, there is no math here and
//     the parser gives up rather than scanning on for a later $.
//
// So "It costs $5 and $10" stays text (the second $ follows a space), and so
// does "echo $HOME and $PATH". A backslash-escaped byte is skipped while
// scanning, so \$ inside math never closes it.
//
// A candidate that crosses another inline construct's syntax is also left as
// text (see crossesMarkdown): math is found before links, code spans,
// autolinks and raw HTML, so without that check "[$HOME](a.md) and
// [$PATH](b.md)" would turn half of each link into math.
//
// singleDollar gates the $…$ form. It is on for vault roots, whose native
// dialect (Obsidian) has it, and off for docs roots, where "$" is far more
// often shell (PATH=$HOME/bin:$PATH) than math. With it off, only the $$…$$
// form is inline math, and a lone $ is text.
//
// midLineDisplay lets $$…$$ open and close anywhere in a line. It is on for
// vault roots (Obsidian's inline $$…$$) and off for docs roots, where "$$" in
// prose is far more often the shell's PID ("tmp=/tmp/x.$$; rm /tmp/y.$$")
// or currency than math (forgectl#650). With it off, $$ is math only when it
// is block-shaped: the opening $$ has nothing but whitespace before it on its
// line, and the closing $$ nothing but whitespace after it on its line. That
// still takes the display math the block parser hands down: $$ lines straight
// after paragraph text, and $$ blocks inside a list item or blockquote.
type mathInlineParser struct{ singleDollar, midLineDisplay bool }

func (mathInlineParser) Trigger() []byte { return []byte{'$'} }

func (p mathInlineParser) Parse(parent ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	delim := 1
	if len(line) > 1 && line[1] == '$' {
		delim = 2
	}
	if delim == 1 && !p.singleDollar {
		return nil
	}
	if !opensMath(line, delim) {
		return nil
	}
	blockShaped := delim == 2 && !p.midLineDisplay
	if blockShaped && !startsItsLine(parent, block) {
		return nil
	}

	// The scan may cross soft line breaks, so remember where we started and
	// put the reader back on every failure — the same save/restore goldmark's
	// own code-span parser uses.
	startLine, startPos := block.Position()
	block.Advance(delim)

	var tex []byte
	prev := line[delim-1]
	for {
		line, _ := block.PeekLine()
		if line == nil {
			block.SetPosition(startLine, startPos)
			return nil
		}
		for i := 0; i < len(line); i++ {
			c := line[i]
			if c == '\\' {
				// Skip the escaped byte. Recording the backslash as prev is
				// harmless: it is not whitespace.
				prev = c
				i++
				continue
			}
			if c != '$' {
				prev = c
				continue
			}
			if delim == 2 {
				if i+1 < len(line) && line[i+1] == '$' {
					// The first unescaped $$ is the closer; a block-shaped
					// one must end its line, or there is no math here.
					if blockShaped && !util.IsBlank(line[i+2:]) {
						block.SetPosition(startLine, startPos)
						return nil
					}
					tex = append(tex, line[:i]...)
					block.Advance(i + 2)
					return newMathInline(tex, true, block, startLine, startPos)
				}
				prev = c
				continue
			}
			if isMathSpace(prev) || (i+1 < len(line) && isASCIIDigit(line[i+1])) {
				block.SetPosition(startLine, startPos)
				return nil
			}
			tex = append(tex, line[:i]...)
			block.Advance(i + 1)
			return newMathInline(tex, false, block, startLine, startPos)
		}
		tex = append(tex, line...)
		if len(line) > 0 {
			prev = line[len(line)-1]
		}
		block.AdvanceLine()
	}
}

// startsItsLine reports whether the reader sits at the start of its line in
// parent's content, whitespace aside. The line segments are parent's own, so
// a container prefix ("> ", list indentation) is already outside them.
func startsItsLine(parent ast.Node, block text.Reader) bool {
	idx, pos := block.Position()
	lines := parent.Lines()
	if idx < 0 || idx >= lines.Len() {
		return false
	}
	start := lines.At(idx).Start
	if start > pos.Start {
		return false
	}
	return util.IsBlank(block.Source()[start:pos.Start])
}

// newMathInline builds the node, or rewinds and returns nil for empty math
// and for a candidate that crosses other markdown.
// GitHub's $`…`$ form wraps the TeX in backticks to protect it from markdown;
// the backticks are not TeX, so one is dropped from each end.
func newMathInline(tex []byte, display bool, block text.Reader, line int, pos text.Segment) ast.Node {
	if len(tex) >= 2 && tex[0] == '`' && tex[len(tex)-1] == '`' {
		tex = tex[1 : len(tex)-1]
	} else if bytes.IndexByte(tex, '`') >= 0 {
		// A backtick outside the $`…`$ form belongs to a code span.
		block.SetPosition(line, pos)
		return nil
	}
	if len(tex) == 0 || crossesMarkdown(tex) {
		block.SetPosition(line, pos)
		return nil
	}
	return &mathInline{tex: tex, display: display}
}

// crossesMarkdown reports whether a math candidate contains the syntax of a
// link ("](") or of an autolink or raw-HTML tag ("<" followed by an ASCII
// letter, "/", "!" or "?"). Either means the $ pair straddles another
// construct, and keeping the text literal is the safe reading. A spaced
// comparison such as "a < b" is not a tag opener and stays math; "a<b" does
// not, which is the price of keeping links and tags intact.
func crossesMarkdown(tex []byte) bool {
	if bytes.Contains(tex, []byte("](")) {
		return true
	}
	for i := 0; i+1 < len(tex); i++ {
		if tex[i] != '<' {
			continue
		}
		switch c := tex[i+1]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '/', c == '!', c == '?':
			return true
		}
	}
	return false
}

// mathBlockParser parses $$ display blocks. Two opener shapes:
//
//   - a line that is exactly $$ (trailing spaces allowed) opens a block that
//     runs to the next line ending in $$ — but only at document top level,
//     and only when that closing line comes before any blank line (see
//     hasDisplayCloserAhead);
//   - a line that is exactly $$…$$ is a one-line block, in any container.
//
// Anything else starting with $$ ($$x$$ followed by more text, or $$ followed
// by TeX that closes on a later line) is left to mathInlineParser, which
// keeps it inside its paragraph. That bounds a stray "$$" in prose to the
// paragraph it sits in instead of letting a block swallow the rest of the
// document.
//
// A $$ block NEVER interrupts a paragraph (CanInterruptParagraph is false):
// it opens only where a paragraph could start. $$ lines that directly follow
// paragraph text stay in that paragraph, where mathInlineParser pairs them
// across soft breaks and renders a <span class="math math-display">.
//
// Every earlier swallow bug had one root: an unterminated multi-line block
// ran to the end of its container. A stray "$$" closer is easy to produce —
// display math after text whose paragraph a blank line, heading, fence,
// thematic break or setext underline ends before the closer — and it then
// opened a block that ate everything to the next $$ or EOF. So the block is
// keep-when-unsure, like the %% comment blocks: it opens only when it can see
// its own closer before any blank line (a blank line is not valid inside
// display math anyway), and so it never crosses a blank line or runs to EOF.
// A stray closer followed by a blank line stays literal text.
//
// The look-ahead reads raw source lines, which is only sound where no
// container prefix ("> ", list indentation) can hide a boundary — so the
// multi-line form opens only at document top level. Inside a list item or
// blockquote, "$$\nx\n$$" becomes a paragraph and the inline parser renders
// it as a display span.
type mathBlockParser struct{}

func (mathBlockParser) Trigger() []byte { return []byte{'$'} }

func (mathBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || pos+len(mathDelim) > len(line) || !bytes.HasPrefix(line[pos:], []byte(mathDelim)) {
		return nil, parser.NoChildren
	}
	rest := line[pos+len(mathDelim):]

	if util.IsBlank(rest) {
		if parent.Kind() != ast.KindDocument || !hasDisplayCloserAhead(reader.Source(), segment.Start) {
			return nil, parser.NoChildren
		}
		reader.AdvanceToEOL()
		return &mathBlock{}, parser.NoChildren
	}

	closer := findDisplayCloser(rest)
	if closer <= 0 || !util.IsBlank(rest[closer+len(mathDelim):]) {
		return nil, parser.NoChildren
	}
	// line carries segment.Padding expanded-tab columns ahead of the source
	// bytes, so line index k is source offset segment.Start - Padding + k.
	start := segment.Start - segment.Padding + pos + len(mathDelim)
	node := &mathBlock{singleLine: true}
	node.Lines().Append(text.NewSegment(start, start+closer))
	reader.AdvanceToEOL()
	return node, parser.NoChildren
}

// hasDisplayCloserAhead reports whether, in the lines after the one starting
// at from, a closing line (isDisplayCloserLine) comes before any blank line,
// any fence line (isFenceLine), or the end of the source. It reads raw lines,
// so callers only use it at document top level.
//
// A fence line ends the search for the same keep-when-unsure reason as a
// blank line: TeX never contains one, and a block that swallowed a fence's
// opener would leave its closer outside to open a fence running to EOF
// ("$$" then "```sh" / "kill -9 $$" / "```" is shell, not math). A line
// that opens an HTML block (opensHTMLBlock) ends it the same way, as it
// ends commentBlockParser's look-ahead: a block that swallowed "<script>"
// or "<pre>" would hide an opener whose HTML block runs to its end tag in
// every parser without the $$ extension (forgectl#767 review).
func hasDisplayCloserAhead(source []byte, from int) bool {
	nl := bytes.IndexByte(source[from:], '\n')
	if nl < 0 {
		return false
	}
	for i := from + nl + 1; i < len(source); {
		end := len(source)
		if n := bytes.IndexByte(source[i:], '\n'); n >= 0 {
			end = i + n
		}
		line := source[i:end]
		if util.IsBlank(line) || isFenceLine(line) || opensHTMLBlock(line) {
			return false
		}
		if isDisplayCloserLine(line) {
			return true
		}
		i = end + 1
	}
	return false
}

// opensHTMLBlock reports whether line could open an HTML block: up to three
// spaces of indent, then "<" and a letter, "/", "!" or "?". A TeX line that
// merely starts with a less-than sign ("< b") does not.
func opensHTMLBlock(line []byte) bool {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i+1 >= len(line) || line[i] != '<' {
		return false
	}
	c := line[i+1]
	return c == '/' || c == '!' || c == '?' || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

// isFenceLine reports whether line could open or close a fenced code block:
// up to three spaces of indent, then three or more backticks or tildes.
func isFenceLine(line []byte) bool {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return false
	}
	c, n := line[i], 0
	for i < len(line) && line[i] == c {
		i++
		n++
	}
	return n >= 3
}

// isDisplayCloserLine is the one closing-line test, shared by the look-ahead
// in Open and by Continue so the two cannot disagree about where a block
// ends: the line, right-trimmed, ends in an unescaped $$.
func isDisplayCloserLine(line []byte) bool {
	return endsWithDisplayDelim(util.TrimRightSpace(line))
}

// endsWithDisplayDelim reports whether b ends in an unescaped $$, so a
// closing line of "a \\$$" (a literal dollar, then $) does not close.
func endsWithDisplayDelim(b []byte) bool {
	for j := 0; j < len(b); j++ {
		switch {
		case b[j] == '\\':
			j++
		case b[j] == '$' && j == len(b)-2 && b[j+1] == '$':
			return true
		}
	}
	return false
}

// findDisplayCloser returns the index of the first unescaped $$ in b, or -1.
func findDisplayCloser(b []byte) int {
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\':
			i++
		case b[i] == '$' && i+1 < len(b) && b[i+1] == '$':
			return i
		}
	}
	return -1
}

func (mathBlockParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	// A one-line block is complete at Open. Closing without consuming hands
	// this line back to goldmark to open whatever block it starts.
	if m, ok := node.(*mathBlock); ok && m.singleLine {
		return parser.Close
	}
	line, segment := reader.PeekLine()
	if isDisplayCloserLine(line) {
		trimmed := util.TrimRightSpace(line)
		// "x $$" closes the block with "x" as its last line.
		if body := util.TrimRightSpace(trimmed[:len(trimmed)-len(mathDelim)]); !util.IsBlank(body) {
			seg := text.NewSegmentPadding(segment.Start, segment.Start+len(body)-segment.Padding, segment.Padding)
			seg.ForceNewline = true
			node.Lines().Append(seg)
		}
		reader.AdvanceToEOL()
		return parser.Close
	}
	seg := segment
	seg.ForceNewline = true // the last line at EOF still ends in a newline
	node.Lines().Append(seg)
	reader.AdvanceToEOL()
	return parser.Continue | parser.NoChildren
}

func (mathBlockParser) Close(ast.Node, text.Reader, parser.Context) {}

// CanInterruptParagraph is false by design; see mathBlockParser.
func (mathBlockParser) CanInterruptParagraph() bool { return false }

func (mathBlockParser) CanAcceptIndentedLine() bool { return false }

// mathFenceTransformer promotes every ```math fence to a mathBlock, the same
// collect-then-replace shape as mermaidTransformer and for the same reason:
// the highlighting extension owns ast.KindFencedCodeBlock, so the fence has to
// become a different node kind before render time.
type mathFenceTransformer struct{}

func (mathFenceTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var found []*ast.FencedCodeBlock

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		// Exact and case-sensitive, as for mermaid: "Math" or "mathml" is
		// not this.
		if fence, ok := n.(*ast.FencedCodeBlock); ok && string(fence.Language(reader.Source())) == mathInfo {
			found = append(found, fence)
		}
		return ast.WalkContinue, nil
	})

	for _, fence := range found {
		parent := fence.Parent()
		if parent == nil {
			continue
		}
		block := &mathBlock{}
		block.SetLines(fence.Lines())
		parent.ReplaceChild(parent, fence, block)
	}
}

// mathRenderer emits math nodes. The TeX is ALWAYS HTML-escaped, for the
// reason mermaidRenderer.render gives: TeX routinely contains < and >, the
// renderer runs under goldmarkhtml.WithUnsafe(), and this file should not
// depend on a policy in another file to stop "$</span><script>$" from
// breaking out. A client renderer reads textContent, which decodes the
// entities back to the TeX.
type mathRenderer struct{}

func (r mathRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMathInline, r.renderInline)
	reg.Register(kindMathBlock, r.renderBlock)
}

// renderInline keeps the element a <span> even for $$…$$ display math, since
// a <div> is not allowed inside the <p> this node sits in.
func (mathRenderer) renderInline(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	m, ok := n.(*mathInline)
	if !ok {
		return ast.WalkContinue, nil
	}
	class, delim := mathInlineClass, "$"
	if m.display {
		class, delim = mathDisplayClass, mathDelim
	}
	_, _ = w.WriteString(`<span class="` + class + `">` + delim)
	_, _ = w.Write(util.EscapeHTML(m.tex))
	_, _ = w.WriteString(delim + `</span>`)
	return ast.WalkSkipChildren, nil
}

func (mathRenderer) renderBlock(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	open := mathDelim + "\n"
	if m, ok := n.(*mathBlock); ok && m.singleLine {
		open = mathDelim
	}
	_, _ = w.WriteString(`<div class="` + mathDisplayClass + `">` + open)
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		segment := lines.At(i)
		_, _ = w.Write(util.EscapeHTML(segment.Value(source)))
	}
	_, _ = w.WriteString(mathDelim + "</div>\n")
	return ast.WalkSkipChildren, nil
}

// mathExtension wires the parsers, the fence transformer and the renderer as
// one unit, so no node kind is ever produced without a renderer for it.
//
// singleDollar turns on inline $…$ and midLineDisplay a $$…$$ anywhere in a
// line (see mathInlineParser). The zero value is the docs-root dialect.
type mathExtension struct{ singleDollar, midLineDisplay bool }

func (e mathExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(mathBlockParserOptions()...)
	m.Parser().AddOptions(
		// No other inline parser triggers on '$', so this priority only has to
		// stay clear of the ones other extensions use.
		parser.WithInlineParsers(util.Prioritized(mathInlineParser(e), 150)),
		parser.WithASTTransformers(util.Prioritized(mathFenceTransformer{}, 110)),
	)
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(mathRenderer{}, 100),
	))
}

// mathBlockParserOptions registers the $$ block parser. It is the ONE place
// that parser is configured, because linkscan's parser (newLinkMarkdown) must
// see the same block structure the renderer does: a "# x" or a link inside a
// $$ block is TeX, and a scan that parsed it as a heading or link would hand
// the resolver a slug or target the rendered page does not have. The inline
// parser is not shared: heading ids come from the raw source line, and a
// candidate holding link syntax is never math (crossesMarkdown).
func mathBlockParserOptions() []parser.Option {
	return []parser.Option{
		// Ahead of FencedCodeBlockParser (700). Nothing else triggers on '$';
		// the paragraph parser is the fallback when this declines.
		parser.WithBlockParsers(util.Prioritized(mathBlockParser{}, 650)),
	}
}
