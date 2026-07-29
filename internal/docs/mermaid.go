package docs

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// mermaidInfo is the fence info string that marks a diagram block.
const mermaidInfo = "mermaid"

// mermaidClass is the class mermaid.js looks for by default when it scans the
// page. Emitting exactly this means the client needs no querySelector override.
const mermaidClass = "mermaid"

// kindMermaidBlock is a node kind of our own, distinct from
// ast.KindFencedCodeBlock. That distinction is the entire mechanism here.
//
// goldmark resolves ONE renderer per node kind, by priority, and the
// highlighting extension registers itself for ast.KindFencedCodeBlock. So a
// mermaid fence cannot simply be given a second renderer alongside chroma's —
// whichever wins, wins for every fenced block, and chroma's internals are not
// exported, so a renderer that wanted to handle mermaid and delegate the rest
// would have to reimplement syntax highlighting to do it.
//
// Promoting the fence to a different KIND during AST transformation sidesteps
// the contest: by render time the node is no longer a fenced code block, so
// chroma is never consulted about it and every other fence keeps its
// highlighting untouched.
//
// Worth being precise about what this does and does not fix. As of chroma
// v2.27, there is no `mermaid` lexer, so goldmark-highlighting currently falls
// back to emitting a plain <pre><code class="language-mermaid"> with the diagram
// text intact — a client COULD read that today. This transform is not repairing
// broken output; it is (a) emitting the canonical <pre class="mermaid"> markup
// mermaid.js expects, and (b) making the reader immune to chroma later gaining
// a mermaid lexer, which would silently start fragmenting the diagram source
// into token spans and break the client with no error and no diff on our side.
var kindMermaidBlock = ast.NewNodeKind("MermaidBlock")

// mermaidBlock holds a diagram fence's raw source lines, to be emitted verbatim
// (HTML-escaped) for the browser-side mermaid renderer.
type mermaidBlock struct {
	ast.BaseBlock
}

func (*mermaidBlock) Kind() ast.NodeKind { return kindMermaidBlock }

func (n *mermaidBlock) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// mermaidTransformer rewrites every ```mermaid fence into a mermaidBlock.
type mermaidTransformer struct{}

// Transform walks the document and promotes mermaid fences. It collects matches
// before mutating, because replacing a node while walking the same tree is how
// you get skipped siblings.
func (mermaidTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var found []*ast.FencedCodeBlock

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		fence, ok := n.(*ast.FencedCodeBlock)
		if !ok {
			return ast.WalkContinue, nil
		}
		// Language() returns the first word of the info string, so
		// ```mermaid and ```mermaid title="x" both match. Compared
		// case-sensitively and exactly: "Mermaid" or "mermaidjs" are not this.
		if lang := fence.Language(reader.Source()); string(lang) == mermaidInfo {
			found = append(found, fence)
		}
		return ast.WalkContinue, nil
	})

	for _, fence := range found {
		parent := fence.Parent()
		if parent == nil {
			continue
		}
		block := &mermaidBlock{}
		// Carry the fence's source segments over unchanged — the diagram text
		// is never reparsed or reformatted, just relocated onto a node kind
		// chroma does not claim.
		block.SetLines(fence.Lines())
		parent.ReplaceChild(parent, fence, block)
	}
}

// mermaidRenderer emits mermaidBlock nodes as <pre class="mermaid">.
type mermaidRenderer struct{}

func (r mermaidRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMermaidBlock, r.render)
}

// render writes the diagram source HTML-ESCAPED inside the <pre>. Escaping is
// not optional: mermaid syntax routinely contains < and >, and a node label can
// contain arbitrary author text. Unescaped, a label could close the <pre> and
// inject markup — and because goldmarkhtml.WithUnsafe() is enabled for this
// renderer, nothing downstream would put it back. (bluemonday still sanitizes
// afterwards and would strip an injected script, but relying on that as the only
// defense would make this renderer's correctness depend on a policy defined in
// another file.) mermaid.js reads the element's textContent, which decodes the
// entities back to the original source, so escaping costs nothing.
func (mermaidRenderer) render(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}

	_, _ = w.WriteString(`<pre class="` + mermaidClass + `">`)
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		segment := lines.At(i)
		_, _ = w.Write(util.EscapeHTML(segment.Value(source)))
	}
	_, _ = w.WriteString("</pre>\n")

	return ast.WalkSkipChildren, nil
}

// mermaidExtension bundles the transformer and the renderer so render.go wires
// them as a single unit — registering one without the other would either leave
// mermaid fences as unrenderable nodes of an unknown kind, or register a
// renderer for a kind nothing ever produces.
type mermaidExtension struct{}

func (mermaidExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithASTTransformers(
		// Priority 100 is arbitrary but must be stable: nothing else in this
		// pipeline transforms fenced code blocks, so there is no ordering
		// contest to lose.
		util.Prioritized(mermaidTransformer{}, 100),
	))
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(mermaidRenderer{}, 100),
	))
}
