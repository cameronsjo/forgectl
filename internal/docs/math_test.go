package docs

// Test plan for math.go
//
// inline math (Classification: core logic)
//   [x] Happy: on a VAULT root $…$ renders as a math-inline span that keeps
//              its delimiters (the single-dollar tests below use vault renders)
//   [x] Unhappy: on a DOCS root a lone $…$ is never math ($5 and $10, shell
//                variables, $x$); $$…$$ and ```math fences still are
//   [x] Unhappy: a docs-root render of single-dollar text is byte-identical to
//                a pipeline with no single-dollar parser at all
//   [x] Happy: markdown inside math is left alone (no emphasis, backslashes kept)
//   [x] Happy: GitHub's $`…`$ form drops the protecting backticks
//   [x] Happy: a leading currency amount does not stop later math
//   [x] Happy: inline math wraps across a soft line break
//   [x] Happy: $$x$$ followed by text stays inline, as display math
//   [x] Unhappy: currency, shell variables, spaced dollars, escaped dollars,
//                code spans, an unclosed $ and empty $$$$ produce no math
//
// display blocks (Classification: core logic)
//   [x] Happy: a $$ … $$ block becomes a math-display div
//   [x] Happy: a one-line $$…$$ block ends at its line
//   [x] Happy: a one-line block after a tab-indented container marker
//   [x] Happy: a $$ block never interrupts a paragraph; $$ lines straight
//              after text pair up as inline display math
//   [x] Happy: a $$ block after a blank line is a display div
//   [x] Unhappy: a stray $$ closer (its paragraph ended by a blank line,
//                heading, thematic break or setext underline) stays literal
//                and swallows nothing; a later real block is still a div
//   [x] Unhappy: a $$ between blank lines is literal
//   [x] Happy: a multi-line $$ in a list item is an inline display span
//   [x] Happy: $$ plus trailing spaces opens inline display math
//   [x] Unhappy: a $$ that never opens (code span, autolink, "$$ 5") ahead of
//                such math does not make a closer swallow later content, in
//                Render or in scanBody
//   [x] Happy: a ```math fence becomes a math-display div, not a chroma <pre>
//   [x] Happy: a ```go fence and a ```mermaid fence are untouched
//   [x] Unhappy: a stray "$$$" in prose does not open a block that swallows
//                the rest of the document
//
//   [x] Unhappy: a $ pair straddling a link, code span, autolink or raw-HTML
//                tag is not math; a spaced "a < b" still is
//   [x] Pinned limitation: "export PATH=$HOME/bin:$PATH" renders math
//   [x] Unhappy: an Obsidian-style bare $$ closer does not open a block
//   [x] Unhappy: a line ending in \$$ does not close a block
//
// escaping (Classification: security)
//   [x] Unhappy (security): tag-shaped inline candidates leave nothing live
//   [x] Unhappy (security): the inline renderer escapes TeX by itself
//   [x] Unhappy (security): markup inside a display block comes out as text
//
// heading ids (Classification: core logic)
//   [x] Happy: math in a heading does not change its id
//   [x] Happy: linkscan's slugs equal the rendered ids around $$ blocks, and
//              a link inside a $$ block is not indexed

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// renderVaultOrFail renders src as a vault-root page, the only root kind where
// single-dollar inline math is on.
func renderVaultOrFail(t *testing.T, src string) string {
	t.Helper()
	out, err := render([]byte(src), RootVault)
	if err != nil {
		t.Fatalf("render(%q, RootVault): %v", src, err)
	}
	return out
}

const (
	mathSpanOpen = `<span class="math math-inline">`
	mathDivOpen  = `<div class="math math-display">`
)

