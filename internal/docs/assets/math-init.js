// Math rendering for `forgectl docs serve`.
//
// math.go emits every formula as HTML-escaped TeX inside a .math element, with
// its original delimiters kept, so a page without this script still shows
// readable source. This script swaps each one for KaTeX output. The shapes it
// receives:
//
//   <span class="math math-inline">$…$</span>
//   <span class="math math-display">$$…$$</span>   display math inside a <p>
//   <div class="math math-display">$$…$$</div>     a one-line $$ block
//   <div class="math math-display">$$\n…\n$$</div> a multi-line $$ block or a
//                                                  ```math fence
//
// Display mode is decided by the math-display class, never by the element: a
// <div> is not allowed inside a <p>, so display math written inside a
// paragraph arrives as a <span>.
(function () {
  "use strict";

  if (typeof katex === "undefined") {
    // The bundle failed to load. The TeX stays visible as text with its
    // delimiters, which is a legible degradation.
    console.warn("[forgectl docs] KaTeX bundle unavailable; math will render as TeX source");
    return;
  }

  // Errors render in the Artificer --urgent color, read at render time so the
  // two themes each get their own. The fallback is only reached if the
  // stylesheet failed to load.
  function errorColor() {
    var v = getComputedStyle(document.documentElement).getPropertyValue("--urgent");
    v = (v || "").trim();
    return v === "" ? "#cc0000" : v;
  }

  // strip removes exactly one delimiter from each end, never a pattern:
  // "$a\$$" is the TeX "a\$" (an escaped dollar before the closer), and a
  // greedy strip would eat the escaped one too. Text that does not carry the
  // expected delimiters is rendered as is.
  function strip(src, display) {
    var d = display ? "$$" : "$";
    if (src.length >= 2 * d.length && src.slice(0, d.length) === d && src.slice(-d.length) === d) {
      return src.slice(d.length, src.length - d.length);
    }
    return src;
  }

  // KaTeX builds its output straight into the DOM, which skips the HTML
  // parser's nesting cap, and deeply nested TeX crashes the tab: Chromium
  // dies at about 150 levels of \frac{…}{ or 200 nested matrix
  // environments (forgectl#596). A formula over either bound is left as its
  // TeX source instead. MAX_DEPTH counts brace groups, \begin…\end,
  // \left…\right and the \begingroup/\bgroup forms together: each nests the
  // output a level deeper (nested matrices crash with no brace nesting at
  // all), and they can be mixed to dodge a per-kind count.
  //
  // MAX_CELLS bounds the alignment separators, & and \\, a formula may hold
  // (forgectl#690). Each one opens a cell, a macro-free source can hold
  // thousands of them, and a cell costs a dozen or more nodes even when it
  // is empty: an empty matrix row of 9,900 & builds about 170,000 nodes, in
  // about 2 s, before tooBig can reject it. 2,000 cells is a 40 × 50
  // matrix, far past any written by hand, and builds at most about 35,000
  // nodes, under MAX_NODES.
  var MAX_SOURCE = 10000;
  var MAX_DEPTH = 100;
  var MAX_CELLS = 2000;

  // LINE_END matches the characters that end a line for KaTeX's lexer, whose
  // \verb match uses ".", which stops at each of them.
  var LINE_END = /[\n\r\u2028\u2029]/;

  // verbEnd returns the index of the delimiter that closes a \verb whose
  // delimiter is at src[k], or -1 when KaTeX would not lex it as \verb. It
  // follows KaTeX's lexer: \verb*<d>…<d> takes any <d>; plain \verb<d>…<d>
  // takes any <d> but * and an ASCII letter (@ included); the body is the
  // shortest run to the next <d> that does not cross a line end.
  function verbEnd(src, k, star) {
    if (k >= src.length) { return -1; }
    var d = src.charAt(k);
    if (!star && (d === "*" || /[A-Za-z]/.test(d))) { return -1; }
    for (var m = k + 1; m < src.length; m++) {
      var ch = src.charAt(m);
      if (ch === d) { return m; }
      if (LINE_END.test(ch)) { return -1; }
    }
    return -1;
  }

  // tooComplex reports whether src is over MAX_SOURCE characters, nests
  // deeper than MAX_DEPTH, or holds more than MAX_CELLS & and \\. An escaped
  // brace (\{, \}) is a literal, a control symbol (\\, \$, …) consumes the
  // one character after it, a % comment runs to the end of its line, and
  // \verb<d>…<d> is one opaque token, as KaTeX reads them. The \verb rule
  // is what keeps a % inside it (\verb|%|) from reading as a comment that
  // hides the openers after it (forgectl#690). A \verb KaTeX would not lex
  // (no closing delimiter on its line) is scanned as ordinary text, which can
  // only count more. A closer never takes the depth below zero, so surplus
  // closers cannot bank credit for later openers.
  function tooComplex(src) {
    if (src.length > MAX_SOURCE) { return true; }
    var depth = 0;
    var cells = 0;
    for (var i = 0; i < src.length; i++) {
      var c = src.charAt(i);
      if (c === "{") {
        depth++;
      } else if (c === "}") {
        depth = Math.max(0, depth - 1);
      } else if (c === "&") {
        cells++;
      } else if (c === "%") {
        while (i + 1 < src.length && src.charAt(i + 1) !== "\n") { i++; }
      } else if (c === "\\") {
        var j = i + 1;
        while (j < src.length && /[A-Za-z]/.test(src.charAt(j))) { j++; }
        var name = src.slice(i + 1, j);
        if (name === "verb") {
          var star = src.charAt(j) === "*";
          var end = verbEnd(src, star ? j + 1 : j, star);
          if (end >= 0) {
            i = end;
            continue;
          }
        }
        if (name === "begin" || name === "left" || name === "begingroup" || name === "bgroup") {
          depth++;
        } else if (name === "end" || name === "right" || name === "endgroup" || name === "egroup") {
          depth = Math.max(0, depth - 1);
        } else if (name === "" && src.charAt(j) === "\\") {
          cells++;
        }
        // A command name ends before j; a control symbol is one character.
        i = name === "" ? i + 1 : j - 1;
      }
      if (depth > MAX_DEPTH || cells > MAX_CELLS) { return true; }
    }
    return false;
  }

  // DEFINER matches a control word that defines a macro: \def and its
  // \gdef/\edef/\xdef forms, the \newcommand family, \let, \futurelet,
  // and the \global prefix. A formula that defines a macro is not rendered
  // (forgectl#675). Macros are how a short source multiplies KaTeX's work
  // without tripping maxExpand: \def\a{\binom11…} applied 10×10×4 times
  // is 445 expansions and 882 characters, and KaTeX takes about 30 s to
  // build 1.44M nodes before tooBig can reject them. They are also the only
  // way past the source scan to deep output, whose layout cost grows about
  // with the cube of its depth.
  //
  // The match runs over the raw source, not the tokens tooComplex walks:
  // it ignores % comments and does not pair backslashes, so a \def inside
  // a comment, or read after \\, still counts. That only ever skips a
  // formula it could have rendered; a scan that tried to tokenize could be
  // steered past a real definition (\verb, for one, lexes its own
  // delimiter). The trailing (?![A-Za-z]) makes it a whole control word, so
  // \define and \letter are not definitions. KaTeX's lexer also counts @
  // as a letter, so \def@ is over-matched here, harmlessly.
  var DEFINER = /\\(?:[gex]?def|(?:re)?newcommand|providecommand|(?:future)?let|global)(?![A-Za-z])/;

  // The post-render bound. The source scan above cannot see macro
  // expansion, and DEFINER is a pre-scan of the source, so this is the
  // backstop: KaTeX renders into a detached element, where nothing is laid
  // out, and the output is attached only if its DOM is at most
  // MAX_DOM_DEPTH levels deep and MAX_NODES nodes in all. Measured in
  // Chromium, a tab crashes at about 960 levels of \frac output, and
  // layout time grows about with the cube of depth: nested \mathinner
  // takes 2 to 3 s to lay out at 250 levels and 19 s at 491 (forgectl#675).
  // Each \frac level is 7 DOM levels, so 34 nested fractions render and 35
  // do not. MAX_NODES bounds how much output one formula can build, and
  // layout time grows faster than its node count: a macro-free row of
  // x'x'x'… lays out 36,000 nodes in about 0.2 s and 72,000 in about 1.4 s
  // (forgectl#697). 40,000 nodes is far past a hand-written formula; a
  // dense 50-line align is about 25,000.
  var MAX_DOM_DEPTH = 250;
  var MAX_NODES = 40000;

  // The page budget (forgectl#697). Every cap above is per formula, so a
  // page of many formulas each under them still adds up: ten 70,000-node
  // formulas froze the tab for 51 s. One render pass (the first render, or
  // a refresh after live reload) spends at most about PAGE_MS milliseconds
  // of main-thread time on math: KaTeX's build, timed as it runs, plus the
  // layout of what the pass attached, estimated by layoutMs. The formula
  // that would cross it, and every formula after it in the pass, is left as
  // TeX source. The build time is checked between formulas, so a pass can
  // run over by one formula's build, which MAX_NODES keeps short. Layout
  // is estimated rather than timed because timing it means forcing a
  // layout after every formula, which on a page of 400 small inline
  // formulas cost 0.7 to 1 s by itself. That page spends about 0.4 s.
  var PAGE_MS = 3000;

  // layoutMs estimates, in milliseconds, what laying out a formula of n
  // nodes costs: about 3 µs a node, plus a term that grows with the square
  // of n. Measured in Chromium on macro-free rows of x' and empty matrix
  // cells: 36,000 nodes take 0.2 to 0.45 s to lay out and 70,000 take 1.4
  // to 2.2 s. It overestimates, so the pass stops early rather than late.
  function layoutMs(n) {
    return n * 0.003 + n * n * 4e-7;
  }

  // outputNodes returns the number of nodes under root, or -1 when the tree
  // is deeper than MAX_DOM_DEPTH or has more than MAX_NODES nodes, counting
  // root's children as depth 1. It walks with an explicit stack, because a
  // recursive walk of an over-deep tree is the stack overflow this guards
  // against, and it stops at the first node over either bound.
  function outputNodes(root) {
    var stack = [root, 0];
    var nodes = 0;
    while (stack.length > 0) {
      var depth = stack.pop();
      var node = stack.pop();
      for (var c = node.firstChild; c !== null; c = c.nextSibling) {
        nodes++;
        if (depth + 1 > MAX_DOM_DEPTH || nodes > MAX_NODES) { return -1; }
        stack.push(c, depth + 1);
      }
    }
    return nodes;
  }

  // The reasons skip gives, as the skipped formula's tooltip.
  var TOO_COMPLEX = "Not rendered: this formula is too long, too large or too deeply nested to render safely.";
  var DEFINES_MACRO = "Not rendered: this formula defines a macro (\\def, \\newcommand, \\let, …), which could make it too slow to render safely.";
  var RENDER_FAILED = "Not rendered: the math renderer failed on this formula.";
  var OVER_BUDGET = "Not rendered: the math before this formula used up the page's rendering budget.";

  // skip leaves the formula as its TeX source, marked .math-skipped, with
  // reason as its tooltip.
  function skip(el, src, reason) {
    el.textContent = src;
    el.classList.add("math-skipped");
    el.title = reason;
  }

  // newBudget is the allowance of one render pass: the time it started,
  // the layout it has charged so far, and whether it has run out.
  function newBudget() {
    return { start: performance.now(), layout: 0, spent: false };
  }

  // budgetLeft is the milliseconds budget has left, after its elapsed time
  // and the layout it has charged.
  function budgetLeft(budget) {
    return PAGE_MS - (performance.now() - budget.start) - budget.layout;
  }

  // renderOne renders one formula that has not been rendered yet, charging
  // budget. The TeX is stashed in data-math-source, which also marks the
  // formula as done: nothing renders it again, because its source cannot
  // change and a theme change only recolors errors (recolorErrors).
  function renderOne(el, color, budget) {
    var src = el.textContent;
    el.dataset.mathSource = src;
    if (budget.spent || budgetLeft(budget) <= 0) {
      budget.spent = true;
      skip(el, src, OVER_BUDGET);
      return;
    }
    var display = el.classList.contains("math-display");
    if (DEFINER.test(src)) {
      skip(el, src, DEFINES_MACRO);
      return;
    }
    if (tooComplex(src)) {
      skip(el, src, TOO_COMPLEX);
      return;
    }
    // Detached: KaTeX builds into holder, which is in no document, so no
    // layout runs until the output passes outputNodes and moves into el.
    var holder = document.createElement("span");
    try {
      katex.render(strip(src, display), holder, {
        displayMode: display,
        // A parse error renders as the TeX in errorColor, with the message
        // as a tooltip, instead of throwing and leaving later formulas
        // unrendered.
        throwOnError: false,
        // The default, stated so nobody flips it: trust:true would let
        // \href, \url, \includegraphics and \htmlClass/\htmlId/\htmlStyle/
        // \htmlData emit links, images and attributes from document text.
        trust: false,
        // Caps every user-specified size (\rule, \kern, \rule{100000em}…)
        // at 500em, so one command cannot build a page-sized box.
        maxSize: 500,
        // Half of KaTeX's default of 1000. It bounds the number of macro
        // expansions, not the tokens they produce or the work those tokens
        // cost, so it cannot bound render time by itself; DEFINER does
        // that for user macros. KaTeX's own macros (\dots is two
        // expansions, \neq four, \iff eight) count toward it; a formula
        // over it renders as a parse error, source visible.
        maxExpand: 500,
        errorColor: color
      });
    } catch (err) {
      // Only a non-parse failure reaches here (a JavaScript stack overflow
      // in KaTeX's builders on macro-nested input is one). Put the source
      // back so the formula stays readable.
      skip(el, src, RENDER_FAILED);
      console.warn("[forgectl docs] math render failed", err);
      return;
    }
    var nodes = outputNodes(holder);
    if (nodes < 0) {
      skip(el, src, TOO_COMPLEX);
      return;
    }
    var layout = layoutMs(nodes);
    if (layout > budgetLeft(budget)) {
      budget.spent = true;
      skip(el, src, OVER_BUDGET);
      return;
    }
    budget.layout += layout;
    el.textContent = "";
    while (holder.firstChild !== null) { el.appendChild(holder.firstChild); }
  }

  // renderAll renders every formula not rendered yet, in one budgeted pass.
  function renderAll() {
    var color = errorColor();
    var budget = newBudget();
    document.querySelectorAll(".math").forEach(function (el) {
      if (el.dataset.mathSource === undefined) { renderOne(el, color, budget); }
    });
  }

  // recolorErrors gives every parse error the current theme's error color.
  // KaTeX's own output uses currentColor and follows the theme by itself;
  // only an error carries an inline color. Recoloring in place, rather than
  // re-rendering every formula as an earlier version did, makes a theme
  // toggle cost nothing in KaTeX (forgectl#697).
  function recolorErrors() {
    var color = errorColor();
    document.querySelectorAll(".math .katex-error").forEach(function (e) {
      e.style.color = color;
    });
  }

  function watchTheme() {
    var last = document.documentElement.getAttribute("data-theme");
    new MutationObserver(function () {
      var now = document.documentElement.getAttribute("data-theme");
      if (now === last) { return; }
      last = now;
      recolorErrors();
    }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  }

  renderAll();
  watchTheme();

  // Live reload (reload.js) swaps the document body in place, bringing in new,
  // unrendered .math elements; refresh renders them, with a fresh budget.
  window.ForgectlMath = { refresh: renderAll };
})();
