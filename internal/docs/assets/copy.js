// Rich copy for `forgectl docs serve` (forgectl#588, folding in the #583
// copy-tex item).
//
// Left alone, Chromium serializes a copied selection with the reader's
// computed theme styles inlined on every element, so a paste imports the dark
// theme. The DOM itself carries no style attributes to strip; the styles are
// added by the browser's serializer. So this handler serializes a DETACHED
// clone of the selection itself, where nothing is styled, and puts clean
// semantic HTML plus plain text on the clipboard.
//
// It is a `copy` event listener with clipboardData.setData rather than the
// async Clipboard API because that API needs a secure context, and an
// off-loopback bind is not one.
//
// There is exactly ONE copy listener. KaTeX's contrib/copy-tex is not
// vendored for the same reason: a second listener would fight this one over
// preventDefault and setData. Formulas are swapped for their TeX here, from
// the data-math-source math-init.js stores on each .math element.
(function () {
  "use strict";

  var XHTML = "http://www.w3.org/1999/xhtml";
  var MERMAID = "pre.mermaid[data-mermaid-source]";

  function docBody(node) {
    var el = node && node.nodeType === 1 ? node : node && node.parentElement;
    // The shell's doc pane by its data-fc hook: a doc can carry
    // class="doc-body" on its own div (forgectl#643).
    return el ? el.closest('[data-fc="doc-body"]') : null;
  }

  // The source when the selection lies wholly inside one rendered formula or
  // diagram: cloneContents() would then return bare KaTeX or SVG pieces with
  // no .math or pre.mermaid ancestor left to substitute. Returns a node.
  function enclosingSource(range) {
    var n = range.commonAncestorContainer;
    var el = n.nodeType === 1 ? n : n.parentElement;
    if (!el) { return null; }
    var math = el.closest(".math");
    if (math && math.getAttribute("data-math-source") !== null) {
      var span = document.createElement("span");
      span.textContent = math.getAttribute("data-math-source");
      return span;
    }
    var dia = el.closest(MERMAID);
    return dia ? codeBlock(dia.getAttribute("data-mermaid-source")) : null;
  }

  // A rendered diagram copies as its SOURCE, in a <pre><code>. The SVG cannot
  // travel: mermaid scopes its <style> to #mermaid-N and its classes, and its
  // markers use url(#id), so pasted into another page the ids and styles are
  // gone and it draws as black rectangles. The whole frame (bar, body, pre)
  // is replaced, and the source goes in through textContent, never as markup.
  function codeBlock(text) {
    var pre = document.createElement("pre");
    var code = document.createElement("code");
    code.textContent = text;
    pre.appendChild(code);
    return pre;
  }

  function substituteDiagrams(root) {
    root.querySelectorAll(MERMAID).forEach(function (el) {
      var frame = el.closest(".embed") || el;
      frame.replaceWith(codeBlock(el.getAttribute("data-mermaid-source")));
    });
  }

  // Swap each rendered formula for its TeX source, delimiters kept. The tag is
  // kept (a display block stays a block) and everything else on it goes. A
  // .math without a stored source was never rendered, so its text already is
  // the TeX and it is left alone.
  function substituteMath(root) {
    root.querySelectorAll(".math").forEach(function (el) {
      var src = el.getAttribute("data-math-source");
      if (src === null) { return; }
      var plain = document.createElement(el.localName);
      plain.textContent = src;
      el.replaceWith(plain);
    });
  }

  // Reader-only nodes: the diagram frame's reset bar, and the pan/zoom
  // viewport and stage (unwrapped, leaving the diagram itself).
  function dropChrome(root) {
    root.querySelectorAll(".embed-bar").forEach(function (el) { el.remove(); });
    root.querySelectorAll(".dia-viewport, .dia-stage").forEach(function (el) {
      el.replaceWith.apply(el, Array.prototype.slice.call(el.childNodes));
    });
  }

  // Strip what only the reader's own CSS and scripts use, then unwrap the
  // attribute-less <span>s that leaves behind (syntax-highlight tokens).
  // An SVG subtree is skipped: author-written inline SVG depends on its own
  // ids (markers, gradients, url(#id)), classes and style, so those stay. Only
  // the reader's pan/zoom marker comes off it.
  function stripAttributes(root) {
    var drop = ["class", "id", "style", "tabindex", "role", "aria-label", "aria-hidden"];
    root.querySelectorAll("svg").forEach(function (el) { el.removeAttribute("data-panzoom"); });
    root.querySelectorAll("*").forEach(function (el) {
      if (el.namespaceURI !== XHTML) { return; }
      drop.forEach(function (a) { el.removeAttribute(a); });
      Array.prototype.slice.call(el.attributes).forEach(function (attr) {
        if (attr.name.indexOf("data-") === 0) { el.removeAttribute(attr.name); }
      });
    });
    var spans = Array.prototype.slice.call(root.querySelectorAll("span")).filter(function (el) {
      return el.namespaceURI === XHTML && el.attributes.length === 0;
    });
    // Innermost first, so an unwrapped child is still in the tree when its
    // parent goes.
    spans.reverse().forEach(function (el) {
      el.replaceWith.apply(el, Array.prototype.slice.call(el.childNodes));
    });
  }

  // Plain text comes from the clone too, so it carries TeX rather than the
  // doubled MathML+HTML text Selection.toString() yields for a formula. The
  // clone is detached, and innerText needs layout, so it is measured in an
  // off-screen holder for the length of the call.
  function plainText(clone) {
    var holder = document.createElement("div");
    holder.style.cssText = "position:fixed;left:-99999px;top:0;width:800px;white-space:normal";
    holder.appendChild(clone.cloneNode(true));
    document.body.appendChild(holder);
    try {
      return holder.innerText;
    } finally {
      holder.remove();
    }
  }

  function build(ranges) {
    var clone = document.createElement("div");
    ranges.forEach(function (r) {
      var part = ranges.length > 1 ? document.createElement("div") : clone;
      part.appendChild(enclosingSource(r) || r.cloneContents());
      if (part !== clone) { clone.appendChild(part); }
    });
    substituteDiagrams(clone);
    substituteMath(clone);
    dropChrome(clone);
    stripAttributes(clone);
    return { html: clone.innerHTML, text: plainText(clone) };
  }

  function onCopy(e) {
    var sel = window.getSelection();
    if (!e.clipboardData || !sel || sel.isCollapsed || sel.rangeCount === 0) { return; }

    // Only selections wholly inside the document body. Anything else (the
    // sidenav, the filter box) copies natively.
    var ranges = [];
    for (var i = 0; i < sel.rangeCount; i++) {
      var r = sel.getRangeAt(i);
      if (!docBody(r.startContainer) || !docBody(r.endContainer)) { return; }
      ranges.push(r);
    }

    // Both payloads are built before either is set, and preventDefault is the
    // last call. A throw anywhere returns with the event untouched, so the
    // browser's native copy runs: a bug here costs the clean HTML, never the
    // copy itself.
    try {
      var out = build(ranges);
      e.clipboardData.setData("text/html", out.html);
      e.clipboardData.setData("text/plain", out.text);
      e.preventDefault();
    } catch (err) {
      console.warn("[forgectl docs] rich copy failed; falling back to native copy", err);
    }
  }

  document.addEventListener("copy", onCopy);
})();