func TestRender_Math_InlineKeepsDelimiters(t *testing.T) {
	out := renderVaultOrFail(t, "Euler $e^{i\\pi}+1=0$.")

	if want := mathSpanOpen + `$e^{i\pi}+1=0$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_InlineIsNotMarkdown(t *testing.T) {
	out := renderVaultOrFail(t, "$a_1 * b_2 * c_3$ and $\\{a\\}$ and $\\$5$")

	if strings.Contains(out, "<em>") {
		t.Errorf("math was parsed as emphasis: %s", out)
	}
	if want := mathSpanOpen + `$a_1 * b_2 * c_3$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
	if want := mathSpanOpen + `$\{a\}$</span>`; !strings.Contains(out, want) {
		t.Errorf("backslashes were not kept, want %q: %s", want, out)
	}
	// An escaped dollar inside math neither closes it nor trips the digit rule.
	if want := mathSpanOpen + `$\$5$</span>`; !strings.Contains(out, want) {
		t.Errorf("an escaped dollar ended the math, want %q: %s", want, out)
	}
}

func TestRender_Math_BacktickFormDropsBackticks(t *testing.T) {
	out := renderVaultOrFail(t, "$`x`$")

	if want := mathSpanOpen + `$x$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_CurrencyBeforeMath(t *testing.T) {
	out := renderVaultOrFail(t, "Pay $20, get $x$ back.")

	if n := strings.Count(out, `class="math`); n != 1 {
		t.Fatalf("want exactly one math element, got %d: %s", n, out)
	}
	if want := mathSpanOpen + `$x$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_InlineAcrossSoftBreak(t *testing.T) {
	out := renderVaultOrFail(t, "Sum $a +\nb$ here.")

	if want := mathSpanOpen + "$a +\nb$</span>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_NotMath(t *testing.T) {
	for _, src := range []string{
		"It costs $5 and $10",
		"Range $5-$10",
		"Price $5, tax $1",
		"echo $HOME and $PATH",
		"$ spaced $",
		"\\$x\\$",
		"`$x$`",
		"$x left open",
		"$$$$",
	} {
		t.Run(src, func(t *testing.T) {
			if out := renderOrFail(t, src); strings.Contains(out, `class="math`) {
				t.Errorf("rendered math from %q: %s", src, out)
			}
		})
	}
}

func TestRender_Math_DisplayBlock(t *testing.T) {
	out := renderOrFail(t, "$$\n\\int_0^1 x\\,dx\n$$\n")

	if want := mathDivOpen + "$$\n\\int_0^1 x\\,dx\n$$</div>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_OneLineBlockEndsAtItsLine(t *testing.T) {
	out := renderOrFail(t, "$$x^2$$\nnext line\n")

	if want := mathDivOpen + "$$x^2$$</div>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
	if !strings.Contains(out, "<p>next line</p>") {
		t.Errorf("the line after a one-line block was not its own paragraph: %s", out)
	}
}

