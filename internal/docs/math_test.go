package docs

// Test plan for math.go
//
// inline math (Classification: core logic)
//   [x] Happy: $…$ renders as a math-inline span that keeps its delimiters
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
//   [x] Happy: a bare $$ line interrupts a paragraph; $$x$$ does not
//   [x] Happy: a ```math fence becomes a math-display div, not a chroma <pre>
//   [x] Happy: a ```go fence and a ```mermaid fence are untouched
//   [x] Unhappy: a stray "$$$" in prose does not open a block that swallows
//                the rest of the document
//
// escaping (Classification: security)
//   [x] Unhappy (security): markup inside inline math comes out as text
//   [x] Unhappy (security): markup inside a display block comes out as text
//
// heading ids (Classification: core logic)
//   [x] Happy: math in a heading does not change its id (linkscan parses
//              without math, so the two must agree)

import (
	"strings"
	"testing"
)

const (
	mathSpanOpen = `<span class="math math-inline">`
	mathDivOpen  = `<div class="math math-display">`
)

func TestRender_Math_InlineKeepsDelimiters(t *testing.T) {
	out := renderOrFail(t, "Euler $e^{i\\pi}+1=0$.")

	if want := mathSpanOpen + `$e^{i\pi}+1=0$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_InlineIsNotMarkdown(t *testing.T) {
	out := renderOrFail(t, "$a_1 * b_2 * c_3$ and $\\{a\\}$ and $\\$5$")

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
	out := renderOrFail(t, "$`x`$")

	if want := mathSpanOpen + `$x$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_CurrencyBeforeMath(t *testing.T) {
	out := renderOrFail(t, "Pay $20, get $x$ back.")

	if n := strings.Count(out, `class="math`); n != 1 {
		t.Fatalf("want exactly one math element, got %d: %s", n, out)
	}
	if want := mathSpanOpen + `$x$</span>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
	}
}

func TestRender_Math_InlineAcrossSoftBreak(t *testing.T) {
	out := renderOrFail(t, "Sum $a +\nb$ here.")

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

func TestRender_Math_BareOpenerInterruptsParagraph(t *testing.T) {
	out := renderOrFail(t, "para\n$$\nx\n$$\n")

	if !strings.Contains(out, "<p>para</p>") || !strings.Contains(out, mathDivOpen+"$$\nx\n$$</div>") {
		t.Errorf("a bare $$ line did not interrupt the paragraph: %s", out)
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

func TestRender_Math_DisplayFollowedByTextStaysInline(t *testing.T) {
	out := renderOrFail(t, "$$x$$ and more")

	if strings.Contains(out, mathDivOpen) {
		t.Errorf("$$x$$ with trailing text became a block: %s", out)
	}
	if want := `<p><span class="math math-display">$$x$$</span> and more</p>`; !strings.Contains(out, want) {
		t.Errorf("output missing %q: %s", want, out)
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

func TestRender_Math_InlineMarkupEscaped(t *testing.T) {
	for _, src := range []string{
		"$<img src=x onerror=alert(1)>$",
		"$</span><script>alert(1)</script>$",
	} {
		t.Run(src, func(t *testing.T) {
			out := renderOrFail(t, src)
			if !strings.Contains(out, mathSpanOpen+"$&lt;") {
				t.Errorf("markup was not escaped inside the math span: %s", out)
			}
			// onerror= may survive as escaped text; a live tag may not.
			for _, bad := range []string{"<img", "<script"} {
				if strings.Contains(out, bad) {
					t.Errorf("output carries a live %q: %s", bad, out)
				}
			}
		})
	}
}

func TestRender_Math_BlockMarkupEscaped(t *testing.T) {
	out := renderOrFail(t, "$$\n</div><script>alert(1)</script>\n$$\n")

	if strings.Contains(out, "<script") {
		t.Errorf("a script tag survived a display block: %s", out)
	}
	if !strings.Contains(out, mathDivOpen+"$$\n&lt;/div&gt;") {
		t.Errorf("markup was not escaped inside the display block: %s", out)
	}
}

func TestRender_Math_HeadingIDUnchanged(t *testing.T) {
	out := renderOrFail(t, "## Energy $E=mc^2$\n")

	if !strings.Contains(out, `<h2 id="energy-emc2">`) {
		t.Errorf("heading id changed: %s", out)
	}
	if !strings.Contains(out, mathSpanOpen+`$E=mc^2$</span>`) {
		t.Errorf("math in a heading was not rendered: %s", out)
	}
}
