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
  var MAX_SOURCE = 10000;
  var MAX_DEPTH = 100;

  // tooComplex reports whether src is over MAX_SOURCE characters or nests
  // deeper than MAX_DEPTH. An escaped brace (\{, \}) is a literal, a
  // control symbol (\\, \$, …) consumes the one character after it, and a
  // % comment runs to the end of its line, as KaTeX reads them. A closer
  // never takes the depth below zero, so surplus closers cannot bank credit
  // for later openers.
  function tooComplex(src) {
    if (src.length > MAX_SOURCE) { return true; }
    var depth = 0;
    for (var i = 0; i < src.length; i++) {
      var c = src.charAt(i);
      if (c === "{") {
        depth++;
      } else if (c === "}") {
        depth = Math.max(0, depth - 1);
      } else if (c === "%") {
        while (i + 1 < src.length && src.charAt(i + 1) !== "\n") { i++; }
      } else if (c === "\\") {
        var j = i + 1;
        while (j < src.length && /[A-Za-z]/.test(src.charAt(j))) { j++; }
        var name = src.slice(i + 1, j);
        if (name === "begin" || name === "left" || name === "begingroup" || name === "bgroup") {
          depth++;
        } else if (name === "end" || name === "right" || name === "endgroup" || name === "egroup") {
          depth = Math.max(0, depth - 1);
        }
        // A command name ends before j; a control symbol is one character.
        i = name === "" ? i + 1 : j - 1;
      }
      if (depth > MAX_DEPTH) { return true; }
    }
    return false;
  }

  // The post-render bound. The source scan above cannot see macro
  // expansion: \def\a#1{\frac1{#1}} and a few macros that each apply the
  // one before four times nest 256 fractions from a formula whose braces
  // never go 10 deep (forgectl#596). So KaTeX renders into a detached
  // element, where nothing is laid out, and the output is attached only if
  // its DOM is at most MAX_DOM_DEPTH levels deep and MAX_NODES nodes in
  // all. Measured in Chromium, a tab crashes at about 960 levels of
  // \frac output and about 990 of nested subscripts; 500 leaves about half
  // of that as margin for constructs whose layout costs more per level.
  // Each \frac level is 7 DOM levels, so 70 nested fractions render and 71
  // do not. MAX_NODES bounds how much a formula's macros can multiply its
  // output. The largest macro-free output seen from a source under
  // MAX_SOURCE is about 60,000 nodes (a+a+…+a, 10,000 characters).
  var MAX_DOM_DEPTH = 500;
  var MAX_NODES = 100000;

  // tooBig reports whether the tree under root is deeper than MAX_DOM_DEPTH
  // or has more than MAX_NODES nodes, counting root's children as depth 1.
  // It walks with an explicit stack, because a recursive walk of an
  // over-deep tree is the stack overflow this guards against, and it stops
  // at the first node over either bound.
  function tooBig(root) {
    var stack = [root, 0];
    var nodes = 0;
    while (stack.length > 0) {
      var depth = stack.pop();
      var node = stack.pop();
      for (var c = node.firstChild; c !== null; c = c.nextSibling) {
        nodes++;
        if (depth + 1 > MAX_DOM_DEPTH || nodes > MAX_NODES) { return true; }
        stack.push(c, depth + 1);
      }
    }
    return false;
  }

  // skip leaves the formula as its TeX source, marked .math-skipped.
  function skip(el, src) {
    el.textContent = src;
    el.classList.add("math-skipped");
    el.title = "Not rendered: this formula is too long or too deeply nested to render safely.";
  }

  // The TeX is stashed on first render, because a render replaces the
  // element's content and a re-render (theme change, refresh) needs the
  // source back. A formula already skipped stays skipped: its source has
  // not changed, and re-rendering it would only redo the work to reject it.
  function renderOne(el, color) {
    var src = el.dataset.mathSource;
    if (src === undefined) {
      src = el.textContent;
      el.dataset.mathSource = src;
    }
    if (el.classList.contains("math-skipped")) { return; }
    var display = el.classList.contains("math-display");
    if (tooComplex(src)) {
      skip(el, src);
      return;
    }
    // Detached: KaTeX builds into holder, which is in no document, so no
    // layout runs until the output passes tooBig and moves into el.
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
        // Half of KaTeX's default of 1000 macro expansions, to bound the
        // work a formula's macros make before tooBig sees the output.
        // KaTeX's own macros (\dots, \neq, \iff, …) count toward it too;
        // a formula over it renders as a parse error, source visible.
        maxExpand: 500,
        errorColor: color
      });
    } catch (err) {
      // Only a non-parse failure reaches here (a JavaScript stack overflow
      // in KaTeX's builders on macro-nested input is one). Put the source
      // back so the formula stays readable.
      skip(el, src);
      console.warn("[forgectl docs] math render failed", err);
      return;
    }
    if (tooBig(holder)) {
      skip(el, src);
      return;
    }
    el.textContent = "";
    while (holder.firstChild !== null) { el.appendChild(holder.firstChild); }
  }

  function renderAll() {
    var color = errorColor();
    document.querySelectorAll(".math").forEach(function (el) { renderOne(el, color); });
  }

  // Re-render on theme change so an error's inline color follows the theme.
  // KaTeX's own output uses currentColor and needs nothing.
  function watchTheme() {
    var last = document.documentElement.getAttribute("data-theme");
    new MutationObserver(function () {
      var now = document.documentElement.getAttribute("data-theme");
      if (now === last) { return; }
      last = now;
      renderAll();
    }).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  }

  renderAll();
  watchTheme();

  // Live reload (reload.js) swaps the document body in place, bringing in new,
  // unrendered .math elements; refresh renders them.
  window.ForgectlMath = { refresh: renderAll };
})();