// A tab after a container marker reaches the parser as expanded padding
// columns, not source bytes, so the one-line block's TeX offset must subtract
// them or it slices the wrong bytes.
func TestRender_Math_OneLineBlockAfterTab(t *testing.T) {
	out := renderOrFail(t, ">\t$$y^2$$\n")

	if want := mathDivOpen + "$$y^2$$</div>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

// A $$ block never interrupts a paragraph: $$ lines straight after paragraph
// text stay in it and pair up as inline display math.
func TestRender_Math_BareOpenerDoesNotInterruptParagraph(t *testing.T) {
	out := renderOrFail(t, "para\n$$\nx\n$$\n")

	if strings.Contains(out, mathDivOpen) {
		t.Errorf("a bare $$ line interrupted the paragraph: %s", out)
	}
	if want := "<p>para\n" + `<span class="math math-display">$$` + "\nx\n$$</span></p>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_BlockAfterBlankLine(t *testing.T) {
	out := renderOrFail(t, "para\n\n$$\nx\n$$\n")

	if !strings.Contains(out, "<p>para</p>") || !strings.Contains(out, mathDivOpen+"$$\nx\n$$</div>") {
		t.Errorf("a $$ block after a blank line is not a display div: %s", out)
	}
}

// A $$ followed only by spaces or tabs opens like a bare $$.
func TestRender_Math_TrailingSpaceAfterInlineOpener(t *testing.T) {
	out := renderOrFail(t, "para\n$$  \nx\n$$\n")

	if want := `<span class="math math-display">$$`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

// strayCloserFixtures each leave a bare $$ closer behind: display math
// written after text, whose paragraph something (a blank line, an ATX
// heading, a thematic break, a setext underline) ends before the closer, or
// a $$ followed by a fence. That
// stray $$ must stay literal rather than open a block that runs to the next
// $$ or EOF, and the real display block later in the document must still be
// one.
var strayCloserFixtures = map[string]string{
	"blank line":       "Intro:\n$$\na\n\n$$" + strayCloserTail,
	"atx heading":      "para\n$$\n# x\n$$" + strayCloserTail,
	"thematic break":   "Def:\n$$\n***\n$$" + strayCloserTail,
	"setext underline": "para\n$$\nx\n===\n$$" + strayCloserTail,
	"obsidian + blank": "$$x = 1\n\n$$" + strayCloserTail,
	// A fence opener after a $$ must not be absorbed into a math block, or
	// the fence's closer is left outside to open a fence that runs to EOF.
	"stray closer then fence": "Intro:\n$$\na\n\n$$\n```bash\necho pid $$\n```" + strayCloserTail,
	"heading then fence":      "# T\n$$\n```sh\nkill -9 $$\n```" + strayCloserTail,
	"backtick fence in $$":    "$$\n```\nx\n$$\n```" + strayCloserTail,
	"tilde fence in $$":       "$$\n~~~\nx\n$$\n~~~" + strayCloserTail,
}

const strayCloserTail = "\n\n## After\n\n[link](a.md)\n\n$$\ny\n$$\n"

func TestRender_Math_StrayCloserDoesNotSwallow(t *testing.T) {
	for name, src := range strayCloserFixtures {
		t.Run(name, func(t *testing.T) {
			out := renderOrFail(t, src)
			for _, want := range []string{
				`<h2 id="after">After</h2>`,
				`<a href="a.md" rel="nofollow">link</a>`,
				mathDivOpen + "$$\ny\n$$</div>",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q: %s", want, out)
				}
			}

			headings, links, _, err := scanBody([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(headings) == 0 || headings[len(headings)-1].Slug != "after" {
				t.Errorf("scan lost the heading after the stray closer: %+v", headings)
			}
			if len(links) != 1 || links[0].Path != "a.md" {
				t.Errorf("scan lost the link after the stray closer: %+v", links)
			}
		})
	}
}

// A $$ between blank lines has no closer before the next blank line, so it
// is text.
func TestRender_Math_LoneDelimiterIsLiteral(t *testing.T) {
	out := renderOrFail(t, "a\n\n$$\n\nb\n")

	if strings.Contains(out, `class="math`) {
		t.Errorf("a lone $$ opened math: %s", out)
	}
	if !strings.Contains(out, "<p>$$</p>") || !strings.Contains(out, "<p>b</p>") {
		t.Errorf("a lone $$ did not stay a literal paragraph: %s", out)
	}
}

// Inside a container the look-ahead cannot see boundaries, so the multi-line
// block does not open there; the inline parser renders a display span.
func TestRender_Math_MultiLineInListItemIsInline(t *testing.T) {
	out := renderOrFail(t, "- $$\n  x\n  $$\n\nafter\n")

	if strings.Contains(out, mathDivOpen) {
		t.Errorf("a multi-line block opened inside a list item: %s", out)
	}
	if want := `<span class="math math-display">$$` + "\nx\n$$</span>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
	if !strings.Contains(out, "<p>after</p>") {
		t.Errorf("content after the list was swallowed: %s", out)
	}
}

// nonOpenerFixtures put a $$ that the inline parser never opens (in a code
// span, in an autolink, followed by a space) ahead of display math written
// straight after the text. That $$ must not change how the math pairs, or the
// closer becomes an opener that swallows everything to the next $$.
var nonOpenerFixtures = map[string]string{
	"code span": "Write display math with `$$`:" + nonOpenerTail,
	"autolink":  "See <http://x/$$>:" + nonOpenerTail,
	"spaced":    "It costs $$ 5:" + nonOpenerTail,
}

const nonOpenerTail = "\n$$\nx_1 * y_2\n$$\n\n## After\n\n[link](a.md)\n"

func TestRender_Math_NonOpenerDollarsDoNotSwallow(t *testing.T) {
	for name, src := range nonOpenerFixtures {
		t.Run(name, func(t *testing.T) {
			out := renderOrFail(t, src)
			for _, want := range []string{
				`<span class="math math-display">$$` + "\nx_1 * y_2\n$$</span>",
				`<h2 id="after">After</h2>`,
				`<a href="a.md" rel="nofollow">link</a>`,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q: %s", want, out)
				}
			}

			headings, links, _, err := scanBody([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(headings) != 1 || headings[0].Slug != "after" {
				t.Errorf("scan lost the heading after the math: %+v", headings)
			}
			if len(links) != 1 || links[0].Path != "a.md" {
				t.Errorf("scan lost the link after the math: %+v", links)
			}
		})
	}
}

func TestRender_Math_OneLineBlockDoesNotInterruptParagraph(t *testing.T) {
	out := renderOrFail(t, "para\n$$x$$\n")

	if strings.Contains(out, mathDivOpen) {
		t.Errorf("$$x$$ interrupted a paragraph: %s", out)
	}
	if want := `<span class="math math-display">$$x$$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

// "$$x$$ and more" is not a block in either dialect. A vault root keeps it
// as inline display math; a docs root, where $$ must be block-shaped
// (forgectl#650), keeps it as text.
func TestRender_Math_DisplayFollowedByTextStaysInline(t *testing.T) {
	out := renderVaultOrFail(t, "$$x$$ and more")

	if strings.Contains(out, mathDivOpen) {
		t.Errorf("$$x$$ with trailing text became a block: %s", out)
	}
	if want := `<p><span class="math math-display">$$x$$</span> and more</p>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
	if got, want := renderOrFail(t, "$$x$$ and more"), "<p>$$x$$ and more</p>\n"; got != want {
		t.Errorf("docs root = %q, want %q", got, want)
	}
}

func TestRender_Math_StrayDollarsStayInTheirParagraph(t *testing.T) {
	out := renderOrFail(t, "$$$ is what you need.\n\nMore text.\n")

	if strings.Contains(out, `class="math`) {
		t.Errorf("stray dollars rendered as math: %s", out)
	}
	if !strings.Contains(out, "<p>More text.</p>") {
		t.Errorf("the following paragraph was swallowed: %s", out)
	}
}

func TestRender_Math_Fence(t *testing.T) {
	out := renderOrFail(t, "```math\na^2 + b^2\n```\n")

	if want := mathDivOpen + "$$\na^2 + b^2\n$$</div>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
	if strings.Contains(out, "<pre") {
		t.Errorf("a math fence still rendered as a code block: %s", out)
	}
}

func TestRender_Math_OtherFencesUntouched(t *testing.T) {
	out := renderOrFail(t, "```go\nfunc main() {}\n```\n\n```mermaid\ngraph LR\n```\n")

	if !strings.Contains(out, `class="chroma"`) {
		t.Errorf("a go fence lost its highlighting: %s", out)
	}
	if !strings.Contains(out, `<pre class="mermaid">`) {
		t.Errorf("a mermaid fence is no longer a mermaid pre: %s", out)
	}
	if strings.Contains(out, `class="math`) {
		t.Errorf("a non-math fence rendered as math: %s", out)
	}
}

// A tag inside $…$ is not math at all (crossesMarkdown), so it reaches the
// sanitizer as ordinary raw HTML; what matters is that nothing live survives.
func TestRender_Math_InlineMarkupNotLive(t *testing.T) {
	for _, src := range []string{
		"$<img src=x onerror=alert(1)>$",
		"$</span><script>alert(1)</script>$",
	} {
		t.Run(src, func(t *testing.T) {
			out := renderOrFail(t, src)
			for _, bad := range []string{"onerror", "<script"} {
				if strings.Contains(out, bad) {
					t.Errorf("output carries a live %q: %s", bad, out)
				}
			}
		})
	}
}

// The renderer escapes inline TeX on its own, without leaning on the parser's
// refusal of tag-shaped candidates or on the sanitizer: render a hostile node
// directly through a pipeline that has neither.
func TestMathRenderer_InlineEscapes(t *testing.T) {
	md := goldmark.New(
		goldmark.WithExtensions(mathExtension{}),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
	doc := ast.NewDocument()
	para := ast.NewParagraph()
	doc.AppendChild(doc, para)
	para.AppendChild(para, &mathInline{tex: []byte(`</span><script>alert(1)</script>`)})

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, nil, doc); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "<script") {
		t.Errorf("inline TeX was written unescaped: %s", out)
	}
	if want := mathSpanOpen + "$&lt;/span&gt;&lt;script&gt;"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

// Comparison operators that are not tag openers stay math, and are escaped.
func TestRender_Math_SpacedComparisonStaysMath(t *testing.T) {
	out := renderVaultOrFail(t, "if $a < b$ and $c<1$ then")

	if want := mathSpanOpen + "$a &lt; b$</span>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
	if want := mathSpanOpen + "$c&lt;1$</span>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

// A $ pair that straddles a link, code span or autolink is not math: the other
// construct keeps its syntax.
func TestRender_Math_DoesNotCrossOtherConstructs(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"[$HOME](a.md) and [$PATH](b.md)", `<a href="a.md" rel="nofollow">$HOME</a> and <a href="b.md" rel="nofollow">$PATH</a>`},
		{"x $y `z$` w", "<code>z$</code>"},
		{"$a <http://x/b$c> d", `<a href="http://x/b$c"`},
		{"$a <span title=\"q$\">b</span>", `<span>b</span>`},
	} {
		t.Run(tc.src, func(t *testing.T) {
			out := renderOrFail(t, tc.src)
			if strings.Contains(out, `class="math`) {
				t.Errorf("rendered math across another construct: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %q: %s", tc.want, out)
			}
		})
	}
}

// Known limitation, pinned so a change to it is deliberate: pandoc's rules
// read "$HOME/bin:$" as math, because the second $ follows a non-space and is
// not followed by a digit. The shell line has to go in a code span.
func TestRender_Math_ShellAssignmentLimitation(t *testing.T) {
	out := renderVaultOrFail(t, "export PATH=$HOME/bin:$PATH")

	if want := mathSpanOpen + "$HOME/bin:$</span>"; !strings.Contains(out, want) {
		t.Errorf("the pinned limitation changed, want %q: %s", want, out)
	}
}

// Obsidian writes "$$x = 1" and closes with a bare "$$" line. That closer
// belongs to the paragraph's open math; if it opened a block instead, the
// block would run to the next $$ and swallow everything in between.
func TestRender_Math_ObsidianCloserDoesNotOpenBlock(t *testing.T) {
	out := renderOrFail(t, obsidianCloserFixture)

	for _, want := range []string{
		`<span class="math math-display">$$x = 1` + "\n" + `$$</span>`,
		`<h1 id="heading">Heading</h1>`,
		`<a href="a.md" rel="nofollow">link</a>`,
		`<strong>bold</strong>`,
		mathDivOpen + "$$\ny\n$$</div>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
}

const obsidianCloserFixture = "$$x = 1\n$$\n\n# Heading\n\nSee [link](a.md) and **bold**.\n\n$$\ny\n$$\n"

// A closing line ending in an escaped dollar ("\$$") is a literal dollar
// followed by $, not the closer.
func TestRender_Math_EscapedDelimiterDoesNotCloseBlock(t *testing.T) {
	out := renderOrFail(t, "$$\na \\$$\nb\n$$\n")

	if want := mathDivOpen + "$$\na \\$$\nb\n$$</div>"; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

// renderedIDs returns every id attribute in rendered HTML, in order.
func renderedIDs(t *testing.T, src string) []string {
	t.Helper()
	var ids []string
	for _, m := range regexp.MustCompile(`<h[1-6] id="([^"]*)"`).FindAllStringSubmatch(renderOrFail(t, src), -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// linkscan must see the block structure the renderer does, or the resolver
// indexes headings and links the rendered page does not have.
func TestScanBody_MathBlockParity(t *testing.T) {
	fixtures := map[string]string{
		"heading in block": "$$\n# x\n$$\n\n# x\n",
		"obsidian closer":  obsidianCloserFixture,
		// A heading inside $$ lines that follow paragraph text interrupts the
		// paragraph as CommonMark says; both parsers must see it, and what
		// follows the stray closer.
		"heading after para": "para\n$$\n# x\n$$\n\n## After\n\n[link](a.md)\n",
	}
	for k, v := range nonOpenerFixtures {
		fixtures["non-opener "+k] = v
	}
	for k, v := range strayCloserFixtures {
		fixtures["stray closer "+k] = v
	}
	for name, src := range fixtures {
		t.Run(name, func(t *testing.T) {
			headings, _, _, err := scanBody([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			var slugs []string
			for _, h := range headings {
				slugs = append(slugs, h.Slug)
			}
			if ids := renderedIDs(t, src); !slices.Equal(slugs, ids) {
				t.Errorf("scanned slugs %v, rendered ids %v", slugs, ids)
			}
		})
	}
}

func TestScanBody_LinkInsideMathBlockNotIndexed(t *testing.T) {
	_, links, _, err := scanBody([]byte("$$\n[x](inside.md)\n$$\n\n[y](outside.md)\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || !strings.Contains(links[0].Path, "outside") {
		t.Errorf("want only the link outside the block, got %+v", links)
	}
}

func TestRender_Math_BlockMarkupEscaped(t *testing.T) {
	out := renderOrFail(t, "$$\na </div><script>alert(1)</script>\n$$\n")

	if strings.Contains(out, "<script") {
		t.Errorf("a script tag survived a display block: %s", out)
	}
	if !strings.Contains(out, mathDivOpen+"$$\na &lt;/div&gt;") {
		t.Errorf("markup was not escaped inside the display block: %s", out)
	}
	// A line that opens an HTML block does not go into a $$ block at all
	// (hasDisplayCloserAhead); the sanitizer still drops its script.
	if out := renderOrFail(t, "$$\n</div><script>alert(1)</script>\n$$\n"); strings.Contains(out, "<script") || strings.Contains(out, mathDivOpen) {
		t.Errorf("an HTML-block opener went into a display block, or its script survived: %s", out)
	}
	// A TeX line that only starts with a less-than sign is still math.
	if out := renderOrFail(t, "$$\na\n< b\n$$\n"); !strings.Contains(out, mathDivOpen) {
		t.Errorf("a TeX line starting with \"< \" stopped a display block: %s", out)
	}
}

func TestRender_Math_HeadingIDUnchanged(t *testing.T) {
	out := renderVaultOrFail(t, "## Energy $E=mc^2$\n")

	if !strings.Contains(out, `<h2 id="energy-emc2">`) {
		t.Errorf("heading id changed: %s", out)
	}
	if !strings.Contains(out, mathSpanOpen+`$E=mc^2$</span>`) {
		t.Errorf("math in a heading was not rendered: %s", out)
	}
}

// singleDollarCorpus is prose that is math to a $…$ parser and text to a docs
// root: the ecosystem-corpus shell strings from #600 plus plain currency and a
// real-looking formula.
var singleDollarCorpus = []string{
	"It costs $5 and $10",
	`"$RAW" and "$KIND"`,
	"rm -rf $TMPDIR/$$ now",
	"export PATH=$HOME/bin:$PATH",
	"Euler $e^{i\\pi}+1=0$.",
	"$a_1 * b_2 * c_3$",
	"$`x`$",
}

// A docs root never turns a lone $…$ into math, and (the vault half of the
// same gate) a vault root still does for the strings that are math under
// pandoc's rules.
func TestRender_Math_SingleDollarIsVaultOnly(t *testing.T) {
	for _, src := range singleDollarCorpus {
		t.Run(src, func(t *testing.T) {
			if out := renderOrFail(t, src); strings.Contains(out, `class="math`) {
				t.Errorf("docs root rendered single-dollar math: %s", out)
			}
		})
	}
	// The three #600 corpus strings stay literal in docs roots, byte for byte.
	for src, want := range map[string]string{
		"export PATH=$HOME/bin:$PATH": "<p>export PATH=$HOME/bin:$PATH</p>\n",
		`"$RAW" and "$KIND"`:          "<p>&#34;$RAW&#34; and &#34;$KIND&#34;</p>\n",
		"It costs $5 and $10":         "<p>It costs $5 and $10</p>\n",
	} {
		if got := renderOrFail(t, src); got != want {
			t.Errorf("docs render of %q = %q, want %q", src, got, want)
		}
	}
	// Vault roots keep the pandoc-rule behaviour: real math parses, currency
	// does not.
	if out := renderVaultOrFail(t, "Euler $e^{i\\pi}+1=0$."); !strings.Contains(out, mathSpanOpen) {
		t.Errorf("vault root lost single-dollar math: %s", out)
	}
	if out := renderVaultOrFail(t, "It costs $5 and $10"); strings.Contains(out, `class="math`) {
		t.Errorf("vault root rendered currency as math: %s", out)
	}
}

// The docs-root dialect keeps every block-shaped math form.
func TestRender_Math_DocsRootKeepsDoubleDollarAndFence(t *testing.T) {
	if out := renderVaultOrFail(t, "so $$x^2$$ here"); !strings.Contains(out, mathDisplayOpenSpan) {
		t.Errorf("vault root lost inline $$…$$: %s", out)
	}
	if out := renderOrFail(t, "$$\nx^2\n$$\n"); !strings.Contains(out, mathDivOpen) {
		t.Errorf("docs root lost a $$ block: %s", out)
	}
	if out := renderOrFail(t, "```math\nx^2\n```\n"); !strings.Contains(out, mathDivOpen) {
		t.Errorf("docs root lost a math fence: %s", out)
	}
}

const mathDisplayOpenSpan = `<span class="math math-display">`

// Byte-identity: a docs root renders single-dollar text exactly as a pipeline
// that has NO inline math parser would (same extensions and options as
// newMarkdown(false, false), minus the single-dollar path), so the gate
// changes nothing beyond dropping $…$. Both sides compare goldmark output
// before sanitizing.
func TestRender_Math_DocsRootByteIdenticalWithoutInlineMath(t *testing.T) {
	ref := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(headingParserOptions(false)...),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
	for _, src := range singleDollarCorpus {
		var want bytes.Buffer
		if err := ref.Convert([]byte(src), &want); err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if err := markdownPlain.Convert([]byte(src), &got); err != nil {
			t.Fatal(err)
		}
		if got.String() != want.String() {
			t.Errorf("docs root output for %q differs from a no-inline-math pipeline:\n got %q\nwant %q", src, got.String(), want.String())
		}
	}
}

// In a docs root, $$ is math only when it is block-shaped: the opener starts
// its line and the closer ends its line, whitespace aside (forgectl#650).
// Shell's PID, currency and any other mid-line $$ stay text; a vault root
// keeps Obsidian's inline $$…$$.
func TestRender_Math_DocsRootDoubleDollarMustBeBlockShaped(t *testing.T) {
	literal := map[string]string{
		"tmp=/tmp/x.$$; rm /tmp/y.$$": "<p>tmp=/tmp/x.$$; rm /tmp/y.$$</p>\n",
		"cost $$5 and $$10":           "<p>cost $$5 and $$10</p>\n",
		"PID $$ of the shell":         "<p>PID $$ of the shell</p>\n",
		"echo $$":                     "<p>echo $$</p>\n",
		"so $$x^2$$ here":             "<p>so $$x^2$$ here</p>\n",
		"$$x$$ and $$y$$":             "<p>$$x$$ and $$y$$</p>\n",
		"kill $$\n$$ is the PID":      "<p>kill $$\n$$ is the PID</p>\n",
		"$$x\nthen $$y$$":             "<p>$$x\nthen $$y$$</p>\n",
		"- run echo $$ and $$x$$\n":   "<ul>\n<li>run echo $$ and $$x$$</li>\n</ul>\n",
		"> tmp=x.$$; rm y.$$\n":       "<blockquote>\n<p>tmp=x.$$; rm y.$$</p>\n</blockquote>\n",
		"# PID $$ and $$x$$\n":        `<h1 id="pid--and-x">PID $$ and $$x$$</h1>` + "\n",
	}
	for src, want := range literal {
		if got := renderOrFail(t, src); got != want {
			t.Errorf("docs render of %q = %q, want %q", src, got, want)
		}
	}
	// Block-shaped display math still renders in a docs root, including the
	// forms that reach the inline parser rather than the block parser.
	for _, src := range []string{
		"$$x$$\n",              // one-line block
		"$$\nx\n$$\n",          // multi-line block
		"para\n$$x$$\n",        // straight after paragraph text
		"para\n$$\nx\n$$\n",    // multi-line, straight after text
		"para\n  $$x$$  \nb\n", // indented, trailing spaces
		"- $$\n  x\n  $$\n",    // in a list item
		"> $$\n> x\n> $$\n",    // in a blockquote
		"> $$x$$\n",            // one-line, in a blockquote
		"para\n$$x\ny$$\n",     // opener and closer on their own lines' ends
	} {
		if out := renderOrFail(t, src); !strings.Contains(out, `class="math math-display"`) {
			t.Errorf("docs root lost block-shaped display math %q: %s", src, out)
		}
	}
	// The vault root keeps inline $$…$$ anywhere in a line.
	for _, src := range []string{"so $$x^2$$ here", "$$x$$ and $$y$$", "a $$x\ny$$ b"} {
		if out := renderVaultOrFail(t, src); !strings.Contains(out, mathDisplayOpenSpan) {
			t.Errorf("vault root lost inline $$…$$ %q: %s", src, out)
		}
	}
}

// TestMathBlock_RefusesToSwallowAnHTMLOpener: a $$ block does not open over
// a line that starts an HTML block, as it does not over a fence, so
// "$$" / "<script>" / "$$" is no math block (the %% comment block refuses
// the same line through opensFenceOrHTML). Mutation: dropping
// opensHTMLBlock from hasDisplayCloserAhead turns this red.
func TestMathBlock_RefusesToSwallowAnHTMLOpener(t *testing.T) {
	for _, src := range []string{"$$\n<script>\n$$\n", "$$\n<pre>\n$$\n", "$$\n  <div>\n$$\n"} {
		b := []byte(src)
		doc := markdownPlain.Parser().Parse(text.NewReader(b), parser.WithContext(newParseContext()))
		_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
			if _, ok := n.(*mathBlock); ok && entering {
				t.Errorf("%q opened a $$ block over an HTML opener", src)
			}
			return ast.WalkContinue, nil
		})
	}
}
