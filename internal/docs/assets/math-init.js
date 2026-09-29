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

  // The TeX is stashed on first render, because katex.render replaces the
  // element's content and a re-render (theme change, refresh) needs the source
  // back.
  function renderOne(el, color) {
    var src = el.dataset.mathSource;
    if (src === undefined) {
      src = el.textContent;
      el.dataset.mathSource = src;
    }
    var display = el.classList.contains("math-display");
    try {
      katex.render(strip(src, display), el, {
        displayMode: display,
        // A parse error renders as the TeX in errorColor, with the message
        // as a tooltip, instead of throwing and leaving later formulas
        // unrendered.
        throwOnError: false,
        // The default, stated so nobody flips it: trust:true would let
        // \href, \url, \includegraphics and \htmlClass/\htmlId/\htmlStyle/
        // \htmlData emit links, images and attributes from document text.
        trust: false,
        errorColor: color
      });
    } catch (err) {
      // Only a non-parse failure reaches here. Put the source back so the
      // formula stays readable.
      el.textContent = src;
      console.warn("[forgectl docs] math render failed", err);
    }
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
